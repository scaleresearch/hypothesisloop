package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ResolveTermination resolves one requested terminal transition. It moves the experiment from
// `from` to `to` (EVICTED or REJECTED) and records the reason in a single guarded UPDATE — the
// RowsAffected guard means a concurrent status change (e.g. natural completion racing with
// eviction) can never double-transition.
//
// Except when the reason names an infrastructure fault that is requeued (domain.RequeuedOnFault). Then the job did nothing wrong: the
// environment failed it, so while the experiment still has infrastructure-requeue budget the row
// goes back to QUEUED instead, and the caller is told so. That decision lives here, in the same
// statement that spends the budget, rather than at each of the two eviction paths that can raise
// such a reason: a read of the counter followed by a separate write could disagree under two
// watchers observing the same fault, and duplicating the branch would let the two paths drift.
//
// It deliberately does not write final usage to the metrics DB — that's a separate store with no
// shared transaction, so callers settle usage afterward via service/settlement.Settler, whose
// idempotent absolute-set writes can retry until quota_settled_at is set. Settle is what turns an
// infrastructure fault into a refund; see its doc comment.
func (s *Store) ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (domain.Termination, error) {
	if domain.RequeuedOnFault(domain.EvictionReason(reason)) {
		requeued, err := s.RequeueInfrastructureFault(ctx, id, from, reason, s.maxInfraRequeues)
		if err != nil {
			return domain.TerminationSkipped, err
		}
		if requeued {
			return domain.TerminationRequeued, nil
		}
		// Ceiling reached (or another writer won the race): fall through and terminate, so a job
		// that keeps landing on broken hardware stops and says so with the reason that ended it.
	}
	moved, err := s.writeJob(ctx, (*Write).jobStatus, `
		UPDATE experiments SET status = $3, eviction_reason = $4, not_admitted_reason = NULL, updated_at = NOW(), ended_at = NOW()
		WHERE id = $1 AND status = $2`, id, string(from), string(to), reason)
	if err != nil {
		return domain.TerminationSkipped, fmt.Errorf("db.ResolveTermination: %w", err)
	}
	if !moved {
		return domain.TerminationSkipped, nil
	}
	return domain.TerminationWritten, nil
}

// EndQueuedSettled ends the queued experiment id as to with reason and marks its quota settled in
// the same statement, so whoever the status change notifies reads the hold already released. The
// caller writes its final usage first (settlement.Settler.Settle). It says whether the row was
// still queued.
func (s *Store) EndQueuedSettled(ctx context.Context, id string, to domain.ExperimentStatus, reason string) (bool, error) {
	moved, err := s.writeJob(ctx, (*Write).jobStatus, `
		UPDATE experiments SET status = $2, eviction_reason = $3, not_admitted_reason = NULL, quota_settled_at = NOW(), updated_at = NOW(), ended_at = NOW()
		WHERE id = $1 AND status = 'QUEUED'`, id, string(to), reason)
	if err != nil {
		return false, fmt.Errorf("db.EndQueuedSettled: %w", err)
	}
	return moved, nil
}
