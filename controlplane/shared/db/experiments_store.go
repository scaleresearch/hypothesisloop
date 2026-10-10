package db

import (
	"context"
	"encoding/json"
	"errors"
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

// experimentColumns is the canonical column list for SELECT queries: the stored columns, then
// the job's project and org, which are read off its platform experiment where they are stored.
const experimentColumns = experimentStoredColumns + `,
	(SELECT tpe.project_id FROM platform_experiments tpe WHERE tpe.id = platform_experiment_id) AS project_id,
	(SELECT tp.org_id FROM platform_experiments tpe JOIN projects tp ON tp.id = tpe.project_id WHERE tpe.id = platform_experiment_id) AS org_id
`

const experimentStoredColumns = `
	id, parent_id, agent_id, actor_kind, actor_id, platform_experiment_id, cluster_name,
	code_ref, branch, config_hash, data_ref, job_spec, resolved_job_spec,
	hypothesis_id, hypothesis, theory,
	accelerator_type, accelerator_count,
	estimated_duration_hours, estimated_cost_acch,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, submitted_at, eviction_reason, not_admitted_reason,
	quota_settled_at, attempt_count, infra_requeue_count, infra_requeue_reason, tried_clusters, ran_on_accelerator_type,
	data_access_key_id, data_secret_access_key, secret_env,
	created_at, updated_at, COALESCE(environment_key, ''), COALESCE(same_hypothesis, '')`

// pgUniqueViolation is PostgreSQL's SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// ErrDuplicateExperiment reports that an experiment with this id already exists.
var ErrDuplicateExperiment = errors.New("experiment already exists")

// createExperiment inserts a new experiment row.
func createExperiment(ctx context.Context, executor sqlExecutor, exp *domain.Experiment) error {
	if exp.CapacityTier == "" {
		exp.CapacityTier = domain.CapacityGuaranteed
	}
	if exp.Status == domain.StatusQueued && exp.NotAdmittedReason == "" {
		exp.NotAdmittedReason = domain.QueueReasonAwaitingAdmission
	}
	if exp.Actor.Kind == "" {
		return fmt.Errorf("experiments_store.CreateExperiment: job %s names no actor: whose credential submits it is kept with it", exp.ID)
	}
	// ClusterName is deliberately left as-is (usually empty) here: it's assigned by the
	// admission loop, capacity-aware, at the moment a specific cluster's room is actually
	// claimed for this job — not guessed at submission time before any capacity is known.

	const q = `
INSERT INTO experiments (
	id, parent_id, agent_id, platform_experiment_id, cluster_name,
	code_ref, config_hash, data_ref, job_spec,
	hypothesis_id, hypothesis, theory,
	accelerator_type, accelerator_count,
	estimated_duration_hours, estimated_cost_acch,
	priority_score, novelty_score, capacity_tier, status,
	queued_at, not_admitted_reason,
	data_access_key_id, data_secret_access_key, secret_env,
	created_at, updated_at, actor_kind, actor_id, branch, submission_key, environment_key, same_hypothesis
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9,
	$10, $11, $12,
	$13, $14,
	$15, $16,
	$17, $18, $19, $20,
	$21, NULLIF($22, ''),
	$23, $24, $25,
	$26, $27, $28, $29, $30, $31, NULLIF($32, ''), NULLIF($33, '')
)`

	// Accelerator type and total count are canonical columns used by quota/capacity SQL. Do not
	// duplicate them inside job_spec; scanExperiment reconstructs the per-node API fields.
	// job_spec is what every read of the job returns, so a secret must never be written into it.
	if exp.Job.SecretEnv != nil {
		return fmt.Errorf("experiments_store.CreateExperiment: job spec of %s still carries secret_env; it is stored apart from the spec", exp.ID)
	}
	secretEnv := exp.SecretEnv
	if secretEnv == nil {
		secretEnv = map[string]string{}
	}
	persistedJob := exp.Job
	persistedJob.AcceleratorCount = 0
	jobSpec, err := json.Marshal(persistedJob)
	if err != nil {
		return fmt.Errorf("experiments_store.CreateExperiment: marshal job spec: %w", err)
	}

	_, err = executor.Exec(ctx, q,
		exp.ID, exp.ParentID, exp.AgentID, exp.PlatformExperimentID, exp.ClusterName,
		exp.CodeRef, exp.ConfigHash, exp.DataRef, jobSpec,
		exp.HypothesisID, exp.Hypothesis, exp.Theory,
		string(exp.AcceleratorType), exp.AcceleratorCount,
		exp.EstimatedDurationHours, exp.EstimatedCostAccH,
		exp.PriorityScore, exp.NoveltyScore, string(exp.CapacityTier), string(exp.Status),
		exp.CreatedAt, exp.NotAdmittedReason,
		exp.DataKey.AccessKeyID, exp.DataKey.SecretAccessKey, secretEnv,
		exp.CreatedAt, exp.UpdatedAt, string(exp.Actor.Kind), exp.Actor.ID, exp.Branch, exp.SubmissionKey, exp.EnvironmentKey, exp.SameHypothesis,
	)
	if err != nil {
		// A submission key is looked up under its agent's admission lock before this insert, so
		// the unique violation that reaches here is the primary key's: this id already exists, a
		// duplicate submission, not a server fault. Two concurrent submissions of one id both
		// find nothing in the pre-check; this is where the second loses, and it must lose as a
		// duplicate.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return fmt.Errorf("%w: %s", ErrDuplicateExperiment, exp.ID)
		}
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

// ListExperimentsTesting returns every experiment that tests one of the hypotheses, oldest first.
func (s *ExperimentsStore) ListExperimentsTesting(ctx context.Context, hypothesisIDs []string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE hypothesis_id = ANY($1) ORDER BY created_at, id`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisIDs)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListExperimentsTesting: %w", err)
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

// ExperimentStats counts the experiments matching filter, grouped in PostgreSQL. Limit/Offset/
// Sort are ignored: this is the whole matching set by construction, which is the point — a
// caller reading pages of rows can only ever count the page it holds. A non-empty groupBy (a key
// of domain.ExperimentStatsGroupFields) also fills Groups with the same counts per value.
func (s *ExperimentsStore) ExperimentStats(ctx context.Context, filter domain.ExperimentFilter, groupBy string) (*domain.ExperimentStats, error) {
	groupColumn := "''"
	if groupBy != "" {
		column, ok := domain.ExperimentStatsGroupFields[groupBy]
		if !ok {
			return nil, fmt.Errorf("experiments_store.ExperimentStats: unknown group field %q", groupBy)
		}
		groupColumn = column
	}
	clauses, args := experimentFilterClauses(filter)
	q := `SELECT ` + groupColumn + ` AS group_key, status, capacity_tier,
       ` + evictionReasonCode + ` AS reason_code, accelerator_type, COUNT(*), SUM(accelerator_count)
FROM experiments WHERE ` + strings.Join(clauses, " AND ") + `
GROUP BY group_key, status, capacity_tier, reason_code, accelerator_type`

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ExperimentStats: %w", err)
	}
	defer rows.Close()

	out := newExperimentStats()
	if groupBy != "" {
		out.Groups = map[string]*domain.ExperimentStats{}
	}
	for rows.Next() {
		var key, status, tier, reasonCode, acceleratorType string
		var n, units int
		if err := rows.Scan(&key, &status, &tier, &reasonCode, &acceleratorType, &n, &units); err != nil {
			return nil, fmt.Errorf("experiments_store.ExperimentStats: scan: %w", err)
		}
		kind := domain.AcceleratorType(acceleratorType).Kind()
		tallyExperimentStats(out, status, tier, reasonCode, kind, n, units)
		if groupBy != "" {
			if out.Groups[key] == nil {
				out.Groups[key] = newExperimentStats()
			}
			tallyExperimentStats(out.Groups[key], status, tier, reasonCode, kind, n, units)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.ExperimentStats: rows: %w", err)
	}
	classifyEvictions(out)
	for _, group := range out.Groups {
		classifyEvictions(group)
	}
	return out, nil
}

// evictionReasonCode is a row's eviction reason without its detail, as EvictionReason.Code
// gives it: split_part yields the whole string when there is no ':'.
const evictionReasonCode = `split_part(COALESCE(eviction_reason, ''), ':', 1)`

func newExperimentStats() *domain.ExperimentStats {
	return &domain.ExperimentStats{
		ByStatus:              map[string]int{},
		ByCapacityTier:        map[string]int{},
		RunningByCapacityTier: map[string]int{},
		EvictedByCapacityTier: map[string]int{},
		EvictionsByReason:     map[string]int{},
		EvictionsByClass:      map[string]int{},
	}
}

func tallyExperimentStats(out *domain.ExperimentStats, stored, tier, reasonCode string, kind domain.TypeKind, n, units int) {
	status := domain.AnsweredStatus(domain.ExperimentStatus(stored), reasonCode)
	out.Total += n
	out.ByStatus[string(status)] += n
	out.ByCapacityTier[tier] += n
	inFlight := &out.InFlightAccelerators
	if kind == domain.TypeKindCPU {
		inFlight = &out.InFlightCores
	}
	switch status {
	case domain.StatusSubmitted:
		*inFlight += units
	case domain.StatusRunning:
		*inFlight += units
		out.RunningByCapacityTier[tier] += n
	case domain.StatusEvicted:
		out.EvictedByCapacityTier[tier] += n
		if reasonCode == "" {
			reasonCode = "unknown"
		}
		out.EvictionsByReason[reasonCode] += n
	}
}

func classifyEvictions(out *domain.ExperimentStats) {
	for class, n := range domain.ClassifyCounts(out.EvictionsByReason) {
		out.EvictionsByClass[string(class)] = n
	}
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
	if filter.Status == domain.StatusCancelled {
		clauses = append(clauses, fmt.Sprintf("%s = $%d", evictionReasonCode, n))
		args = append(args, string(domain.EvictionCancelled))
		n++
	} else if filter.Status != "" {
		clauses = append(clauses, fmt.Sprintf("status = $%d AND %s <> $%d", n, evictionReasonCode, n+1))
		args = append(args, string(filter.Status), string(domain.EvictionCancelled))
		n += 2
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
		clauses = append(clauses, fmt.Sprintf("(hypothesis ILIKE $%d OR theory ILIKE $%d)", n, n))
		args = append(args, "%"+filter.Search+"%")
	}
	if clause, scoped := tenantClause(filter.Scope, "platform_experiment_id", args); clause != "" {
		clauses, args = append(clauses, clause), scoped
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
