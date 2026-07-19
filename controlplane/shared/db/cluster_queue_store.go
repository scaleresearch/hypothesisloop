package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// ClusterQueueStore backs the desired-state feed and job-status reports cluster-agents
// push to. The control plane never writes an explicit "create"/"delete" instruction — the
// experiment's own status (already updated by services/scheduler before Backend is called)
// *is* the instruction, and it is the single source of truth: no parallel "desired" table.
// A cluster-agent asks "what's the desired set of Jobs for my cluster?" and reconciles its
// local state to match, the same way a kubelet reconciles pods to the spec it's given — no
// outbox, no claim/lease/ack bookkeeping needed.
//
// Callers that need a Job gone immediately (preemption, eviction) must update the
// experiment's status away from the desired set (SUBMITTED/ADMITTED/RUNNING) *before*
// waiting for deletion to be confirmed — see services/scheduler/loop.go's preempt() and
// services/controller's eviction paths, all of which do the status update first specifically
// so this table stays the only source of truth for "should a Job exist."
type ClusterQueueStore struct {
	pool *Pool
}

// NewClusterQueueStore creates a ClusterQueueStore backed by pool.
func NewClusterQueueStore(pool *Pool) *ClusterQueueStore {
	return &ClusterQueueStore{pool: pool}
}

// desiredStatuses are the experiment statuses that should have a running Job. Anything not
// in this set (COMPLETED, FAILED, EVICTED, REJECTED, QUEUED, DRAFT, PROMOTED) should not.
var desiredStatuses = []string{
	string(domain.StatusSubmitted),
	string(domain.StatusAdmitted),
	string(domain.StatusRunning),
}

// ListDesiredWorkloads returns every experiment in clusterName that should currently have a
// Job running — the cluster-agent's whole "what should exist" view. Read-only; needs no
// locking, since it's not claiming anything, just reporting current truth.
func (s *ClusterQueueStore) ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE cluster_name = $1 AND status = ANY($2)
ORDER BY id`
	rows, err := s.pool.pool.Query(ctx, q, clusterName, desiredStatuses)
	if err != nil {
		return nil, fmt.Errorf("db.ListDesiredWorkloads: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// UpsertJobReport records the latest observed phase for experimentID, applying it only if
// seq is greater than the previously stored sequence_number — guards against out-of-order or
// duplicate delivery from a reconnecting or retrying cluster-agent.
//
// jobUID is the k8s Job's UID as reported by cluster-agent. The first non-empty UID seen for an
// experiment is treated as authoritative and never overwritten (COALESCE keeps it); a
// subsequent report carrying a *different* non-empty UID is a real anomaly (name collision,
// stray manually-created Job, a second cluster-agent misconfigured against the same cluster) —
// uidMismatch reports that so the caller can flag it (log + metric), not silently accept or
// silently drop the report. The phase/sequence update still applies either way — ownership
// verification is a distinct signal, not a reason to stop tracking the job's lifecycle.
//
// applied reports whether the row actually changed: false means the write did not take effect
// (stale/duplicate sequence number, or experimentID isn't assigned to clusterName) and the
// caller must not treat this report as durably acknowledged — see clusteragentapi.PushStatus,
// which surfaces exactly this back to the cluster-agent so it retries instead of losing the
// report.
func (s *ClusterQueueStore) UpsertJobReport(ctx context.Context, experimentID, clusterName, phase string, admittedAcceleratorType domain.AcceleratorType, seq int64, jobUID string) (applied bool, uidMismatch bool, err error) {
	if jobUID != "" {
		var existing *string
		err := s.pool.pool.QueryRow(ctx,
			`SELECT job_uid FROM cluster_job_reports WHERE experiment_id = $1`, experimentID,
		).Scan(&existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, false, fmt.Errorf("db.UpsertJobReport: check uid: %w", err)
		}
		if existing != nil && *existing != "" && *existing != jobUID {
			uidMismatch = true
		}
	}

	var acceleratorArg, uidArg any
	if admittedAcceleratorType != "" {
		acceleratorArg = string(admittedAcceleratorType)
	}
	if jobUID != "" {
		uidArg = jobUID
	}
	// The SELECT ... WHERE EXISTS guard (instead of a plain VALUES insert) is the ownership
	// check: clusterAuthMiddleware already proved the caller *is* clusterName's cluster-agent,
	// but without this it could still overwrite the job report of an experiment assigned to a
	// different cluster (clusterName here is caller-supplied, taken from the URL, not derived
	// from data this experiment actually belongs to). Requiring experiments.cluster_name =
	// clusterName means a report for a job this cluster doesn't own is silently dropped —
	// same as a stale/lower sequence number, both are "did not apply", reflected in RowsAffected.
	tag, execErr := s.pool.pool.Exec(ctx, `
		INSERT INTO cluster_job_reports (experiment_id, cluster_name, phase, admitted_accelerator_type, job_uid, sequence_number, updated_at)
		SELECT $1, $2, $3, $4, $5, $6, now()
		WHERE EXISTS (SELECT 1 FROM experiments WHERE id = $1 AND cluster_name = $2)
		ON CONFLICT (experiment_id) DO UPDATE SET
			cluster_name = EXCLUDED.cluster_name,
			phase = EXCLUDED.phase,
			admitted_accelerator_type = COALESCE(EXCLUDED.admitted_accelerator_type, cluster_job_reports.admitted_accelerator_type),
			job_uid = COALESCE(cluster_job_reports.job_uid, EXCLUDED.job_uid),
			sequence_number = EXCLUDED.sequence_number,
			updated_at = now()
		WHERE EXCLUDED.sequence_number > cluster_job_reports.sequence_number
	`, experimentID, clusterName, phase, acceleratorArg, uidArg, seq)
	if execErr != nil {
		return false, uidMismatch, fmt.Errorf("db.UpsertJobReport: %w", execErr)
	}
	return tag.RowsAffected() > 0, uidMismatch, nil
}

// JobReport is the latest pushed status for one experiment's job.
type JobReport struct {
	Phase           string
	AdmittedAcceleratorType domain.AcceleratorType
	UpdatedAt       time.Time
}

// GetJobReport returns the latest report for experimentID, or (nil, nil) if none yet.
func (s *ClusterQueueStore) GetJobReport(ctx context.Context, experimentID string) (*JobReport, error) {
	var r JobReport
	var accelerator *string
	err := s.pool.pool.QueryRow(ctx,
		`SELECT phase, admitted_accelerator_type, updated_at FROM cluster_job_reports WHERE experiment_id = $1`,
		experimentID,
	).Scan(&r.Phase, &accelerator, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db.GetJobReport: %w", err)
	}
	if accelerator != nil {
		r.AdmittedAcceleratorType = domain.AcceleratorType(*accelerator)
	}
	return &r, nil
}

// DeleteJobReport removes the report row once an experiment reaches a terminal state and its
// status no longer needs tracking (keeps the table bounded to in-flight experiments). Used by
// trusted internal callers (queuebackend cleanup) that already know experimentID is theirs to
// remove; not exposed to cluster-agents directly — see DeleteJobReportForCluster for that.
func (s *ClusterQueueStore) DeleteJobReport(ctx context.Context, experimentID string) error {
	_, err := s.pool.pool.Exec(ctx, `DELETE FROM cluster_job_reports WHERE experiment_id = $1`, experimentID)
	if err != nil {
		return fmt.Errorf("db.DeleteJobReport: %w", err)
	}
	return nil
}

// DeleteJobReportForCluster removes the tracked job report for experimentID, scoped to
// clusterName — used by the cluster-agent-facing PushStatus handler so an authenticated
// cluster-agent reporting a job "gone" can only delete its own cluster's report, never
// another cluster's report for an experiment it doesn't own.
func (s *ClusterQueueStore) DeleteJobReportForCluster(ctx context.Context, experimentID, clusterName string) error {
	_, err := s.pool.pool.Exec(ctx,
		`DELETE FROM cluster_job_reports WHERE experiment_id = $1 AND cluster_name = $2`,
		experimentID, clusterName)
	if err != nil {
		return fmt.Errorf("db.DeleteJobReportForCluster: %w", err)
	}
	return nil
}

// BumpAttemptOnRecreate increments experiments.attempt for experimentID, but only if it is
// still in a desired-running status (SUBMITTED/ADMITTED/RUNNING) — called when a cluster-agent
// reports a Job gone, so the caller can tell "this experiment's Job vanished and is about to be
// recreated" (bumped=true) from "this experiment already reached a terminal state, the gone
// report is just expected cleanup" (bumped=false, no attempt change needed). The next
// desired-state poll carries the new attempt number, and cluster-agent stamps it onto the
// recreated Job so the new object is identifiable as a distinct execution attempt.
func (s *ClusterQueueStore) BumpAttemptOnRecreate(ctx context.Context, experimentID string) (attempt int, bumped bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `
		UPDATE experiments SET attempt = attempt + 1, updated_at = now()
		WHERE id = $1 AND status = ANY($2)
		RETURNING attempt
	`, experimentID, desiredStatuses).Scan(&attempt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("db.BumpAttemptOnRecreate: %w", err)
	}
	return attempt, true, nil
}

// RecordHeartbeat marks clusterName as seen just now. Called on every desired-state poll, so
// a cluster with no recent heartbeat means its agent has stopped connecting, not that the
// control plane failed to reach it (it never tries to). cpuAvailable/cpuTotal are the
// cluster-agent's live-computed CPU core counts for this poll; pass (0, false) when not
// reported (leaves the previously stored value untouched rather than zeroing it out).
func (s *ClusterQueueStore) RecordHeartbeat(ctx context.Context, clusterName string, cpuAvailable, cpuTotal float64, hasCPUReport bool) error {
	var cpuAvailArg, cpuTotalArg any
	if hasCPUReport {
		cpuAvailArg, cpuTotalArg = cpuAvailable, cpuTotal
	}
	_, err := s.pool.pool.Exec(ctx, `
		INSERT INTO cluster_heartbeats (cluster_name, last_seen_at, cpu_available_cores, cpu_total_cores)
		VALUES ($1, now(), $2, $3)
		ON CONFLICT (cluster_name) DO UPDATE SET
			last_seen_at = now(),
			cpu_available_cores = COALESCE($2, cluster_heartbeats.cpu_available_cores),
			cpu_total_cores = COALESCE($3, cluster_heartbeats.cpu_total_cores)
	`, clusterName, cpuAvailArg, cpuTotalArg)
	if err != nil {
		return fmt.Errorf("db.RecordHeartbeat: %w", err)
	}
	return nil
}

// ClusterHeartbeat is the last-seen time and last-reported live CPU capacity for one cluster.
type ClusterHeartbeat struct {
	ClusterName       string
	LastSeenAt        time.Time
	CPUAvailableCores float64
	CPUTotalCores     float64
}

// ListHeartbeats returns the last-seen time for every cluster that has ever polled.
func (s *ClusterQueueStore) ListHeartbeats(ctx context.Context) ([]ClusterHeartbeat, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT cluster_name, last_seen_at, cpu_available_cores, cpu_total_cores FROM cluster_heartbeats ORDER BY cluster_name`)
	if err != nil {
		return nil, fmt.Errorf("db.ListHeartbeats: %w", err)
	}
	defer rows.Close()
	var out []ClusterHeartbeat
	for rows.Next() {
		var h ClusterHeartbeat
		var cpuAvail, cpuTotal *float64
		if err := rows.Scan(&h.ClusterName, &h.LastSeenAt, &cpuAvail, &cpuTotal); err != nil {
			return nil, fmt.Errorf("db.ListHeartbeats: scan: %w", err)
		}
		if cpuAvail != nil {
			h.CPUAvailableCores = *cpuAvail
		}
		if cpuTotal != nil {
			h.CPUTotalCores = *cpuTotal
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListStaleDesiredState returns SUBMITTED/ADMITTED/RUNNING experiments whose most recent
// cluster_job_reports row is older than staleAfter, or that have no report row at all despite
// being in a desired-running status — the control-plane-side GC sweep: cluster-agent's own
// ~2-3s reconcile is GC-like on the cluster side, but nothing previously caught the rarer
// double-failure of an extended cluster-agent outage combined with a control-plane bug/crash
// that leaves an experiment stuck desired with no corresponding live Job report anywhere.
// Alert-only (see Controller.sweepStaleDesiredState) — this just finds candidates, it never
// mutates state.
func (s *ClusterQueueStore) ListStaleDesiredState(ctx context.Context, staleAfter time.Duration) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments e
		WHERE e.status = ANY($1)
		AND e.submitted_at IS NOT NULL AND e.submitted_at < $2
		AND NOT EXISTS (
			SELECT 1 FROM cluster_job_reports r
			WHERE r.experiment_id = e.id AND r.updated_at > $2
		)
		ORDER BY e.id`
	cutoff := time.Now().Add(-staleAfter)
	rows, err := s.pool.pool.Query(ctx, q, desiredStatuses, cutoff)
	if err != nil {
		return nil, fmt.Errorf("db.ListStaleDesiredState: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// GetLiveCPUCapacityByCluster returns the most recently reported cpu_available_cores per
// cluster whose heartbeat is still fresh (within connectedWithin) — stale/disconnected
// clusters are simply absent from the result rather than contributing a possibly-long-stale
// number. Broken out per cluster (not summed into one pool) so admission can reason about
// which specific cluster has room.
func (s *ClusterQueueStore) GetLiveCPUCapacityByCluster(ctx context.Context, connectedWithin time.Duration) (map[string]float64, error) {
	cutoff := time.Now().Add(-connectedWithin)
	rows, err := s.pool.pool.Query(ctx, `
		SELECT cluster_name, cpu_available_cores
		FROM cluster_heartbeats
		WHERE cpu_available_cores IS NOT NULL AND last_seen_at > $1
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("db.GetLiveCPUCapacityByCluster: %w", err)
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var name string
		var avail float64
		if err := rows.Scan(&name, &avail); err != nil {
			return nil, fmt.Errorf("db.GetLiveCPUCapacityByCluster: %w", err)
		}
		out[name] = avail
	}
	return out, rows.Err()
}
