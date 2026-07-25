package db

import (
	"context"
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

// MarkQueued sets the QUEUED lifecycle and its current scheduler decision together.
func (s *ExperimentsStore) MarkQueued(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("experiments_store.MarkQueued: reason is required")
	}
	const q = `UPDATE experiments SET status = 'QUEUED', queued_at = COALESCE(queued_at, NOW()),
	cluster_name = '', submitted_at = NULL, not_admitted_reason = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// ClaimSubmitted serializes admission decisions for one cluster, checks fresh capacity while
// holding that lock, and conditionally persists the desired-state claim.
func (s *ExperimentsStore) ClaimSubmitted(ctx context.Context, id, clusterName string, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
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
	const q = `UPDATE experiments
SET status = 'SUBMITTED', submitted_at = NOW(), cluster_name = $2, not_admitted_reason = NULL, updated_at = NOW()
WHERE id = $1 AND status = 'QUEUED'`
	tag, err := tx.Exec(ctx, q, id, clusterName)
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
// (age_score) and overwriting estimated_duration_hours plus every resource-dimension estimate
// with the caller-computed, proportionally rescaled remaining amounts — all four dimensions move
// together so no downstream reader mixes an old estimate with a new one. Loop.preempt computes
// the rescale ratio once from the pre-mutation experiment.
func (s *ExperimentsStore) RequeuePreempted(ctx context.Context, id string, remainingHours, newCostAccH, newCPUCoreHours, newRAMGBHours, newStorageGBHours float64) error {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		eviction_reason = 'preempted_for_guaranteed',
		estimated_duration_hours = $2,
		estimated_cost_acch = $3,
		estimated_cpu_core_hours = $4,
		estimated_ram_gb_hours = $5,
		estimated_storage_gb_hours = $6,
		submitted_at = NULL,
		-- clear cluster_name: job holds no capacity, so next admission tick can place it anywhere.
		cluster_name = '',
		not_admitted_reason = 'capacity_unavailable',
		updated_at = NOW()
	WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, remainingHours, newCostAccH, newCPUCoreHours, newRAMGBHours, newStorageGBHours)
	return err
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
