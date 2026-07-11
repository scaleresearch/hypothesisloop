package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// ExperimentsStore provides persistence for domain.Experiment.
type ExperimentsStore struct {
	pool *Pool
}

// NewExperimentsStore creates an ExperimentsStore backed by pool.
func NewExperimentsStore(pool *Pool) *ExperimentsStore {
	return &ExperimentsStore{pool: pool}
}

// experimentColumns is the canonical column list for SELECT queries.
const experimentColumns = `
	id, parent_id, agent_id, platform_experiment_id, project_id, cluster_name,
	code_ref, config_hash, data_ref, job_spec,
	hypothesis_id, hypothesis, objective, theory, summary,
	gpu_type, gpu_count,
	estimated_duration_hours, estimated_cost_t4h,
	estimated_cpu_core_hours, estimated_ram_gb_hours, estimated_storage_gb_hours,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, submitted_at, started_at, preempt_count, eviction_reason, not_admitted_reason,
	actual_duration_hours, actual_cost_t4h,
	actual_cpu_core_hours, actual_ram_gb_hours, actual_storage_gb_hours,
	artifacts,
	created_at, updated_at
`

// CreateExperiment inserts a new experiment row.
func (s *ExperimentsStore) CreateExperiment(ctx context.Context, exp *domain.Experiment) error {
	artifacts := exp.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}

	if exp.CapacityTier == "" {
		exp.CapacityTier = domain.CapacityGuaranteed
	}
	if exp.ClusterName == "" {
		exp.ClusterName = "default"
	}

	const q = `
INSERT INTO experiments (
	id, parent_id, agent_id, platform_experiment_id, project_id, cluster_name,
	code_ref, config_hash, data_ref, job_spec,
	hypothesis_id, hypothesis, objective, theory,
	gpu_type, gpu_count,
	estimated_duration_hours, estimated_cost_t4h,
	estimated_cpu_core_hours, estimated_ram_gb_hours, estimated_storage_gb_hours,
	priority_score, novelty_score, capacity_tier, status,
	actual_duration_hours, actual_cost_t4h,
	actual_cpu_core_hours, actual_ram_gb_hours, actual_storage_gb_hours,
	artifacts,
	created_at, updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6,
	$7, $8, $9, $10,
	$11, $12, $13, $14,
	$15, $16,
	$17, $18,
	$19, $20, $21,
	$22, $23, $24, $25,
	$26, $27,
	$28, $29, $30,
	$31,
	$32, $33
)`

	jobSpec, err := json.Marshal(exp.Job)
	if err != nil {
		return fmt.Errorf("experiments_store.CreateExperiment: marshal job spec: %w", err)
	}

	_, err = s.pool.pool.Exec(ctx, q,
		exp.ID, exp.ParentID, exp.AgentID, nullableStr(exp.PlatformExperimentID), exp.ProjectID, exp.ClusterName,
		exp.CodeRef, exp.ConfigHash, exp.DataRef, jobSpec,
		exp.HypothesisID, exp.Hypothesis, exp.Objective, exp.Theory,
		string(exp.GPUType), exp.GPUCount,
		exp.EstimatedDurationHours, exp.EstimatedCostT4H,
		exp.EstimatedCPUCoreHours, exp.EstimatedRAMGBHours, exp.EstimatedStorageGBHours,
		exp.PriorityScore, exp.NoveltyScore, string(exp.CapacityTier), string(exp.Status),
		exp.ActualDurationHours, exp.ActualCostT4H,
		exp.ActualCPUCoreHours, exp.ActualRAMGBHours, exp.ActualStorageGBHours,
		artifacts,
		exp.CreatedAt, exp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("experiments_store.CreateExperiment: %w", err)
	}
	return nil
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetExperiment fetches a single experiment by ID.
func (s *ExperimentsStore) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE id = $1`

	row := s.pool.pool.QueryRow(ctx, q, id)
	exp, err := scanExperiment(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("experiments_store.GetExperiment: %w", err)
	}
	return exp, nil
}

// ListExperiments returns experiments matching the given filter.
// All filter fields are optional; zero values are ignored.
func (s *ExperimentsStore) ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	clauses := []string{"1=1"}
	args := []any{}
	n := 1

	if filter.AgentID != "" {
		clauses = append(clauses, fmt.Sprintf("agent_id = $%d", n))
		args = append(args, filter.AgentID)
		n++
	}
	if filter.ProjectID != "" {
		clauses = append(clauses, fmt.Sprintf("project_id = $%d", n))
		args = append(args, filter.ProjectID)
		n++
	}
	if filter.Status != "" {
		clauses = append(clauses, fmt.Sprintf("status = $%d", n))
		args = append(args, string(filter.Status))
		n++
	}
	if filter.PlatformExperimentID != "" {
		clauses = append(clauses, fmt.Sprintf("platform_experiment_id = $%d", n))
		args = append(args, filter.PlatformExperimentID)
		n++
	}
	if filter.HypothesisID != "" {
		clauses = append(clauses, fmt.Sprintf("hypothesis_id = $%d", n))
		args = append(args, filter.HypothesisID)
		n++
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", n))
		args = append(args, filter.Since)
		n++
	}

	q := `SELECT` + experimentColumns + `FROM experiments
WHERE ` + strings.Join(clauses, " AND ") + `
ORDER BY priority_score DESC, created_at ASC`

	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, filter.Limit)
	}

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListExperiments: %w", err)
	}
	defer rows.Close()

	return collectExperiments(rows)
}

// UpdateExperiment updates all mutable fields of an experiment.
func (s *ExperimentsStore) UpdateExperiment(ctx context.Context, exp *domain.Experiment) error {
	artifacts := exp.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}

	const q = `
UPDATE experiments SET
	status                  = $2,
	priority_score          = $3,
	actual_duration_hours   = $4,
	actual_cost_t4h         = $5,
	actual_cpu_core_hours   = $6,
	actual_ram_gb_hours     = $7,
	actual_storage_gb_hours = $8,
	artifacts               = $9,
	updated_at              = $10
WHERE id = $1`

	_, err := s.pool.pool.Exec(ctx, q,
		exp.ID,
		string(exp.Status),
		exp.PriorityScore,
		exp.ActualDurationHours,
		exp.ActualCostT4H,
		exp.ActualCPUCoreHours,
		exp.ActualRAMGBHours,
		exp.ActualStorageGBHours,
		artifacts,
		exp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperiment: %w", err)
	}
	return nil
}

// UpdateExperimentStatus updates only the status field (and updated_at).
func (s *ExperimentsStore) UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error {
	const q = `UPDATE experiments SET status = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, string(status))
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperimentStatus: %w", err)
	}
	return nil
}

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

// UpdateExperimentSummary stores the agent's post-run write-up.
func (s *ExperimentsStore) UpdateExperimentSummary(ctx context.Context, id, summary string) error {
	const q = `UPDATE experiments SET summary = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, summary)
	if err != nil {
		return fmt.Errorf("experiments_store.UpdateExperimentSummary: %w", err)
	}
	return nil
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

// GetLineage returns the ancestor chain of an experiment (oldest first).
func (s *ExperimentsStore) GetLineage(ctx context.Context, id string) ([]*domain.Experiment, error) {
	q := `
WITH RECURSIVE lineage AS (
	SELECT` + experimentColumns + `FROM experiments WHERE id = $1
	UNION ALL
	SELECT` + experimentColumns + `FROM experiments e
	INNER JOIN lineage l ON e.id = l.parent_id
)
SELECT` + experimentColumns + `FROM lineage ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetLineage: %w", err)
	}
	defer rows.Close()

	return collectExperiments(rows)
}

// GetRunningAndQueued returns all experiments with status RUNNING, QUEUED, or SUBMITTED.
func (s *ExperimentsStore) GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('RUNNING', 'QUEUED', 'SUBMITTED')
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetRunningAndQueued: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListRunningExperiments returns all RUNNING experiments.
func (s *ExperimentsStore) ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status = 'RUNNING'
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListRunningExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListExperimentsByPlatformExperiment returns all jobs for a given platform experiment.
func (s *ExperimentsStore) ListExperimentsByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE platform_experiment_id = $1
ORDER BY created_at DESC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListExperimentsByPlatformExperiment: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListActiveByPlatformExperiment returns all non-terminal jobs (QUEUED, SUBMITTED, ADMITTED,
// RUNNING) for a platform experiment. Used to evict jobs when an experiment is closed.
func (s *ExperimentsStore) ListActiveByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE platform_experiment_id = $1
  AND status IN ('QUEUED', 'SUBMITTED', 'ADMITTED', 'RUNNING')
ORDER BY created_at DESC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListActiveByPlatformExperiment: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// MarkQueued sets status=QUEUED and queued_at=now (first time only — preserved on requeue).
func (s *ExperimentsStore) MarkQueued(ctx context.Context, id string) error {
	const q = `UPDATE experiments SET status = 'QUEUED', queued_at = COALESCE(queued_at, NOW()), updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id)
	return err
}

// MarkSubmitted sets status=SUBMITTED and submitted_at=now.
func (s *ExperimentsStore) MarkSubmitted(ctx context.Context, id string) error {
	const q = `UPDATE experiments SET status = 'SUBMITTED', submitted_at = NOW(), updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id)
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
// (age_score), increments preempt_count, updates estimated_duration_hours to remaining.
func (s *ExperimentsStore) RequeuePreempted(ctx context.Context, id string, remainingHours float64) error {
	const q = `UPDATE experiments SET
		status = 'QUEUED',
		preempt_count = preempt_count + 1,
		eviction_reason = 'preempted_for_guaranteed',
		estimated_duration_hours = $2,
		-- rescale cost proportionally to the new (shorter) remaining duration, not just
		-- overwrite it — old_cost * (new_hours / old_hours) keeps the $/hour rate intact so
		-- the still-debited reservation matches what's actually left to run.
		estimated_cost_t4h = estimated_cost_t4h * ($2 / NULLIF(estimated_duration_hours, 0)),
		submitted_at = NULL,
		started_at = NULL,
		updated_at = NOW()
	WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, remainingHours)
	return err
}

// ListSubmittedExperiments returns all experiments in SUBMITTED state.
func (s *ExperimentsStore) ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'SUBMITTED' ORDER BY submitted_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListSubmittedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListAdmittedExperiments returns all experiments in ADMITTED state (submitted to the
// workload backend, not yet observed RUNNING by the job watcher) — still holding physical
// capacity, same as SUBMITTED/RUNNING.
func (s *ExperimentsStore) ListAdmittedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'ADMITTED' ORDER BY submitted_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListAdmittedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListQueuedExperiments returns all QUEUED experiments ordered by queued_at ASC (age_score).
func (s *ExperimentsStore) ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'QUEUED' ORDER BY queued_at ASC NULLS LAST, created_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListQueuedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// GetAgentRunningExperiments returns RUNNING experiments for an agent in a platform experiment.
func (s *ExperimentsStore) GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE agent_id = $1 AND platform_experiment_id = $2 AND status = 'RUNNING'
ORDER BY started_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetAgentRunningExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// GetAgentQueuedExperiments returns QUEUED and SUBMITTED experiments for an agent in a platform experiment.
// Used by the quota-exhaustion path to cancel waiting jobs and return their reservations.
func (s *ExperimentsStore) GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE agent_id = $1 AND platform_experiment_id = $2 AND status IN ('QUEUED', 'SUBMITTED')
ORDER BY queued_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetAgentQueuedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment
// in the given platform experiment without a summary. Agents must document successful
// runs before submitting new jobs so the collective learning loop is maintained.
// FAILED and EVICTED runs are excluded — documenting unsuccessful infrastructure
// outcomes adds little signal and would unfairly block agents after noisy failures.
func (s *ExperimentsStore) HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM experiments
		WHERE agent_id = $1
		  AND platform_experiment_id = $2
		  AND status = 'COMPLETED'
		  AND (summary IS NULL OR summary = '')
	)`
	var exists bool
	err := s.pool.pool.QueryRow(ctx, q, agentID, platformExpID).Scan(&exists)
	return exists, err
}

// CountRecentSubmissions counts experiments submitted by the agent within the given
// platform experiment since the given time. Used to enforce per-agent submission rate limits.
func (s *ExperimentsStore) CountRecentSubmissions(ctx context.Context, agentID, platformExpID string, since time.Time) (int, error) {
	const q = `SELECT COUNT(*) FROM experiments
	WHERE agent_id = $1
	  AND platform_experiment_id = $2
	  AND created_at >= $3`
	var n int
	err := s.pool.pool.QueryRow(ctx, q, agentID, platformExpID, since).Scan(&n)
	return n, err
}

// ---- helpers ----

type rowScanner interface {
	Scan(dest ...any) error
}

func scanExperiment(row rowScanner) (*domain.Experiment, error) {
	exp := &domain.Experiment{}
	var gpuType, capacityTier, status string
	var artifacts []string
	var platformExpID *string
	var evictionReason *string
	var notAdmittedReason *string
	var jobSpec []byte

	if err := row.Scan(
		&exp.ID, &exp.ParentID, &exp.AgentID, &platformExpID, &exp.ProjectID, &exp.ClusterName,
		&exp.CodeRef, &exp.ConfigHash, &exp.DataRef, &jobSpec,
		&exp.HypothesisID, &exp.Hypothesis, &exp.Objective, &exp.Theory, &exp.Summary,
		&gpuType, &exp.GPUCount,
		&exp.EstimatedDurationHours, &exp.EstimatedCostT4H,
		&exp.EstimatedCPUCoreHours, &exp.EstimatedRAMGBHours, &exp.EstimatedStorageGBHours,
		&exp.PriorityScore, &exp.NoveltyScore, &capacityTier, &status,
		&exp.QueuedAt, &exp.SubmittedAt, &exp.StartedAt, &exp.PreemptCount, &evictionReason, &notAdmittedReason,
		&exp.ActualDurationHours, &exp.ActualCostT4H,
		&exp.ActualCPUCoreHours, &exp.ActualRAMGBHours, &exp.ActualStorageGBHours,
		&artifacts,
		&exp.CreatedAt, &exp.UpdatedAt,
	); err != nil {
		return nil, err
	}

	if len(jobSpec) > 0 {
		if err := json.Unmarshal(jobSpec, &exp.Job); err != nil {
			return nil, fmt.Errorf("scan experiment: unmarshal job spec: %w", err)
		}
	}
	exp.GPUType = domain.GPUType(gpuType)
	exp.CapacityTier = domain.CapacityTier(capacityTier)
	exp.Status = domain.ExperimentStatus(status)
	if platformExpID != nil {
		exp.PlatformExperimentID = *platformExpID
	}
	if evictionReason != nil {
		exp.EvictionReason = *evictionReason
	}
	if notAdmittedReason != nil {
		exp.NotAdmittedReason = *notAdmittedReason
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	exp.Artifacts = artifacts

	return exp, nil
}

func collectExperiments(rows pgx.Rows) ([]*domain.Experiment, error) {
	var out []*domain.Experiment
	for rows.Next() {
		exp, err := scanExperiment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan experiment: %w", err)
		}
		out = append(out, exp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
