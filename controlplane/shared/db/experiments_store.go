package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	hypothesis_id, hypothesis, objective, theory,
	gpu_type, gpu_count,
	estimated_duration_hours, estimated_cost_t4h,
	estimated_cpu_core_hours, estimated_ram_gb_hours, estimated_storage_gb_hours,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, submitted_at, started_at, preempt_count, attempt, eviction_reason, not_admitted_reason,
	actual_duration_hours, actual_cost_t4h,
	actual_cpu_core_hours, actual_ram_gb_hours, actual_storage_gb_hours,
	artifacts, quota_settled_at,
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
	// ClusterName is deliberately left as-is (usually empty) here: it's assigned by the
	// admission loop, capacity-aware, at the moment a specific cluster's room is actually
	// claimed for this job — not guessed at submission time before any capacity is known.

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
		exp.ID, exp.ParentID, exp.AgentID, exp.PlatformExperimentID, exp.ProjectID, exp.ClusterName,
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
