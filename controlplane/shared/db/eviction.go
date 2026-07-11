package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// ResourceRefund is one resource dimension's final observed cost for a terminated experiment —
// not an amount to subtract from a running total. TransitionAndRefund writes it as an absolute
// value against that experiment's own usage series, so applying it twice is idempotent.
type ResourceRefund struct {
	ExperimentID string
	ResourceType domain.ResourceType
	Tier         domain.CapacityTier
	Amount       float64
}

// TransitionAndRefund atomically transitions an experiment from `from` to `to` (EVICTED or
// REJECTED — every early-termination path) and records the reason in a single Postgres
// transaction, then writes each experiment's final observed cost per resource dimension to the
// metrics-DB usage tracker. The status transition is the durable, transactional part — it alone
// decides whether the write happens at all (see the RowsAffected guard below), so a concurrent
// status change (e.g. natural completion racing with eviction) can never double-write.
//
// The usage write itself can no longer share Postgres's transaction (used_* lives in the
// metrics DB now, which has no cross-store transactions with Postgres) — a crash between the
// status commit and the usage write can in principle leave a terminal experiment with a stale
// reservation. This is the accepted tradeoff of a single source of truth for usage with no
// dual-write; see UsageTracker's doc comment. Unlike the old delta-based refund, though, this
// write is an idempotent absolute set (SetObserved), so retrying it after a crash is always safe.
//
// Returns (false, nil) if the row was no longer in `from` status (a concurrent transition
// already handled it); refunds are skipped in that case, same semantics as before.
func (s *Store) TransitionAndRefund(ctx context.Context, id string, from, to domain.ExperimentStatus, reason, agentID, platformExpID string, refunds []ResourceRefund) (updated bool, err error) {
	tag, err := s.ExperimentsStore.pool.pool.Exec(ctx, `
		UPDATE experiments SET status = $3, eviction_reason = $4, updated_at = NOW()
		WHERE id = $1 AND status = $2`, id, string(from), string(to), reason)
	if err != nil {
		return false, fmt.Errorf("db.TransitionAndRefund: transition: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if platformExpID != "" {
		var refundErrs []error
		for _, r := range refunds {
			// Amount == 0 is meaningful (job never ran) and must still be written, not skipped —
			// skipping it would leave the job's reservation stuck at its estimate forever.
			if err := s.usage.SetObserved(ctx, agentID, platformExpID, r.ExperimentID, r.ResourceType, r.Tier, r.Amount); err != nil {
				refundErrs = append(refundErrs, fmt.Errorf("refund %s: %w", r.ResourceType, err))
			}
		}
		if len(refundErrs) > 0 {
			return true, fmt.Errorf("db.TransitionAndRefund: transitioned but %d refund(s) failed: %v", len(refundErrs), refundErrs)
		}
	}

	return true, nil
}
