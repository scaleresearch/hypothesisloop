package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// TransitionStatus ends a job that ran: it sets `to` (COMPLETED or FAILED) only if the status is
// still `from`, and returns whether it did, so a completion racing an eviction lands once. reason
// is what the status is stored and announced with: why it failed, or nothing.
func (s *Store) TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error) {
	if to == domain.StatusQueued {
		return false, fmt.Errorf("experiments_store.TransitionStatus: QUEUED is set by the requeue that says why")
	}
	const q = `UPDATE experiments SET status = $3, eviction_reason = NULLIF($4, ''), not_admitted_reason = NULL, updated_at = NOW(), ended_at = NOW()
WHERE id = $1 AND status = $2`
	moved, err := s.writeJob(ctx, (*Write).jobStatus, q, id, string(from), string(to), reason)
	if err != nil {
		return false, fmt.Errorf("experiments_store.TransitionStatus: %w", err)
	}
	return moved, nil
}

// writeJob runs q, one write of at most one experiments row, and announces the row it wrote,
// reporting whether there was one.
func (s *Store) writeJob(ctx context.Context, announce func(*Write, jobWritten), q string, args ...any) (bool, error) {
	written := false
	err := s.write(ctx, func(w *Write) error {
		j, err := scanJobWritten(w.QueryRow(ctx, q+jobReturning, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		written = true
		announce(w, j)
		return nil
	})
	return written, err
}

// ListExperimentsWithStatus returns all experiments in the given status.
func (s *Store) ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return s.ListExperiments(ctx, domain.ExperimentFilter{Scope: domain.AllTenants, Status: status})
}

// UpdateExperimentPriority writes a changed priority_score. updated_at stays: it is the time of
// the row's last status transition, which ListRecentlyUndesiredWorkloads and finished_at read.
func (s *Store) UpdateExperimentPriority(ctx context.Context, id string, score float64) error {
	const q = `UPDATE experiments SET priority_score = $2 WHERE id = $1 AND priority_score IS DISTINCT FROM $2`
	_, err := s.pool.pool.Exec(ctx, q, id, score)
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperimentPriority: %w", err)
	}
	return nil
}

// ClaimSubmitted serializes admission decisions for one cluster, checks fresh capacity while
// holding that lock, and conditionally persists the desired-state claim. resolvedJob is the job
// as the runtime must run it (a slot's size and the grace periods resolved), written into
// resolved_job_spec in the SAME transaction as the SUBMITTED status transition — see
// domain.Experiment.ResolvedJob's doc comment. Its image is decided here and set on resolvedJob:
// the one image the cluster runs, or on a cluster that runs any, the image the job names.
//
// On a shared cluster it is also the one place the platform compute limits (domain.ComputeLimit)
// are kept: under the org's lock, taken after the cluster's, what the org's and the project's
// placed jobs on shared clusters reserve plus the job's own acch_per_hour stays within each
// limit. It returns why it placed nothing, and nil when it placed the job.
func (s *Store) ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (*domain.ClaimRefusal, error) {
	if resolvedJob == nil {
		return nil, fmt.Errorf("experiments_store.ClaimSubmitted: resolved job spec is required")
	}
	refusal := &domain.ClaimRefusal{Code: domain.NotAdmittedCapacityUnavailable}
	if err := s.write(ctx, func(tx *Write) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "cluster-admission/"+clusterName); err != nil {
			return fmt.Errorf("experiments_store.ClaimSubmitted: lock: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT`+experimentColumns+`FROM experiments
WHERE cluster_name = $1 AND status = 'SUBMITTED'
ORDER BY submitted_at ASC, id ASC`, clusterName)
		if err != nil {
			return fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
		}
		desired, err := collectExperiments(rows)
		rows.Close()
		if err != nil {
			return fmt.Errorf("experiments_store.ClaimSubmitted: desired placements: %w", err)
		}
		// A nil callback is a speculative submit: there is no node yet for a live-fit predicate to
		// check, and the capacity being claimed is the SUBMITTED row itself (already fenced against
		// GetAcceleratorTypeCapacity's desired-free by the caller's own footprint accounting). The lock and
		// the status='QUEUED' predicate below still fully serialize the claim.
		available := true
		if capacityAvailable != nil {
			var err error
			available, err = capacityAvailable(ctx, desired)
			if err != nil {
				return fmt.Errorf("experiments_store.ClaimSubmitted: capacity: %w", err)
			}
		}
		if !available {
			return nil
		}
		rate, limited, err := withinComputeLimit(ctx, tx, id, clusterName)
		if err != nil {
			return fmt.Errorf("experiments_store.ClaimSubmitted: %w", err)
		}
		if limited != nil {
			refusal = limited
			return nil
		}
		resolvedJobSpec, err := json.Marshal(resolvedJob)
		if err != nil {
			return fmt.Errorf("experiments_store.ClaimSubmitted: marshal resolved job spec: %w", err)
		}
		// This statement is the one write that places a job, so the placement rule is part of it
		// (clusterHostsJob), and it starts an attempt, so the reason the last one ended goes with it.
		// What admission filtered in memory only chose the candidate. Un-sharing,
		// removing or turning off a cluster takes the admission lock held above, so it either finds
		// this job already placed or is committed when this statement reads the cluster.
		q := `UPDATE experiments e
SET status = 'SUBMITTED', submitted_at = NOW(), cluster_name = $2, last_cluster = $2, eviction_reason = NULL, not_admitted_reason = NULL, updated_at = NOW(),
	resolved_job_spec = $3::jsonb, acch_per_hour = $4
WHERE e.id = $1 AND e.status = 'QUEUED' AND ` + clusterHostsJob + `
RETURNING ` + jobWrittenColumns
		var j jobWritten
		switch err := tx.QueryRow(ctx, q, id, clusterName, resolvedJobSpec, rate).Scan(j.dest()...); {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("experiments_store.ClaimSubmitted: update: %w", err)
		}
		tx.jobStatus(j)
		refusal = nil
		return nil
	}); err != nil {
		return nil, err
	}
	return refusal, nil
}

// withinComputeLimit is what job id reserves placed (domain.AccHPerHour) and, when clusterName is
// shared, the refusal of a job that would take its project or its org past its platform compute
// limit. It takes the org's lock: the caller holds the cluster's.
func withinComputeLimit(ctx context.Context, tx *Write, id, clusterName string) (float64, *domain.ClaimRefusal, error) {
	var acceleratorType domain.AcceleratorType
	var count int
	var shared bool
	var orgID, orgName, projectID, projectName string
	var limit domain.ComputeLimit
	err := tx.QueryRow(ctx, `SELECT e.accelerator_type, e.accelerator_count, COALESCE((SELECT c.shared FROM clusters c WHERE c.name = $2), false),
	o.id, o.name, p.id, p.name, o.shared_acch_per_hour, o.project_shared_acch_per_hour
FROM experiments e
JOIN platform_experiments pe ON pe.id = e.platform_experiment_id
JOIN projects p ON p.id = pe.project_id
JOIN orgs o ON o.id = p.org_id
WHERE e.id = $1`, id, clusterName).Scan(&acceleratorType, &count, &shared, &orgID, &orgName, &projectID, &projectName, &limit.Org, &limit.Project)
	if err != nil {
		return 0, nil, fmt.Errorf("compute limit: job: %w", err)
	}
	rate, err := domain.AccHPerHour(acceleratorType, count)
	if err != nil {
		return 0, nil, fmt.Errorf("compute limit: job %s: %w", id, err)
	}
	if !shared {
		return rate, nil, nil
	}
	if err := orgSharedLock(ctx, tx, orgID); err != nil {
		return 0, nil, fmt.Errorf("compute limit: lock: %w", err)
	}
	var orgHeld, projectHeld float64
	if err := tx.QueryRow(ctx, heldOnSharedSQL, orgID, projectID, id).Scan(&orgHeld, &projectHeld); err != nil {
		return 0, nil, fmt.Errorf("compute limit: held: %w", err)
	}
	switch {
	case domain.ExceedsLimit(projectHeld+rate, limit.Project):
		return rate, &domain.ClaimRefusal{Code: domain.NotAdmittedProjectComputeLimit, Holder: projectName, Held: projectHeld, Limit: limit.Project, Needs: rate}, nil
	case domain.ExceedsLimit(orgHeld+rate, limit.Org):
		return rate, &domain.ClaimRefusal{Code: domain.NotAdmittedOrgComputeLimit, Holder: orgName, Held: orgHeld, Limit: limit.Org, Needs: rate}, nil
	}
	return rate, nil, nil
}

// MarkStarted sets status=RUNNING, but only if still SUBMITTED. Guards against a
// job_watcher poll observing a stale "Running" phase after the experiment was already
// cancelled/evicted (the backend Job can report Running briefly until cluster-agent reconciles
// it away). Returns whether the transition happened, so callers can skip onRunning side effects
// (accelerator-type recording, accelerator type substitution debit) when it didn't.
//
// The same write records the accelerator type the job runs on (ran_on_accelerator_type), which
// is the type it was admitted at: from here on the job has run, whatever ends this attempt.
func (s *Store) MarkStarted(ctx context.Context, id string) (bool, error) {
	const q = `UPDATE experiments SET status = 'RUNNING', ran_on_accelerator_type = accelerator_type, updated_at = NOW()
WHERE id = $1 AND status = 'SUBMITTED'`
	moved, err := s.writeJob(ctx, (*Write).jobStatus, q, id)
	if err != nil {
		return false, fmt.Errorf("experiments_store.MarkStarted: %w", err)
	}
	return moved, nil
}

// MarkUnderperforming records the ranking metric the job trails its cohort on. Only the first
// call for a job of one that has not ended writes, and announces job.underperforming, so the
// advisory is sent once per job.
func (s *Store) MarkUnderperforming(ctx context.Context, id, metricName string) error {
	q := `UPDATE experiments SET underperforming_metric = $2
WHERE id = $1 AND underperforming_metric IS NULL AND status IN ` + unfinishedSQL
	announce := func(w *Write, j jobWritten) {
		w.Change(j.change(platformapi.EventJobUnderperforming, j.underperformingMetric, ""))
	}
	if _, err := s.writeJob(ctx, announce, q, id, metricName); err != nil {
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

// MarkHeldJobs records on a QUEUED job the queue reason it is held for and announces job.held:
// once, when the job has waited since it was queued for longer than its estimated duration and
// longer than minimum. A job the scheduler has not judged yet has no reason to tell.
func (s *Store) MarkHeldJobs(ctx context.Context, minimum time.Duration) error {
	const q = `UPDATE experiments SET held_reason = not_admitted_reason
WHERE status = 'QUEUED' AND held_reason IS NULL AND split_part(not_admitted_reason, ': ', 1) <> 'awaiting_admission'
  AND NOW() - queued_at > GREATEST($1 * INTERVAL '1 second', estimated_duration_hours * INTERVAL '1 hour')` + jobReturning
	return s.write(ctx, func(w *Write) error {
		rows, err := w.Query(ctx, q, minimum.Seconds())
		if err != nil {
			return fmt.Errorf("experiments_store.MarkHeldJobs: %w", err)
		}
		held, err := collectJobsWritten(rows)
		if err != nil {
			return fmt.Errorf("experiments_store.MarkHeldJobs: %w", err)
		}
		for _, j := range held {
			w.jobHeld(j)
		}
		return nil
	})
}

// MarkBaselineLate records reason, domain.HeldBaselineLate and its sentence, on the job id while
// it is queued or placed and not running, and announces job.held to its owner. A job is told so
// once.
func (s *Store) MarkBaselineLate(ctx context.Context, id, reason string) error {
	if err := sentenced(reason); err != nil {
		return fmt.Errorf("experiments_store.MarkBaselineLate: %w", err)
	}
	q := `UPDATE experiments SET held_reason = $2
WHERE id = $1 AND status IN ` + unstartedSQL + ` AND split_part(COALESCE(held_reason, ''), ': ', 1) <> $3`
	if _, err := s.writeJob(ctx, (*Write).jobHeld, q, id, reason, domain.HeldBaselineLate); err != nil {
		return fmt.Errorf("experiments_store.MarkBaselineLate: %w", err)
	}
	return nil
}

// UpdateNotAdmittedReason replaces the explanation only while the job is still QUEUED.
// ClaimSubmitted clears it in the same statement that changes the lifecycle, so a stale
// scheduler decision can never attach a queue reason to an admitted job.
func (s *Store) UpdateNotAdmittedReason(ctx context.Context, id, reason string) error {
	if err := sentenced(reason); err != nil {
		return fmt.Errorf("experiments_store.UpdateNotAdmittedReason: %w", err)
	}
	const q = `UPDATE experiments SET not_admitted_reason = $2
	WHERE id = $1 AND status = 'QUEUED' AND not_admitted_reason IS DISTINCT FROM $2`
	if _, err := s.writeJob(ctx, (*Write).jobQueueReason, q, id, reason); err != nil {
		return fmt.Errorf("experiments_store.UpdateNotAdmittedReason: %w", err)
	}
	return nil
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
func (s *Store) RequeuePreempted(ctx context.Context, id string, remainingHours, newCostAccH float64) (bool, error) {
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
	moved, err := s.writeJob(ctx, (*Write).jobStatus, q, id, remainingHours, newCostAccH,
		string(domain.EvictionPreemptedForGuaranteed))
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeuePreempted: %w", err)
	}
	return moved, nil
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
func (s *Store) RequeueForRetry(ctx context.Context, id string, maxAttemptsBefore int) (bool, error) {
	q := `UPDATE experiments SET
		status = 'QUEUED',
		attempt_count = attempt_count + 1,
		submitted_at = NULL,
		-- clear cluster_name: the failed attempt holds no capacity, so the next admission tick
		-- can place this anywhere, exactly as a preemption requeue does.
		cluster_name = '',
		resolved_job_spec = NULL,
		not_admitted_reason = '` + domain.QueueReasonAwaitingAdmission + `',
		updated_at = NOW()
	WHERE id = $1 AND status IN ` + placedSQL + ` AND attempt_count - infra_requeue_count < $2`
	moved, err := s.writeJob(ctx, (*Write).jobStatus, q, id, maxAttemptsBefore)
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeueForRetry: %w", err)
	}
	return moved, nil
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
func (s *Store) RequeueInfrastructureFault(ctx context.Context, id string, from domain.ExperimentStatus, reason string, maxInfraRequeues int) (bool, error) {
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
		ended_at = NULL,
		tried_clusters = tried_clusters || COALESCE((
			SELECT jsonb_build_array(jsonb_build_object(
				'cluster_id', c.reported_id, 'at', to_jsonb(NOW()), 'reason', split_part($3, ':', 1)))
			FROM clusters c WHERE $5 AND c.name = e.cluster_name AND c.reported_id IS NOT NULL), '[]'::jsonb),
		updated_at = NOW()
	WHERE e.id = $1 AND e.status = $2 AND e.infra_requeue_count < $4`
	moved, err := s.writeJob(ctx, (*Write).jobStatus, q, id, string(from), reason, maxInfraRequeues, domain.FailedByItsCluster(domain.EvictionReason(reason)))
	if err != nil {
		return false, fmt.Errorf("experiments_store.RequeueInfrastructureFault: %w", err)
	}
	return moved, nil
}

// MarkQuotaSettled records that exp's final usage was durably written to the metrics DB, so the
// settlement reconciler stops retrying it. Only called after that write succeeds.
func (s *Store) MarkQuotaSettled(ctx context.Context, id string) error {
	if err := s.write(ctx, func(tx *Write) error {
		var err error
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
		return nil
	}); err != nil {
		return err
	}
	return nil
}
