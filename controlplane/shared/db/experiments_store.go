package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ExperimentsStore provides persistence for domain.Experiment.
type ExperimentsStore struct {
	pool *Pool
}

type sqlExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// NewExperimentsStore creates an ExperimentsStore backed by pool.
func NewExperimentsStore(pool *Pool) *ExperimentsStore {
	return &ExperimentsStore{pool: pool}
}

// experimentColumns is the canonical column list for SELECT queries.
// experimentColumnsQualified is experimentColumns with every name prefixed by an "e." alias, for
// queries where a second relation is in scope and a bare column name would be ambiguous.
var experimentColumnsQualified = qualifyColumns(experimentColumns, "e")

func qualifyColumns(cols, alias string) string {
	var b strings.Builder
	b.WriteString("\n\t")
	for i, c := range strings.Split(cols, ",") {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(alias + "." + strings.TrimSpace(c))
	}
	b.WriteString("\n")
	return b.String()
}

const experimentColumns = `
	id, parent_id, agent_id, platform_experiment_id, project_id, cluster_name,
	code_ref, config_hash, data_ref, job_spec,
	hypothesis_id, hypothesis, objective, theory,
	accelerator_type, accelerator_count,
	estimated_duration_hours, estimated_cost_acch,
	estimated_cpu_core_hours, estimated_ram_gb_hours, estimated_storage_gb_hours,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, submitted_at, eviction_reason, not_admitted_reason,
	artifacts, quota_settled_at,
	created_at, updated_at
`

// CreateExperiment inserts a new experiment row.
func (s *ExperimentsStore) CreateExperiment(ctx context.Context, exp *domain.Experiment) error {
	return createExperiment(ctx, s.pool.pool, exp)
}

func createExperiment(ctx context.Context, executor sqlExecutor, exp *domain.Experiment) error {
	artifacts := exp.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}

	if exp.CapacityTier == "" {
		exp.CapacityTier = domain.CapacityGuaranteed
	}
	if exp.Status == domain.StatusQueued && exp.NotAdmittedReason == "" {
		exp.NotAdmittedReason = domain.NotAdmittedCapacityUnavailable
	}
	// ClusterName is deliberately left as-is (usually empty) here: it's assigned by the
	// admission loop, capacity-aware, at the moment a specific cluster's room is actually
	// claimed for this job — not guessed at submission time before any capacity is known.

	const q = `
INSERT INTO experiments (
	id, parent_id, agent_id, platform_experiment_id, project_id, cluster_name,
	code_ref, config_hash, data_ref, job_spec,
	hypothesis_id, hypothesis, objective, theory,
	accelerator_type, accelerator_count,
	estimated_duration_hours, estimated_cost_acch,
	estimated_cpu_core_hours, estimated_ram_gb_hours, estimated_storage_gb_hours,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, not_admitted_reason, artifacts,
	created_at, updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6,
	$7, $8, $9, $10,
	$11, $12, $13, $14,
	$15, $16,
	$17, $18,
	$19, $20, $21,
	$22, $23, $24, $25,
	$26, NULLIF($27, ''), $28,
	$29, $30
)`

	// Accelerator type and total count are canonical columns used by quota/capacity SQL. Do not
	// duplicate them inside job_spec; scanExperiment reconstructs the per-node API fields.
	persistedJob := exp.Job
	persistedJob.AcceleratorCount = 0
	jobSpec, err := json.Marshal(persistedJob)
	if err != nil {
		return fmt.Errorf("experiments_store.CreateExperiment: marshal job spec: %w", err)
	}

	_, err = executor.Exec(ctx, q,
		exp.ID, exp.ParentID, exp.AgentID, exp.PlatformExperimentID, exp.ProjectID, exp.ClusterName,
		exp.CodeRef, exp.ConfigHash, exp.DataRef, jobSpec,
		exp.HypothesisID, exp.Hypothesis, exp.Objective, exp.Theory,
		string(exp.AcceleratorType), exp.AcceleratorCount,
		exp.EstimatedDurationHours, exp.EstimatedCostAccH,
		exp.EstimatedCPUCoreHours, exp.EstimatedRAMGBHours, exp.EstimatedStorageGBHours,
		exp.PriorityScore, exp.NoveltyScore, string(exp.CapacityTier), string(exp.Status),
		exp.CreatedAt, exp.NotAdmittedReason, artifacts,
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
	clauses, args := experimentFilterClauses(filter)
	n := len(args) + 1

	q := `SELECT` + experimentColumns + `FROM experiments
WHERE ` + strings.Join(clauses, " AND ") + `
ORDER BY ` + experimentOrderBy(filter.Sort)

	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, filter.Limit)
		n++
	}
	if filter.Offset > 0 {
		q += fmt.Sprintf(" OFFSET $%d", n)
		args = append(args, filter.Offset)
	}

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListExperiments: %w", err)
	}
	defer rows.Close()

	return collectExperiments(rows)
}

// CountExperiments returns the number of experiments matching the given filter, ignoring
// Limit/Offset/Sort — the total a caller paginating with ListExperiments should show.
func (s *ExperimentsStore) CountExperiments(ctx context.Context, filter domain.ExperimentFilter) (int, error) {
	clauses, args := experimentFilterClauses(filter)
	q := `SELECT COUNT(*) FROM experiments WHERE ` + strings.Join(clauses, " AND ")
	var total int
	if err := s.pool.pool.QueryRow(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("experiments_store.CountExperiments: %w", err)
	}
	return total, nil
}

func experimentFilterClauses(filter domain.ExperimentFilter) ([]string, []any) {
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
	if filter.Search != "" {
		clauses = append(clauses, fmt.Sprintf("(hypothesis ILIKE $%d OR objective ILIKE $%d OR theory ILIKE $%d)", n, n, n))
		args = append(args, "%"+filter.Search+"%")
		n++
	}

	return clauses, args
}

// experimentOrderBy resolves an ExperimentFilter.Sort value into a safe ORDER BY clause,
// falling back to the historical priority-then-recency order when Sort is unset or unknown.
func experimentOrderBy(sort string) string {
	if sort == "" {
		return "priority_score DESC, created_at ASC"
	}
	field, dir := sort, "ASC"
	if strings.HasPrefix(sort, "-") {
		field, dir = sort[1:], "DESC"
	}
	col, ok := domain.ExperimentSortFields[field]
	if !ok {
		return "priority_score DESC, created_at ASC"
	}
	return col + " " + dir
}

