package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// TransitionStatus ends a job that ran: it sets `to` (COMPLETED or FAILED) only if the status is
// still `from`, and returns whether it did, so a completion racing an eviction lands once. reason
// is what the status is stored and announced with: why it failed, or nothing.
func (s *ExperimentsStore) TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error) {
	if to == domain.StatusQueued {
		return false, fmt.Errorf("experiments_store.TransitionStatus: QUEUED requires MarkQueued with a reason")
	}
	const q = `UPDATE experiments SET status = $3, eviction_reason = NULLIF($4, ''), not_admitted_reason = NULL, updated_at = NOW() WHERE id = $1 AND status = $2`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(from), string(to), reason)
	if err != nil {
		return false, fmt.Errorf("experiments_store.TransitionStatus: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListExperimentsWithStatus returns all experiments in the given status.
func (s *ExperimentsStore) ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return s.ListExperiments(ctx, domain.ExperimentFilter{Scope: domain.AllTenants, Status: status})
}

// UpdateExperimentPriority writes a changed priority_score. updated_at stays: it is the time of
// the row's last status transition, which ListRecentlyUndesiredWorkloads and finished_at read.
func (s *ExperimentsStore) UpdateExperimentPriority(ctx context.Context, id string, score float64) error {
	const q = `UPDATE experiments SET priority_score = $2 WHERE id = $1 AND priority_score IS DISTINCT FROM $2`
	_, err := s.pool.pool.Exec(ctx, q, id, score)
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperimentPriority: %w", err)
	}
	return nil
}

// MarkQueued sets the QUEUED lifecycle and its current scheduler decision together. Also resets
// resolved_job_spec to NULL, as every transition to QUEUED does: a requeued job is re-admitted
// from scratch and may land on a different cluster with a different fair share, or on another
// slot, so the previous resolution (a slot's size, the slot pin in its node selector) is
// stale and must not be read as though it were the resolution for wherever it lands next. A
// QUEUED row therefore never carries one, and EffectiveJob on it is the submitted job.
func (s *ExperimentsStore) MarkQueued(ctx context.Context, id, reason string) error {
	if err := sentenced(reason); err != nil {
		return fmt.Errorf("experiments_store.MarkQueued: %w", err)
	}
	const q = `UPDATE experiments SET status = 'QUEUED', queued_at = COALESCE(queued_at, NOW()),
	cluster_name = '', submitted_at = NULL, not_admitted_reason = $2, resolved_job_spec = NULL, updated_at = NOW()
	WHERE id = $1 AND status = 'SUBMITTED'`
	_, err := s.pool.pool.Exec(ctx, q, id, reason)
	return err
}

// ClaimSubmitted serializes admission decisions for one cluster, checks fresh capacity while
// holding that lock, and conditionally persists the desired-state claim. resolvedJob is the job
// as the runtime must run it (a slot's size and the grace periods resolved), written into
// resolved_job_spec in the SAME transaction as the SUBMITTED status transition — see
// domain.Experiment.ResolvedJob's doc comment. Its image is decided here and set on resolvedJob:
// the one image the cluster runs, or on a cluster that runs any, the image the job names.
func (s *ExperimentsStore) ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	if resolvedJob == nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: resolved job spec is required")
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "cluster-admission/"+clusterName); err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: lock: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT`+experimentColumns+`FROM experiments
WHERE cluster_name = $1 AND status = 'SUBMITTED'
ORDER BY submitted_at ASC, id ASC`, clusterName)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
	}
	desired, err := collectExperiments(rows)
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
	}
	// A nil callback is a speculative submit: there is no node yet for a live-fit predicate to
	// check, and the capacity being claimed is the SUBMITTED row itself (already fenced against
	// GetFlavorCapacity's desired-free by the caller's own footprint accounting). The lock and
	// the status='QUEUED' predicate below still fully serialize the claim.
	available := true
	if capacityAvailable != nil {
		var err error
		available, err = capacityAvailable(ctx, desired)
		if err != nil {
			return false, fmt.Errorf("experiments_store.ClaimSubmitted: capacity: %w", err)
		}
	}
	if !available {
		return false, nil
	}
	resolvedJobSpec, err := json.Marshal(resolvedJob)
	if err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: marshal resolved job spec: %w", err)
	}
	// This statement is the one write that places a job, so the placement rule is part of it
	// (clusterHostsJob), and it starts an attempt, so the reason the last one ended goes with it.
	// What admission filtered in memory only chose the candidate. Un-sharing,
	// removing or turning off a cluster takes the admission lock held above, so it either finds
	// this job already placed or is committed when this statement reads the cluster. The image
	// is decided from the same read of the cluster as the rule that lets the job onto it. A job
	// whose commit declares an environment is placed only once it is ready, in its image, which
	// is built on the image the cluster runs.
	const q = `UPDATE experiments e
SET status = 'SUBMITTED', submitted_at = NOW(), cluster_name = $2, last_cluster = $2, eviction_reason = NULL, not_admitted_reason = NULL, updated_at = NOW(),
	resolved_job_spec = $3::jsonb || jsonb_build_object('image', COALESCE(
		(SELECT v.image FROM environments v WHERE v.key = e.environment_key),
		(SELECT CASE WHEN c.job_image = '' THEN e.job_spec ->> 'image' ELSE c.job_image END FROM clusters c WHERE c.name = $2)))
WHERE e.id = $1 AND e.status = 'QUEUED' AND ` + clusterHostsJob + `
  AND (e.environment_key IS NULL OR EXISTS (SELECT 1 FROM environments v WHERE v.key = e.environment_key AND v.state = 'ready'))
RETURNING e.resolved_job_spec ->> 'image'`
	var image string
	switch err := tx.QueryRow(ctx, q, id, clusterName, resolvedJobSpec).Scan(&image); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: update: %w", err)
	}
	resolvedJob.Image = image
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("experiments_store.ClaimSubmitted: commit: %w", err)
	}
	return true, nil
}

// MarkStarted sets status=RUNNING, but only if still SUBMITTED. Guards against a
// job_watcher poll observing a stale "Running" phase after the experiment was already
// cancelled/evicted (the backend Job can report Running briefly until cluster-agent reconciles
// it away). Returns whether the transition happened, so callers can skip onRunning side effects
// (accelerator-type recording, flavor-substitution debit) when it didn't.
//
// The same write records the accelerator type the job runs on (ran_on_accelerator_type), which
// is the type it was admitted at: from here on the job has run, whatever ends this attempt.
func (s *ExperimentsStore) MarkStarted(ctx context.Context, id string) (bool, error) {
	const q = `UPDATE experiments SET status = 'RUNNING', ran_on_accelerator_type = accelerator_type, updated_at = NOW()
WHERE id = $1 AND status = 'SUBMITTED'`
	tag, err := s.pool.pool.Exec(ctx, q, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkUnderperforming records the ranking metric the job trails its cohort on. Only the first
// call for a job writes, and that write is what emits job.underperforming (see schema.sql), so
// the advisory is sent once per job and replays like every other row-backed event.
func (s *ExperimentsStore) MarkUnderperforming(ctx context.Context, id, metricName string) error {
	const q = `UPDATE experiments SET underperforming_metric = $2
WHERE id = $1 AND underperforming_metric IS NULL`
	if _, err := s.pool.pool.Exec(ctx, q, id, metricName); err != nil {
		return fmt.Errorf("experiments_store.MarkUnderperforming: %w", err)
	}
	return nil
}

// sentenced refuses a queue reason that is a bare code: the sentence is what the job's owner
// reads in not_admitted_message and in job.held.
func sentenced(reason string) error {
	if code, sentence := domain.SplitQueueReason(reason); code == "" || sentence == "" {
		return fmt.Errorf("queue reason %q is not \"code: sentence\"", reason)
	}
	return nil
}

// MarkHeldJobs records on a QUEUED job the queue reason it is held for, and that write is what
// emits job.held (see schema.sql). A job held by the summary gate is marked at once: its owner
// lifts that gate itself, so every minute untold is a minute lost. Any other reason is marked
// when the job has waited since it was queued for longer than its estimated duration and longer
// than minimum, once, also after a notice of the summary gate. A job the scheduler has not judged
// yet has no reason to tell.
func (s *ExperimentsStore) MarkHeldJobs(ctx context.Context, minimum time.Duration) error {
	const q = `UPDATE experiments SET held_reason = not_admitted_reason
WHERE status = 'QUEUED' AND split_part(not_admitted_reason, ': ', 1) <> 'awaiting_admission'
  AND ((held_reason IS NULL AND split_part(not_admitted_reason, ': ', 1) = 'summary_gate')
    OR ((held_reason IS NULL OR split_part(held_reason, ': ', 1) = 'summary_gate')
        AND split_part(not_admitted_reason, ': ', 1) <> 'summary_gate'
        AND NOW() - queued_at > GREATEST($1 * INTERVAL '1 second', estimated_duration_hours * INTERVAL '1 hour')))`
	if _, err := s.pool.pool.Exec(ctx, q, minimum.Seconds()); err != nil {
		return fmt.Errorf("experiments_store.MarkHeldJobs: %w", err)
	}
	return nil
}

// UpdateNotAdmittedReason replaces the explanation only while the job is still QUEUED.
// ClaimSubmitted clears it in the same statement that changes the lifecycle, so a stale
// scheduler decision can never attach a queue reason to an admitted job.
func (s *ExperimentsStore) UpdateNotAdmittedReason(ctx context.Context, id, reason string) error {
	if err := sentenced(reason); err != nil {
		return fmt.Errorf("experiments_store.UpdateNotAdmittedReason: %w", err)
	}
	const q = `UPDATE experiments SET not_admitted_reason = $2
	WHERE id = $1 AND status = 'QUEUED' AND not_admitted_reason IS DISTINCT FROM $2`
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
		resolved_job_spec = NULL,
		not_admitted_reason = '` + domain.QueueReasonAwaitingAdmission + `',
		updated_at = NOW()
	WHERE id = $1 AND status = 'RUNNING'`
	tag, err := s.pool.pool.Exec(ctx, q, id, remainingHours, newCostAccH,
		string(domain.EvictionPreemptedForGuaranteed))
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeuePreempted: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RequeueForRetry returns a gang whose attempt failed to QUEUED for a fresh whole-gang attempt,
// incrementing attempt_count in the same statement so the budget is spent exactly once even if
// two watchers observe the same failure. The row goes from SUBMITTED or RUNNING straight to
// QUEUED: it never reads FAILED, so no terminal status is announced for a job that runs again.
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
func (s *ExperimentsStore) RequeueForRetry(ctx context.Context, id string, maxAttemptsBefore int) (bool, error) {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		attempt_count = attempt_count + 1,
		submitted_at = NULL,
		-- clear cluster_name: the failed attempt holds no capacity, so the next admission tick
		-- can place this anywhere, exactly as a preemption requeue does.
		cluster_name = '',
		resolved_job_spec = NULL,
		not_admitted_reason = '` + domain.QueueReasonAwaitingAdmission + `',
		updated_at = NOW()
	WHERE id = $1 AND status IN ('SUBMITTED', 'RUNNING') AND attempt_count - infra_requeue_count < $2`
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
// The eviction reason says what ended the attempt and stays while the job waits for its next
// placement; the claim clears it. The record that outlives the attempt is tried_clusters.
// not_admitted_reason is required for any QUEUED row (experiments_queue_reason_consistent).
//
// quota_settled_at is cleared for the same reason RequeueForRetry clears it: the row is no longer
// terminal and the next attempt owes a settlement of its own.
//
// The cluster the row was placed on is appended to tried_clusters in the same statement, under
// the identity its runtime reported, so the next placement can leave it out for a while
// (scheduler.tried_cluster_ttl_seconds). That holds for every infrastructure fault the cluster
// is the cause of (domain.FailedByItsCluster), a scale-up that timed out included, and for no
// other: a job whose code could not be fetched or whose offer was gone goes back to any
// cluster at once. Every one spends the one allowance, because the tried list only delays a
// return to the cluster and a job could otherwise go round its clusters for ever.
//
// Returns false when the ceiling is reached or another writer got there first — both mean the
// caller must terminate instead.
func (s *ExperimentsStore) RequeueInfrastructureFault(ctx context.Context, id string, from domain.ExperimentStatus, reason string, maxInfraRequeues int) (bool, error) {
	const q = `UPDATE experiments e SET
		status = 'QUEUED',
		eviction_reason = $3,
		attempt_count = attempt_count + 1,
		infra_requeue_count = infra_requeue_count + 1,
		infra_requeue_reason = $3,
		submitted_at = NULL,
		-- clear cluster_name: the failed attempt holds no capacity, and the next admission tick
		-- must be free to place this somewhere other than the cluster that just failed it.
		cluster_name = '',
		resolved_job_spec = NULL,
		queued_at = COALESCE(queued_at, NOW()),
		not_admitted_reason = '` + domain.QueueReasonAwaitingAdmission + `',
		quota_settled_at = NULL,
		tried_clusters = tried_clusters || COALESCE((
			SELECT jsonb_build_array(jsonb_build_object(
				'cluster_id', c.reported_id, 'at', to_jsonb(NOW()), 'reason', split_part($3, ':', 1)))
			FROM clusters c WHERE $5 AND c.name = e.cluster_name AND c.reported_id IS NOT NULL), '[]'::jsonb),
		updated_at = NOW()
	WHERE e.id = $1 AND e.status = $2 AND e.infra_requeue_count < $4`
	tag, err := s.pool.pool.Exec(ctx, q, id, string(from), reason, maxInfraRequeues, domain.FailedByItsCluster(domain.EvictionReason(reason)))
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
