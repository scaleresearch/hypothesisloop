package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// TransitionStatus does a CAS update: sets status only if it currently matches `from`.
// Returns whether the update happened. Used by CancelExperiment to prevent TOCTOU double-refund races.
func (s *ExperimentsStore) TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error) {
	if to == domain.StatusQueued {
		return false, fmt.Errorf("experiments_store.TransitionStatus: QUEUED requires MarkQueued with a reason")
	}
	const q = `UPDATE experiments SET status = $3, not_admitted_reason = NULL, updated_at = NOW() WHERE id = $1 AND status = $2`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(from), string(to))
	if err != nil {
		return false, fmt.Errorf("experiments_store.TransitionStatus: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// TransitionStatusFromNonTerminal moves id to `to` unless already terminal.
// For callers that can't name the exact prior status (e.g. a watcher unsure if it ever observed
// RUNNING) without risking overwriting a concurrent terminal write.
func (s *ExperimentsStore) TransitionStatusFromNonTerminal(ctx context.Context, id string, to domain.ExperimentStatus) (bool, error) {
	if to == domain.StatusQueued {
		return false, fmt.Errorf("experiments_store.TransitionStatusFromNonTerminal: QUEUED requires MarkQueued with a reason")
	}
	const q = `UPDATE experiments SET status = $2, not_admitted_reason = NULL, updated_at = NOW()
WHERE id = $1 AND status NOT IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED')`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(to))
	if err != nil {
		return false, fmt.Errorf("experiments_store.TransitionStatusFromNonTerminal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListExperimentsWithStatus returns all experiments in the given status.
func (s *ExperimentsStore) ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return s.ListExperiments(ctx, domain.ExperimentFilter{Status: status})
}

// UpdateExperimentPriority updates only the priority_score field.
func (s *ExperimentsStore) UpdateExperimentPriority(ctx context.Context, id string, score float64) error {
	const q = `UPDATE experiments SET priority_score = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, score)
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperimentPriority: %w", err)
	}
	return nil
}

// MarkQueued sets the QUEUED lifecycle and its current scheduler decision together. Also resets
// resolved_job_spec to NULL: a requeued job is re-admitted from scratch and may land on a
// different cluster with a different fair share, so any previous "max" resolution is stale and
// must not be read as though it were the resolution for wherever it lands next.
func (s *ExperimentsStore) MarkQueued(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("experiments_store.MarkQueued: reason is required")
	}
	const q = `UPDATE experiments SET status = 'QUEUED', queued_at = COALESCE(queued_at, NOW()),
	cluster_name = '', submitted_at = NULL, not_admitted_reason = $2, resolved_job_spec = NULL, updated_at = NOW()
	WHERE id = $1 AND status IN ('SUBMITTED', 'ADMITTED')`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// ClaimSubmitted serializes admission decisions for one cluster, checks fresh capacity while
// holding that lock, and conditionally persists the desired-state claim. resolvedJob is the
// literal resolution of any "max" sentinel in the job (nil if there was nothing to resolve),
// written into resolved_job_spec in the SAME transaction as the SUBMITTED status transition —
// see domain.Experiment.ResolvedJob's doc comment.
func (s *ExperimentsStore) ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "cluster-admission/"+clusterName); err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: lock: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT`+experimentColumns+`FROM experiments
WHERE cluster_name = $1 AND status IN ('SUBMITTED', 'ADMITTED')
ORDER BY submitted_at ASC, id ASC`, clusterName)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
	}
	desired, err := collectExperiments(rows)
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
	}
	available, err := capacityAvailable(ctx, desired)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: capacity: %w", err)
	}
	if !available {
		return false, nil
	}
	var resolvedJobSpec []byte
	if resolvedJob != nil {
		var err error
		resolvedJobSpec, err = json.Marshal(resolvedJob)
		if err != nil {
			return false, fmt.Errorf("experiments_store.ClaimSubmitted: marshal resolved job spec: %w", err)
		}
	}
	const q = `UPDATE experiments
SET status = 'SUBMITTED', submitted_at = NOW(), cluster_name = $2, not_admitted_reason = NULL, resolved_job_spec = $3, updated_at = NOW()
WHERE id = $1 AND status = 'QUEUED'`
	tag, err := tx.Exec(ctx, q, id, clusterName, resolvedJobSpec)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: commit: %w", err)
	}
	return true, nil
}

// MarkStarted sets status=RUNNING, but only if still SUBMITTED/ADMITTED. Guards against a
// job_watcher poll observing a stale "Running" phase after the experiment was already
// cancelled/evicted (the backend Job can report Running briefly until cluster-agent reconciles
// it away). Returns whether the transition happened, so callers can skip onRunning side effects
// (accelerator-type recording, flavor-substitution debit) when it didn't.
func (s *ExperimentsStore) MarkStarted(ctx context.Context, id string) (bool, error) {
	const q = `UPDATE experiments SET status = 'RUNNING', updated_at = NOW()
WHERE id = $1 AND status IN ('SUBMITTED', 'ADMITTED')`
	tag, err := s.pool.pool.Exec(ctx, q, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateEvictionReason sets the eviction_reason field without changing status.
func (s *ExperimentsStore) UpdateEvictionReason(ctx context.Context, id, reason string) error {
	const q = `UPDATE experiments SET eviction_reason = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// UpdateNotAdmittedReason replaces the explanation only while the job is still QUEUED.
// ClaimSubmitted clears it in the same statement that changes the lifecycle, so a stale
// scheduler decision can never attach a queue reason to an admitted job.
func (s *ExperimentsStore) UpdateNotAdmittedReason(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("experiments_store.UpdateNotAdmittedReason: reason is required")
	}
	const q = `UPDATE experiments SET not_admitted_reason = $2, updated_at = NOW()
	WHERE id = $1 AND status = 'QUEUED'`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// RequeuePreempted returns a job to QUEUED after preemption, preserving original queued_at
// (age_score) and overwriting estimated_duration_hours plus the accelerator-hours estimate with
// the caller-computed, proportionally rescaled remaining amounts — both move together so no
// downstream reader mixes an old estimate with a new one. Loop.preempt computes the rescale ratio
// once from the pre-mutation experiment.
//
// Returns requeued=false when the job was no longer RUNNING. The status guard is not optional:
// preempt reads its candidates, then spends metrics queries ranking them, and a job can complete,
// be cancelled, be stage-cut or be evicted inside that window. Without the guard this UPDATE
// would drag a terminal job back to QUEUED and re-run work that had already finished — or that a
// human had explicitly cancelled.
func (s *ExperimentsStore) RequeuePreempted(ctx context.Context, id string, remainingHours, newCostAccH float64) (bool, error) {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		eviction_reason = $4,
		estimated_duration_hours = $2,
		estimated_cost_acch = $3,
		submitted_at = NULL,
		-- clear cluster_name: job holds no capacity, so next admission tick can place it anywhere.
		cluster_name = '',
		not_admitted_reason = 'capacity_unavailable',
		updated_at = NOW()
	WHERE id = $1 AND status = 'RUNNING'`
	tag, err := s.pool.pool.Exec(ctx, q, id, remainingHours, newCostAccH,
		string(domain.EvictionPreemptedForGuaranteed))
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeuePreempted: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RequeueForRetry returns a failed gang to QUEUED for a fresh whole-gang attempt, incrementing
// attempt_count in the same statement so the budget is spent exactly once even if two watchers
// observe the same failure.
//
// maxAttemptsBefore is the number of the agent's OWN failed attempts after which the job stays
// FAILED — i.e. job.max_retries. Attempts the environment ended are excluded by comparing
// attempt_count - infra_requeue_count (domain.Experiment.RetriesUsed), so a job repeatedly
// killed by broken hardware still gets its full allowance for its own bugs. The comparison is in
// the WHERE clause rather than in the caller so the read
// and the write cannot disagree under concurrency; requeued=false therefore means either "budget
// exhausted" or "another writer got there first", and both mean the same thing to the caller: do
// nothing more.
//
// Unlike RequeuePreempted this does NOT rescale the estimate. A preempted job is assumed to
// resume where it stopped, so only its remaining hours are re-reserved; a retried gang starts
// from step zero, so the full original estimate is exactly what the next attempt needs.
//
// quota_settled_at is cleared because the row is no longer terminal and the next attempt owes a
// settlement of its own. Nothing is lost by that: settlement.Settle recomputes cumulative
// observed hours over the whole experiment and writes an absolute value, so the final settle
// bills every attempt.
func (s *ExperimentsStore) RequeueForRetry(ctx context.Context, id string, maxAttemptsBefore int) (bool, error) {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		attempt_count = attempt_count + 1,
		submitted_at = NULL,
		-- clear cluster_name: the failed attempt holds no capacity, so the next admission tick
		-- can place this anywhere, exactly as a preemption requeue does.
		cluster_name = '',
		not_admitted_reason = 'capacity_unavailable',
		quota_settled_at = NULL,
		updated_at = NOW()
	WHERE id = $1 AND status = 'FAILED' AND attempt_count - infra_requeue_count < $2`
	tag, err := s.pool.pool.Exec(ctx, q, id, maxAttemptsBefore)
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeueForRetry: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RequeueInfrastructureFault returns an experiment the environment failed to QUEUED for a fresh
// attempt that costs it nothing, spending one of its infrastructure-requeue allowance in the same
// guarded UPDATE that reads it — so two watchers observing the same fault cannot both spend it,
// and a read can never disagree with the write it authorised.
//
// attempt_count advances even though this is not one of the agent's failures, and that is the
// point of having two counters rather than one. attempt_count is the generation number the
// runtimes put in the workload's environment (HYPOTHESISLOOP_ATTEMPT): a requeue reuses the
// experiment id, so without it the rebuilt workload is byte-identical to the terminal one still
// sitting in the cluster, the reconciler reads that dead workload as matching desired state, and
// the job never actually runs again. infra_requeue_count records how much of that advance was
// not the agent's doing, and max_retries is compared against the difference — so the retry budget
// is untouched while desired state still genuinely changes.
//
// The eviction reason is kept rather than cleared: it is the durable record of what ended the
// last attempt, and the only thing that lets a stats reader say "these were infrastructure".
// not_admitted_reason is required for any QUEUED row (experiments_queue_reason_consistent).
//
// quota_settled_at is cleared for the same reason RequeueForRetry clears it: the row is no longer
// terminal and the next attempt owes a settlement of its own.
//
// Returns false when the ceiling is reached or another writer got there first — both mean the
// caller must terminate instead.
func (s *ExperimentsStore) RequeueInfrastructureFault(ctx context.Context, id string, from domain.ExperimentStatus, reason string, maxInfraRequeues int) (bool, error) {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		eviction_reason = $3,
		attempt_count = attempt_count + 1,
		infra_requeue_count = infra_requeue_count + 1,
		submitted_at = NULL,
		-- clear cluster_name: the failed attempt holds no capacity, and the next admission tick
		-- must be free to place this somewhere other than the cluster that just failed it.
		cluster_name = '',
		queued_at = COALESCE(queued_at, NOW()),
		not_admitted_reason = 'capacity_unavailable',
		quota_settled_at = NULL,
		updated_at = NOW()
	WHERE id = $1 AND status = $2 AND infra_requeue_count < $4`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(from), reason, maxInfraRequeues)
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeueInfrastructureFault: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkQuotaSettled records that exp's final usage was durably written to the metrics DB, so the
// settlement reconciler stops retrying it. Only called after that write succeeds.
func (s *ExperimentsStore) MarkQuotaSettled(ctx context.Context, id string) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var agentID, platformExpID string
	if err := tx.QueryRow(ctx, `SELECT agent_id, platform_experiment_id FROM experiments WHERE id=$1`, id).Scan(&agentID, &platformExpID); err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: identify owner: %w", err)
	}
	key := agentID + "/" + platformExpID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: acquire lock: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE experiments SET quota_settled_at = NOW() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: update: %w", err)
	}
	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: commit: %w", err)
	}
	return nil
}
