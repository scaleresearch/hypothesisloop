package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ResolveTermination resolves one requested terminal transition. It moves the experiment from
// `from` to `to` (EVICTED or REJECTED) and records the reason in a single guarded UPDATE — the
// RowsAffected guard means a concurrent status change (e.g. natural completion racing with
// eviction) can never double-transition.
//
// Except when the reason names an infrastructure fault. Then the job did nothing wrong: the
// environment failed it, so while the experiment still has infrastructure-requeue budget the row
// goes back to QUEUED instead, and the caller is told so. That decision lives here, in the same
// statement that spends the budget, rather than at each of the two eviction paths that can raise
// such a reason: a read of the counter followed by a separate write could disagree under two
// watchers observing the same fault, and duplicating the branch would let the two paths drift.
//
// It deliberately does not write final usage to the metrics DB — that's a separate store with no
// shared transaction, so callers settle usage afterward via services/settlement.Settler, whose
// idempotent absolute-set writes can retry until quota_settled_at is set. Settle is what turns an
// infrastructure fault into a refund; see its doc comment.
// triedClusterID, when non-empty, is the cluster (runtime-derived cluster_id) this attempt just
// gave up on — appended to the job's tried_clusters instead of spending infra_requeue_count
// budget. Pass "" for every eviction path unrelated to speculative scale-up failover; existing
// callers are unaffected.
func (s *Store) ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string, triedClusterID string) (domain.Termination, error) {
	if domain.IsInfrastructureFault(domain.EvictionReason(reason)) {
		requeued, err := s.ExperimentsStore.RequeueInfrastructureFault(ctx, id, from, reason, s.maxInfraRequeues, triedClusterID)
		if err != nil {
			return domain.TerminationSkipped, err
		}
		if requeued {
			return domain.TerminationRequeued, nil
		}
		// Ceiling reached (or another writer won the race): fall through and terminate, so a job
		// that keeps landing on broken hardware stops and says so with the reason that ended it.
	}
	tag, err := s.ExperimentsStore.pool.pool.Exec(ctx, `
		UPDATE experiments SET status = $3, eviction_reason = $4, not_admitted_reason = NULL, updated_at = NOW()
		WHERE id = $1 AND status = $2`, id, string(from), string(to), reason)
	if err != nil {
		return domain.TerminationSkipped, fmt.Errorf("db.ResolveTermination: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.TerminationSkipped, nil
	}
	return domain.TerminationWritten, nil
}
