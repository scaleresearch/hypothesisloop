package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// TransitionStatus atomically updates status only when the current status matches
// expectedStatus. Returns true if the row was updated, false if the status had
// already changed (e.g. a concurrent request beat this one). Used by CancelExperiment
// to prevent TOCTOU double-refund races.
func (s *ExperimentsStore) TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error) {
	const q = `UPDATE experiments SET status = $3, updated_at = NOW() WHERE id = $1 AND status = $2`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(from), string(to))
	if err != nil {
		return false, fmt.Errorf("experiments_store.TransitionStatus: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// TransitionStatusFromNonTerminal moves id to `to` unless it's already in a terminal state
// (COMPLETED/FAILED/EVICTED). Unlike TransitionStatus's single-`from` CAS, this covers callers
// that can't name the exact prior status (e.g. a watcher that lost track of whether it ever
// observed RUNNING across a pod restart) without risking a stomp of a concurrent terminal
// write — such a caller either overspecifies `from` and wrongly no-ops on a legitimate
// completion, or (worse) unconditionally overwrites and resurrects an already-terminal job.
func (s *ExperimentsStore) TransitionStatusFromNonTerminal(ctx context.Context, id string, to domain.ExperimentStatus) (bool, error) {
	const q = `UPDATE experiments SET status = $2, updated_at = NOW()
WHERE id = $1 AND status NOT IN ('COMPLETED', 'FAILED', 'EVICTED')`
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

// MarkQueued sets status=QUEUED and queued_at=now (first time only — preserved on requeue).
func (s *ExperimentsStore) MarkQueued(ctx context.Context, id string) error {
	const q = `UPDATE experiments SET status = 'QUEUED', queued_at = COALESCE(queued_at, NOW()), updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id)
	return err
}

// MarkSubmitted sets status=SUBMITTED, submitted_at=now, and cluster_name=clusterName, all in
// one statement — the cluster assignment and the SUBMITTED transition that claims its capacity
// must land atomically, so no other tick can observe one without the other.
func (s *ExperimentsStore) MarkSubmitted(ctx context.Context, id, clusterName string) error {
	const q = `UPDATE experiments SET status = 'SUBMITTED', submitted_at = NOW(), cluster_name = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, clusterName)
	return err
}

// MarkStarted sets status=RUNNING and started_at=now (idempotent: won't overwrite existing
// started_at). Conditional on the experiment still being SUBMITTED/ADMITTED — without this
// guard, a job_watcher poll that observes the backend's first "Running" phase after the
// experiment was already cancelled/evicted (a real race: the watcher isn't told to stop, and
// the backend Job can still report Running for a few seconds until the cluster-agent
// reconciles it away) would unconditionally stomp the terminal EVICTED status back to
// RUNNING. Returns whether the transition actually happened, so callers can skip every other
// onRunning side effect (accelerator-type recording, flavor-substitution debit) when it didn't.
func (s *ExperimentsStore) MarkStarted(ctx context.Context, id string) (bool, error) {
	const q = `UPDATE experiments SET status = 'RUNNING', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
WHERE id = $1 AND status IN ('SUBMITTED', 'ADMITTED')`
	tag, err := s.pool.pool.Exec(ctx, q, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateAdmittedFlavor records the GPU flavor the job actually ran on (which may
// differ from the requested type within the Hopper H100/H200 group) and the recomputed
// estimated cost at that flavor's rate. All downstream accounting — refunds, quota-exhaustion,
// and phase-2 consumption — reads gpu_type/estimated_cost_t4h, so persisting here keeps the
// charged rate consistent across the whole job lifecycle.
func (s *ExperimentsStore) UpdateAdmittedFlavor(ctx context.Context, id string, gpuType domain.GPUType, estimatedCostT4H float64) error {
	const q = `UPDATE experiments SET gpu_type = $2, estimated_cost_t4h = $3, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, string(gpuType), estimatedCostT4H)
	return err
}

// UpdateEvictionReason sets the eviction_reason field without changing status.
func (s *ExperimentsStore) UpdateEvictionReason(ctx context.Context, id, reason string) error {
	const q = `UPDATE experiments SET eviction_reason = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// UpdateNotAdmittedReason sets why a QUEUED job wasn't admitted on its most recent skipped
// tick. Pass "" to clear it (e.g. on admission). Does not touch updated_at — this is set every
// tick a job is skipped and shouldn't perturb age-based ordering (sortGuaranteed/sortBurst key
// off queued_at, not updated_at, so this is purely defensive, not currently load-bearing).
func (s *ExperimentsStore) UpdateNotAdmittedReason(ctx context.Context, id, reason string) error {
	var arg any
	if reason != "" {
		arg = reason
	}
	const q = `UPDATE experiments SET not_admitted_reason = $2 WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, arg)
	return err
}

// RequeuePreempted returns a job to QUEUED after preemption. Preserves original queued_at
// (age_score), increments preempt_count, and overwrites estimated_duration_hours plus every
// resource-dimension estimate (GPU cost, CPU/RAM/storage) with the caller-computed, proportionally
// rescaled remaining amounts — all four dimensions must move together so no downstream reader
// (reconcile, settlement, quota-exhaustion) ever mixes an old estimate for one dimension with a
// new one for another. The caller (Loop.preempt) computes the rescale ratio once from the
// pre-mutation experiment and uses the same numbers to correct the metrics-DB reservation, so the
// Postgres estimate and the metrics-DB reservation never disagree.
func (s *ExperimentsStore) RequeuePreempted(ctx context.Context, id string, remainingHours, newCostT4H, newCPUCoreHours, newRAMGBHours, newStorageGBHours float64) error {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		preempt_count = preempt_count + 1,
		eviction_reason = 'preempted_for_guaranteed',
		estimated_duration_hours = $2,
		estimated_cost_t4h = $3,
		estimated_cpu_core_hours = $4,
		estimated_ram_gb_hours = $5,
		estimated_storage_gb_hours = $6,
		submitted_at = NULL,
		started_at = NULL,
		-- clear cluster_name: the job no longer holds capacity anywhere, so the next admission
		-- tick is free to place it on whichever cluster actually has room then, not stuck
		-- retrying the cluster it was evicted from.
		cluster_name = '',
		updated_at = NOW()
	WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, remainingHours, newCostT4H, newCPUCoreHours, newRAMGBHours, newStorageGBHours)
	return err
}

// MarkQuotaSettled records that exp's final observed usage has been durably written to the
// metrics DB, so the settlement reconciler stops retrying it. Only ever called after that write
// actually succeeds.
func (s *ExperimentsStore) MarkQuotaSettled(ctx context.Context, id string) error {
	const q = `UPDATE experiments SET quota_settled_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("experiments_store.MarkQuotaSettled: %w", err)
	}
	return nil
}
