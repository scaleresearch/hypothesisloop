package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// InTxObservationTimeout bounds every metrics-store read taken while a quota transaction holds
// its advisory lock. Those reads are HTTP calls to a separate service, and an unbounded one stalls
// admission for the whole cluster behind the lock while its connection stays checked out of a
// small pool. Failing the transaction is recoverable — the caller retries on the next tick —
// where hanging on the lock is not.
const InTxObservationTimeout = 5 * time.Second

// PlatformExperimentsStore provides persistence for platform experiments,
// agent signups, and per-agent quotas.
type PlatformExperimentsStore struct {
	pool *Pool
}

// NewPlatformExperimentsStore creates a PlatformExperimentsStore backed by pool.
func NewPlatformExperimentsStore(pool *Pool) *PlatformExperimentsStore {
	return &PlatformExperimentsStore{pool: pool}
}

// ---- platform_experiments ----

// coordinatorColumns are what a platform experiment says of the coordinator the platform runs
// for it, read from platform_experiments as pe.
var coordinatorColumns = `pe.coordinator_hosted, pe.coordinator_launcher, pe.coordinator_model, pe.coordinator_instance_name,
       pe.coordinator_instance_restarts, pe.coordinator_instance_renewals, pe.coordinator_instance_stopped, pe.coordinator_instance_exited_at, pe.coordinator_instance_exit_log,
       pe.coordinator_instance_seen_at, pe.coordinator_instance_asked_at, pe.coordinator_instance_launch,
       pe.coordinator_instance_state, pe.coordinator_instance_progress_at, pe.coordinator_instance_end_reason,
       ` + domain.InstanceReleasedSQL("pe.closed_at")

// CreatePlatformExperiment stores pe and signs up the agents it is created with, the ones of
// create made first, or stores nothing. The limits a signup holds are held here too
// (ErrMaxAgentsReached, ErrMaxHostedReached).
func (s *PlatformExperimentsStore) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertPlatformExperiment(ctx, tx, pe); err != nil {
		return err
	}
	if len(signups) > 0 {
		if _, err := enrolTx(ctx, tx, pe.ID, create, signups, nil); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.Create: commit: %w", err)
	}
	return nil
}

// ProjectOrg is the org a project belongs to: ErrProjectNotFound for no such project.
func (s *PlatformExperimentsStore) ProjectOrg(ctx context.Context, projectID string) (string, error) {
	var orgID string
	err := s.pool.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, projectID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProjectNotFound
	}
	if err != nil {
		return "", fmt.Errorf("platform_experiments_store.ProjectOrg: %w", err)
	}
	return orgID, nil
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func insertPlatformExperiment(ctx context.Context, db execer, pe *domain.PlatformExperiment) error {
	const q = `
INSERT INTO platform_experiments (id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, created_at, updated_at, max_concurrent_accelerators, default_quota_tier, coordinator_hosted, coordinator_launcher, coordinator_model, max_hosted, repository, repository_url, agent_image, goal, created_by_kind, created_by_id, duration_hours, repository_account, continues)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, NULLIF($30, ''))`

	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: marshal metrics: %w", err)
	}
	// The ladder is fixed at creation, so it is validated here rather than defended against
	// on every read.
	if err := domain.ValidateStages(pe.Stages); err != nil {
		return fmt.Errorf("platform_experiments_store.Create: %w", err)
	}
	stages, err := json.Marshal(pe.Stages)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: marshal stages: %w", err)
	}
	if pe.Repository != "" && pe.RepositoryAccount != "" {
		return fmt.Errorf("platform_experiments_store.Create: a platform experiment names its repository or the account one is created on, got repository %q and account %q", pe.Repository, pe.RepositoryAccount)
	}
	if pe.CoordinatorHosted != (pe.Coordinator != nil) {
		return fmt.Errorf("platform_experiments_store.Create: coordinator_hosted is %v and a coordinator is given: %v", pe.CoordinatorHosted, pe.Coordinator != nil)
	}
	var coordinator domain.HostedCoordinator
	if pe.Coordinator != nil {
		coordinator = *pe.Coordinator
	}
	var createdBy domain.Actor
	if pe.Actor != nil {
		createdBy = *pe.Actor
	}
	_, err = db.Exec(ctx, q,
		pe.ID, pe.ProjectID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds,
		pe.StartsAt, pe.ClosesAt(), string(pe.Status),
		stages, 1,
		pe.CreatedAt, pe.UpdatedAt, pe.MaxConcurrentAccelerators, string(pe.DefaultQuotaTier),
		pe.CoordinatorHosted, coordinator.Launcher, coordinator.Model, pe.MaxHosted,
		pe.Repository, pe.RepositoryURL, pe.AgentImage,
		pe.Goal, string(createdBy.Kind), createdBy.ID, pe.DurationHours, pe.RepositoryAccount, pe.Continues,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: %w", err)
	}
	return nil
}

// summarySQL is the summary a platform experiment is answered with: the coordinator's, then the
// platform's record of the close once it is written.
const summarySQL = `concat_ws(E'\n\n', NULLIF(pe.summary, ''), NULLIF(pe.close_record, ''))`

// plannerOfDraft selects, of experiment_signups as planner, the planner of pe while pe is a draft.
const plannerOfDraft = `planner.platform_experiment_id = pe.id AND planner.role = '` + string(domain.SignupRolePlanner) + `' AND pe.status = 'draft'`

// priorRuns selects, latest close first, the ids of the prior runs of platform experiment $1:
// every other closed one of its project that works on the same repository and ran: its
// researchers started or it declared a baseline, so a draft closed before either is none. The run
// its continues names, and each one that run carried on, is among them, since only a run that
// declared a baseline can be continued.
const priorRuns = `
SELECT prior.id FROM platform_experiments run
JOIN platform_experiments prior ON prior.project_id = run.project_id AND prior.repository = run.repository
WHERE run.id = $1 AND run.repository <> '' AND prior.id <> run.id AND prior.status = '` + string(domain.PlatformExpClosed) + `'
  AND (prior.started_at IS NOT NULL OR prior.baseline_job_id <> '')
ORDER BY prior.closed_at DESC, prior.id`

// PriorRuns is the ids of the prior runs of platform experiment id, empty when it has none.
func (s *PlatformExperimentsStore) PriorRuns(ctx context.Context, id string) ([]string, error) {
	var ids []string
	if err := s.pool.pool.QueryRow(ctx, `SELECT ARRAY(`+priorRuns+`)`, id).Scan(&ids); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.PriorRuns: %w", err)
	}
	return ids, nil
}

// RunToRank is a platform experiment as its standings are read: what a listing names it by, its
// metrics and creation, its COMPLETED jobs and its researchers.
type RunToRank struct {
	PE          *domain.PlatformExperiment
	Completed   map[string]bool
	Competitors map[string]bool
}

// RunsToRank reads the platform experiments ids, in their order, in one query. One with no row
// is left out.
func (s *PlatformExperimentsStore) RunsToRank(ctx context.Context, ids []string) ([]RunToRank, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT pe.id, pe.name, pe.status, pe.closed_at, pe.close_reason, pe.metrics, pe.created_at,
       ARRAY(SELECT e.id FROM experiments e WHERE e.platform_experiment_id = pe.id AND e.status = 'COMPLETED'),
       ARRAY(SELECT s.agent_id FROM experiment_signups s WHERE s.platform_experiment_id = pe.id AND s.role = $2)
FROM unnest($1::text[]) WITH ORDINALITY AS wanted(id, n)
JOIN platform_experiments pe ON pe.id = wanted.id
ORDER BY wanted.n`, ids, string(domain.SignupRoleCompetitor))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RunsToRank: %w", err)
	}
	defer rows.Close()
	var out []RunToRank
	for rows.Next() {
		pe := &domain.PlatformExperiment{}
		var status string
		var metricsRaw []byte
		var completed, competitors []string
		if err := rows.Scan(&pe.ID, &pe.Name, &status, &pe.ClosedAt, &pe.CloseReason, &metricsRaw, &pe.CreatedAt, &completed, &competitors); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.RunsToRank: scan: %w", err)
		}
		if err := json.Unmarshal(metricsRaw, &pe.Metrics); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.RunsToRank: unmarshal metrics of %s: %w", pe.ID, err)
		}
		pe.Status = domain.PlatformExperimentStatus(status)
		run := RunToRank{PE: pe, Completed: make(map[string]bool, len(completed)), Competitors: make(map[string]bool, len(competitors))}
		for _, id := range completed {
			run.Completed[id] = true
		}
		for _, id := range competitors {
			run.Competitors[id] = true
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// GetPlatformExperiment fetches a single platform experiment by ID.
func (s *PlatformExperimentsStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	q := `
SELECT id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, ` + summarySQL + `, created_at, updated_at, max_concurrent_accelerators, default_quota_tier, pe.shared,
       ` + coordinatorColumns + `, pe.max_hosted, pe.repository, pe.repository_url, pe.agent_image, pe.goal, COALESCE(pe.continues, ''), pe.created_by_kind, pe.created_by_id, pe.proposal, pe.proposed_at, pe.declined_reason,
       pe.baseline_job_id, pe.job_estimated_duration_hours, pe.closed_at, pe.close_reason, pe.closed_by_kind, pe.closed_by_id, pe.close_message, pe.repository_error, pe.repository_account,
       pe.duration_hours, pe.started_at, pe.planner_ends_at, pe.accepts_at, pe.coordinator_instance_activity, pe.coordinator_instance_activity_at,
       (SELECT planner.instance_activity FROM experiment_signups planner WHERE ` + plannerOfDraft + `),
       (SELECT planner.instance_activity_at FROM experiment_signups planner WHERE ` + plannerOfDraft + `)
FROM platform_experiments pe
WHERE id = $1`

	pe := &domain.PlatformExperiment{}
	var status string
	var metricsRaw, stagesRaw []byte
	var coordinator domain.HostedCoordinator
	var createdBy, closedBy domain.Actor
	var proposal []byte
	var closesAt time.Time
	var startedAt, answeredAt, plannerEndsAt, acceptsAt, coordinatorActivityAt, plannerActivityAt *time.Time
	var coordinatorActivity string
	var plannerActivity *string
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&pe.ID, &pe.ProjectID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.MaxAgents,
		&metricsRaw, &pe.ReportIntervalSeconds,
		&pe.StartsAt, &closesAt, &status,
		&stagesRaw, &pe.CurrentStage, &pe.Summary,
		&pe.CreatedAt, &pe.UpdatedAt, &pe.MaxConcurrentAccelerators, &pe.DefaultQuotaTier, &pe.Shared,
		&pe.CoordinatorHosted, &coordinator.Launcher, &coordinator.Model, &coordinator.Instance.Name, &coordinator.Instance.Restarts,
		&coordinator.Instance.Renewals, &coordinator.Instance.Stopped, &coordinator.Instance.ExitedAt, &coordinator.Instance.ExitLog,
		&coordinator.Instance.SeenAt, &coordinator.Instance.AskedAt, &coordinator.Instance.Launch, &coordinator.Instance.Reported, &coordinator.Instance.ProgressAt, &coordinator.Instance.EndReason, &coordinator.Instance.Released, &pe.MaxHosted, &pe.Repository, &pe.RepositoryURL, &pe.AgentImage,
		&pe.Goal, &pe.Continues, &createdBy.Kind, &createdBy.ID, &proposal, &answeredAt, &pe.DeclinedReason,
		&pe.BaselineJobID, &pe.JobEstimatedDurationHours, &pe.ClosedAt, &pe.CloseReason, &closedBy.Kind, &closedBy.ID, &pe.CloseMessage, &pe.RepositoryError, &pe.RepositoryAccount,
		&pe.DurationHours, &startedAt, &plannerEndsAt, &acceptsAt, &coordinatorActivity, &coordinatorActivityAt,
		&plannerActivity, &plannerActivityAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("platform_experiments_store.Get: %w", err)
	}
	if status == string(domain.PlatformExpDraft) {
		pe.PlannerEndsAt, pe.AcceptsAt = plannerEndsAt, acceptsAt
	}
	if plannerActivity != nil {
		pe.Activity = domain.ActivityOf(*plannerActivity, plannerActivityAt)
	}
	if pe.CoordinatorHosted {
		coordinator.Activity = domain.ActivityOf(coordinatorActivity, coordinatorActivityAt)
		pe.Coordinator = &coordinator
	}
	if createdBy.Kind == "" {
		createdBy.Kind = domain.ActorUnverified
	}
	pe.Actor = &createdBy
	if closedBy.Kind != "" {
		pe.ClosedBy = &closedBy
	}
	if len(proposal) > 0 {
		pe.Proposal = proposal
	}
	pe.SetAnswered(answeredAt, len(proposal) > 0)
	if pe.Goal != "" {
		if pe.Revisions, err = s.planningRequests(ctx, id); err != nil {
			return nil, err
		}
	}
	if pe.BaselineJobID != "" {
		if pe.BaselineRaises, err = s.baselineRaises(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(metricsRaw, &pe.Metrics); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.Get: unmarshal metrics: %w", err)
	}
	if err := json.Unmarshal(stagesRaw, &pe.Stages); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.Get: unmarshal stages: %w", err)
	}
	pe.Status = domain.PlatformExperimentStatus(status)
	if startedAt != nil {
		pe.StartedAt = *startedAt
	}
	pe.SetClosesAt(closesAt)
	return pe, nil
}

// PlatformExperimentExists says whether there is a platform experiment of that id, for a
// service that needs no more of it.
func (s *PlatformExperimentsStore) PlatformExperimentExists(ctx context.Context, id string) (bool, error) {
	var found bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_experiments WHERE id = $1)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("platform_experiments_store.PlatformExperimentExists: %w", err)
	}
	return found, nil
}

// GetPlatformExperimentMetrics is a narrow read of just the declared MetricDefinitions, used to
// tell a ranking metric's direction apart from a constraint or attribute one (registry.Service's
// underperformance check) without pulling the whole row through that service's Store interface.
func (s *PlatformExperimentsStore) GetPlatformExperimentMetrics(ctx context.Context, id string) (metrics []domain.MetricDefinition, found bool, err error) {
	const q = `SELECT metrics FROM platform_experiments WHERE id = $1`
	var raw []byte
	if err := s.pool.pool.QueryRow(ctx, q, id).Scan(&raw); err != nil {
		if err == pgx.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("platform_experiments_store.GetPlatformExperimentMetrics: %w", err)
	}
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return nil, false, fmt.Errorf("platform_experiments_store.GetPlatformExperimentMetrics: unmarshal metrics: %w", err)
	}
	return metrics, true, nil
}

// PlatformExperimentsFilter constrains ListPlatformExperiments/CountPlatformExperiments.
// Zero values are ignored.
type PlatformExperimentsFilter struct {
	Status string
	// Search matches against name/description, case-insensitively, substring match.
	Search string
	// Scope keeps the platform experiments the caller may see.
	Scope domain.TenantScope
	// WithShared also keeps the ones staff shared with every account, whatever Scope says: an
	// account lists those beside its own.
	WithShared bool
	// Shared, when set, keeps only the shared ones or only the others.
	Shared *bool
	Limit  int
	Offset int
	// Sort selects the ORDER BY column and direction, as "field" (ascending) or "-field"
	// (descending). Empty keeps the default, newest-first order. See
	// PlatformExperimentSortFields for the allowed field names.
	Sort string
}

// PlatformExperimentSortFields are the column names PlatformExperimentsFilter.Sort may
// reference, mapped to their underlying SQL column — an explicit allowlist so Sort can't be
// used for SQL injection.
var PlatformExperimentSortFields = map[string]string{
	"created_at": "pe.created_at",
	"starts_at":  "pe.starts_at",
	"name":       "pe.name",
	"status":     "pe.status",
}

// statusPriorityTiebreak ranks running/open experiments ahead of closed ones when the primary
// sort column is tied, so a page of same-date experiments doesn't bury active ones under
// arbitrary insertion order.
const statusPriorityTiebreak = "(CASE WHEN pe.status = 'closed' THEN 1 ELSE 0 END)"

func platformExperimentOrderBy(sort string) string {
	if sort == "" {
		return "pe.created_at DESC, " + statusPriorityTiebreak
	}
	field, dir := sort, "ASC"
	if strings.HasPrefix(sort, "-") {
		field, dir = sort[1:], "DESC"
	}
	col, ok := PlatformExperimentSortFields[field]
	if !ok {
		return "pe.created_at DESC, " + statusPriorityTiebreak
	}
	return col + " " + dir + ", " + statusPriorityTiebreak
}

func platformExperimentFilterClauses(filter PlatformExperimentsFilter) ([]string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	n := 1
	if filter.Status != "" {
		clauses = append(clauses, fmt.Sprintf("pe.status = $%d", n))
		args = append(args, filter.Status)
		n++
	}
	if filter.Search != "" {
		clauses = append(clauses, fmt.Sprintf("(pe.name ILIKE $%d OR pe.description ILIKE $%d)", n, n))
		args = append(args, "%"+filter.Search+"%")
	}
	if filter.Shared != nil {
		clauses = append(clauses, map[bool]string{true: "pe.shared", false: "NOT pe.shared"}[*filter.Shared])
	}
	if clause, scoped := tenantClause(filter.Scope, "pe.id", args); clause != "" {
		if filter.WithShared {
			clause = "(" + clause + " OR pe.shared)"
		}
		clauses, args = append(clauses, clause), scoped
	}
	return clauses, args
}

// defaultPlatformExperimentListLimit is the page of a ListPlatformExperiments read that states
// none, so Limit<=0 never means "unbounded", and MaxPlatformExperimentListLimit the largest page
// GET /platform-experiments takes: the handler refuses a larger one, so the store never shortens
// a page on its own.
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultPlatformExperimentListLimit = 20
	MaxPlatformExperimentListLimit     = 200
)

// ListPlatformExperiments returns platform experiments matching the given filter. A filter.Limit
// of 0 or less is the default page.
func (s *PlatformExperimentsStore) ListPlatformExperiments(ctx context.Context, filter PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error) {
	if filter.Limit <= 0 {
		filter.Limit = defaultPlatformExperimentListLimit
	}

	clauses, args := platformExperimentFilterClauses(filter)
	n := len(args) + 1

	q := platformExperimentSelect(clauses, filter.Sort) + fmt.Sprintf(" LIMIT $%d", n)
	args = append(args, filter.Limit)
	n++
	if filter.Offset > 0 {
		q += fmt.Sprintf(" OFFSET $%d", n)
		args = append(args, filter.Offset)
	}

	return s.queryPlatformExperiments(ctx, q, args...)
}

// ListPlatformExperimentsByStatus returns EVERY platform experiment in one status, unpaginated.
//
// This is the control-plane loops' read, deliberately separate from ListPlatformExperiments.
// That one is the agent-facing list and defaults to a small page on purpose; a reconciler that
// borrowed it silently stopped reconciling at the page boundary — stage ladders past the first
// twenty running platform experiments never advanced, and nothing anywhere said so. A control
// loop acts on all of its subjects or on none of them, so it must never read them through a
// paginated query.
func (s *PlatformExperimentsStore) ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	clauses, args := platformExperimentFilterClauses(PlatformExperimentsFilter{Scope: domain.AllTenants, Status: string(status)})
	return s.queryPlatformExperiments(ctx, platformExperimentSelect(clauses, ""), args...)
}

func platformExperimentSelect(clauses []string, sort string) string {
	return `
SELECT pe.id, pe.project_id, pe.name, pe.description, pe.budget_accelerator_hours, pe.max_agents,
       pe.metrics, pe.report_interval_seconds,
       pe.starts_at, pe.ends_at, pe.status,
       pe.stages, pe.current_stage, ` + summarySQL + `,
       pe.created_at, pe.updated_at, pe.max_concurrent_accelerators, pe.default_quota_tier, pe.shared,
       pe.coordinator_hosted, pe.max_hosted, pe.repository, pe.repository_url, pe.agent_image, pe.goal, pe.proposed_at, pe.proposal IS NOT NULL, pe.declined_reason, pe.baseline_job_id,
       pe.duration_hours, pe.started_at, pe.closed_at, pe.close_reason,
       COUNT(es.agent_id) AS signup_count
FROM platform_experiments pe
LEFT JOIN experiment_signups es ON es.platform_experiment_id = pe.id
WHERE ` + strings.Join(clauses, " AND ") + `
GROUP BY pe.id
ORDER BY ` + platformExperimentOrderBy(sort)
}

func (s *PlatformExperimentsStore) queryPlatformExperiments(ctx context.Context, q string, args ...any) ([]*domain.PlatformExperiment, error) {
	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.List: %w", err)
	}
	defer rows.Close()

	var out []*domain.PlatformExperiment
	for rows.Next() {
		pe := &domain.PlatformExperiment{}
		var status string
		var metricsRaw, stagesRaw []byte
		var closesAt time.Time
		var startedAt, answeredAt *time.Time
		var proposed bool
		if err := rows.Scan(
			&pe.ID, &pe.ProjectID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.MaxAgents,
			&metricsRaw, &pe.ReportIntervalSeconds,
			&pe.StartsAt, &closesAt, &status,
			&stagesRaw, &pe.CurrentStage, &pe.Summary,
			&pe.CreatedAt, &pe.UpdatedAt, &pe.MaxConcurrentAccelerators, &pe.DefaultQuotaTier, &pe.Shared,
			&pe.CoordinatorHosted, &pe.MaxHosted, &pe.Repository, &pe.RepositoryURL, &pe.AgentImage, &pe.Goal, &answeredAt, &proposed, &pe.DeclinedReason, &pe.BaselineJobID,
			&pe.DurationHours, &startedAt, &pe.ClosedAt, &pe.CloseReason,
			&pe.SignupCount,
		); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.List: scan: %w", err)
		}
		if err := json.Unmarshal(metricsRaw, &pe.Metrics); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.List: unmarshal metrics: %w", err)
		}
		if err := json.Unmarshal(stagesRaw, &pe.Stages); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.List: unmarshal stages: %w", err)
		}
		pe.Status = domain.PlatformExperimentStatus(status)
		if startedAt != nil {
			pe.StartedAt = *startedAt
		}
		pe.SetAnswered(answeredAt, proposed)
		pe.SetClosesAt(closesAt)
		out = append(out, pe)
	}
	return out, rows.Err()
}

// CountPlatformExperiments returns the number of platform experiments matching the given
// filter, ignoring Limit/Offset/Sort — the total a paginating caller should show.
func (s *PlatformExperimentsStore) CountPlatformExperiments(ctx context.Context, filter PlatformExperimentsFilter) (int, error) {
	clauses, args := platformExperimentFilterClauses(filter)
	q := `SELECT COUNT(*) FROM platform_experiments pe WHERE ` + strings.Join(clauses, " AND ")
	var total int
	if err := s.pool.pool.QueryRow(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Count: %w", err)
	}
	return total, nil
}

// ClosePlatformExperiment closes a platform experiment and records why: by is whose credential
// asked, nil when the platform closes it at its end time, and message what that caller said of
// why. One already closed keeps what it has.
func (s *PlatformExperimentsStore) ClosePlatformExperiment(ctx context.Context, id string, by *domain.Actor, message string) error {
	reason, actor := "", domain.Actor{}
	if by != nil {
		reason, actor = string(domain.CloseByRequest), *by
	}
	// What the platform closes is a run at its end, a draft nobody proposed for inside the
	// planning limit or whose answer no person took up, an open one that was not running by
	// its baseline deadline, or a running one no researcher's machine came up for.
	const q = `
UPDATE platform_experiments
SET status = 'closed', updated_at = NOW(), closed_at = NOW(),
    close_reason = CASE WHEN $2 <> '' THEN $2
                        WHEN status = 'draft' AND proposed_at IS NULL THEN '` + string(domain.ClosePlanningLimitReached) + `'
                        WHEN status = 'draft' THEN '` + string(domain.CloseProposalExpired) + `'
                        WHEN status = 'open' THEN '` + string(domain.CloseBaselineLimitReached) + `'
                        WHEN started_at IS NULL THEN '` + string(domain.CloseAgentsNotStarted) + `'
                        ELSE '` + string(domain.CloseEndTimeReached) + `' END,
    closed_by_kind = $3, closed_by_id = $4, close_message = $5
WHERE id = $1 AND status <> 'closed'`
	if _, err := s.pool.pool.Exec(ctx, q, id, reason, string(actor.Kind), actor.ID, message); err != nil {
		return fmt.Errorf("platform_experiments_store.Close: %w", err)
	}
	return nil
}

// RecordClose writes record as the platform's record of the close of the closed platform
// experiment id, once: one written before stays. The coordinator's summary is kept beside it.
func (s *PlatformExperimentsStore) RecordClose(ctx context.Context, id, record string) error {
	const q = `UPDATE platform_experiments SET close_record = $2, updated_at = NOW() WHERE id = $1 AND status = 'closed' AND close_record = ''`
	if _, err := s.pool.pool.Exec(ctx, q, id, record); err != nil {
		return fmt.Errorf("platform_experiments_store.RecordClose: %w", err)
	}
	return nil
}

// ErrNotReopenable is a platform experiment that is not a closed run that started and no other
// run continues.
var ErrNotReopenable = errors.New("platform experiment is not a closed run that started")

// ReopenPlatformExperiment sets the closed platform experiment id, whose run started and which no
// other run continues, running again until endsAt, durationHours from its start, and with a
// coordinatorModel that is not empty its hosted coordinator on that model. What the close wrote
// goes with it: closed_at, why and by whom, the agent lines and record of the close and the
// placements, so every reader takes it for the running run it was. The status change is announced
// by the row's trigger. ErrNotReopenable, and ErrPlatformExperimentNotFound for an id none has.
func (s *PlatformExperimentsStore) ReopenPlatformExperiment(ctx context.Context, id string, endsAt time.Time, durationHours float64, coordinatorModel string) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
UPDATE platform_experiments pe
SET status = 'running', updated_at = now(), ends_at = $2, duration_hours = $3,
    coordinator_model = CASE WHEN $4 <> '' THEN $4 ELSE coordinator_model END,
    closed_at = NULL, close_reason = '', closed_by_kind = '', closed_by_id = '', close_message = '',
    close_agents = '', close_record = ''
WHERE pe.id = $1 AND pe.status = 'closed' AND pe.started_at IS NOT NULL AND ($4 = '' OR pe.coordinator_hosted)
  AND NOT EXISTS (SELECT 1 FROM platform_experiments later WHERE later.continues = pe.id)`, id, endsAt, durationHours, coordinatorModel)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_experiments WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("platform_experiments_store.Reopen: %w", err)
		}
		if !exists {
			return ErrPlatformExperimentNotFound
		}
		return ErrNotReopenable
	}
	if _, err := tx.Exec(ctx, `DELETE FROM experiment_top3 WHERE platform_experiment_id = $1`, id); err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: placements: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: commit: %w", err)
	}
	return nil
}

// unsettledJobsSQL says a job of pe still runs, waits to, or ended and its cost is not yet
// written to the metrics store (quota_settled_at): what pe spent is not yet whole.
const unsettledJobsSQL = `EXISTS (SELECT 1 FROM experiments e WHERE e.platform_experiment_id = pe.id
    AND (e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING') OR e.quota_settled_at IS NULL))`

// JobsSettled says every job of the platform experiment id ended and its cost was written, so
// what it spent is whole.
func (s *PlatformExperimentsStore) JobsSettled(ctx context.Context, id string) (bool, error) {
	var settled bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT NOT `+unsettledJobsSQL+` FROM platform_experiments pe WHERE pe.id = $1`, id).Scan(&settled); err != nil {
		return false, fmt.Errorf("platform_experiments_store.JobsSettled: %w", err)
	}
	return settled, nil
}

// ClosesToRecord is the platform experiments closed in the last within, after they were open,
// that have no record of the close yet and whose every job ended and was settled (JobsSettled): those whose
// record of the close waited for what they spent to be whole.
func (s *PlatformExperimentsStore) ClosesToRecord(ctx context.Context, within time.Duration) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT pe.id FROM platform_experiments pe
WHERE pe.status = 'closed' AND pe.close_record = '' AND pe.closed_at > now() - make_interval(secs => $1)
  AND pe.close_reason IN ($2, $3, $4, $5) AND (pe.goal = '' OR pe.starts_at >= '0001-01-02')
  AND NOT `+unsettledJobsSQL+`
ORDER BY pe.closed_at`, within.Seconds(), string(domain.CloseEndTimeReached), string(domain.CloseByRequest), string(domain.CloseBaselineLimitReached), string(domain.CloseAgentsNotStarted))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ClosesToRecord: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ClosesToRecord: %w", err)
	}
	return ids, nil
}

// SetPlatformExperimentSummary records the operator's narrative verdict on a run. Separate from
// UpdatePlatformExperiment because it is the one field that is written *after* the experiment
// closes, when everything else is frozen. ErrPlatformExperimentNotFound for an id none has.
func (s *PlatformExperimentsStore) SetPlatformExperimentSummary(ctx context.Context, id, summary string) error {
	const q = `UPDATE platform_experiments SET summary = $2, updated_at = NOW() WHERE id = $1`
	tag, err := s.pool.pool.Exec(ctx, q, id, summary)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.SetSummary: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPlatformExperimentNotFound
	}
	return nil
}

// TellCoordinator records reason as the status_reason the platform experiment id waits for its
// coordinator with, "" when it waits for someone else. A reason that differs from the one
// recorded is announced as platform_experiment.status with the reason as its detail, by the
// trigger on the row, and one that is the same writes nothing. A closed one is told nothing.
func (s *PlatformExperimentsStore) TellCoordinator(ctx context.Context, id, reason string) error {
	const q = `UPDATE platform_experiments SET coordinator_reason = NULLIF($2, '')
WHERE id = $1 AND status <> 'closed' AND coordinator_reason IS DISTINCT FROM NULLIF($2, '')`
	if _, err := s.pool.pool.Exec(ctx, q, id, reason); err != nil {
		return fmt.Errorf("platform_experiments_store.TellCoordinator: %w", err)
	}
	return nil
}

// HoldBaselineClock moves baseline_due_at of the open platform experiment id, which has no
// baseline declared, out by the time its baseline waits on the platform: held says it waits now.
// The time since the last call that saw it waiting is credited, up to most in all, so a wait is
// counted from the first call that sees it and is short by at most the interval between calls.
func (s *PlatformExperimentsStore) HoldBaselineClock(ctx context.Context, id string, held bool, most time.Duration) error {
	const q = `
UPDATE platform_experiments pe
SET ends_at = pe.ends_at + make_interval(secs => c.credit),
    baseline_held_seconds = pe.baseline_held_seconds + c.credit,
    baseline_held_at = CASE WHEN $2::boolean THEN now() END
FROM (SELECT id, CASE WHEN $2::boolean AND baseline_held_at IS NOT NULL
                      THEN LEAST(GREATEST(EXTRACT(EPOCH FROM now() - baseline_held_at)::double precision, 0),
                                 GREATEST($3::double precision - baseline_held_seconds, 0))
                      ELSE 0 END AS credit
      FROM platform_experiments WHERE id = $1) c
WHERE pe.id = c.id AND pe.status = 'open' AND pe.baseline_job_id = '' AND ($2::boolean OR pe.baseline_held_at IS NOT NULL)`
	if _, err := s.pool.pool.Exec(ctx, q, id, held, most.Seconds()); err != nil {
		return fmt.Errorf("platform_experiments_store.HoldBaselineClock: %w", err)
	}
	return nil
}

// UpdatePlatformExperiment writes pe's editable fields, but only if the row is still in
// expectedStatus — the service reads status, decides which fields are safe to change for that
// status, then calls here; without this compare-and-swap, a concurrent Start() could flip the row
// to running between that read and this write, and a since-locked field (budget, metrics, ...)
// computed under the stale "open" decision would land after admission already used the old value.
// A mismatch (0 rows affected) means status moved: fail fast rather than silently no-op.
//
// The quota of an agent of role baseline is a share of the budget, so in an open experiment,
// where the budget changes, it is written again from the row as patched in the same
// transaction (allocateBaselineQuota).
func (s *PlatformExperimentsStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: marshal metrics: %w", err)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := writePlatformExperiment(ctx, tx, pe, metrics, expectedStatus, ""); err != nil {
		return fmt.Errorf("platform_experiments_store.Update: %w", err)
	}
	if expectedStatus == domain.PlatformExpOpen {
		if err := reallocateBaselineQuotas(ctx, tx, pe); err != nil {
			return fmt.Errorf("platform_experiments_store.Update: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.Update: commit: %w", err)
	}
	return nil
}

// writePlatformExperiment writes the patchable fields of pe while it is still in expectedStatus
// and, when baseline is not empty, still has that baseline.
func writePlatformExperiment(ctx context.Context, tx pgx.Tx, pe *domain.PlatformExperiment, metrics []byte, expectedStatus domain.PlatformExperimentStatus, baseline string) error {
	const q = `UPDATE platform_experiments
SET name=$2, description=$3, budget_accelerator_hours=$4, max_agents=$5, metrics=$6,
    report_interval_seconds=$7, starts_at=$8, ends_at=$9, updated_at=NOW(),
    max_concurrent_accelerators=$11, baseline_job_id=$12, job_estimated_duration_hours=$13, duration_hours=$14
WHERE id=$1 AND status=$10 AND ($15 = '' OR baseline_job_id = $15)`
	tag, err := tx.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds, pe.StartsAt, pe.ClosesAt(),
		expectedStatus,
		pe.MaxConcurrentAccelerators, pe.BaselineJobID, pe.JobEstimatedDurationHours, pe.DurationHours, baseline,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("status or baseline changed concurrently, retry")
	}
	return nil
}

// RaiseBaseline writes the running pe as patched, its baseline raised from raise.From to
// raise.To, and records the raise, or does neither: one whose status or baseline moved since it
// was read is an error.
func (s *PlatformExperimentsStore) RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: marshal metrics: %w", err)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := writePlatformExperiment(ctx, tx, pe, metrics, domain.PlatformExpRunning, raise.From); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO baseline_raises (platform_experiment_id, from_job_id, to_job_id, reason, detail, actor_kind, actor_id, created_xid)
VALUES ($1, $2, $3, $4, $5, $6, $7, pg_current_xact_id()::text::bigint)`,
		pe.ID, raise.From, raise.To, raise.Reason, raise.Detail, string(raise.Actor.Kind), raise.Actor.ID); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: commit: %w", err)
	}
	return nil
}

func (s *PlatformExperimentsStore) baselineRaises(ctx context.Context, id string) ([]domain.BaselineRaise, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT from_job_id, to_job_id, reason, detail, actor_kind, actor_id, created_at FROM baseline_raises WHERE platform_experiment_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.baselineRaises: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.BaselineRaise, error) {
		var raise domain.BaselineRaise
		err := row.Scan(&raise.From, &raise.To, &raise.Reason, &raise.Detail, &raise.Actor.Kind, &raise.Actor.ID, &raise.At)
		raise.At = raise.At.UTC()
		return raise, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.baselineRaises: %w", err)
	}
	return out, nil
}

// reallocateBaselineQuotas writes the quota of every agent of role baseline of pe again, from
// pe's budget.
func reallocateBaselineQuotas(ctx context.Context, tx pgx.Tx, pe *domain.PlatformExperiment) error {
	rows, err := tx.Query(ctx, `
SELECT agent_id, quota_tier FROM experiment_signups
WHERE platform_experiment_id = $1 AND role = $2`, pe.ID, string(domain.SignupRoleBaseline))
	if err != nil {
		return fmt.Errorf("list baseline agents: %w", err)
	}
	type holder struct {
		AgentID string
		Tier    domain.QuotaTier
	}
	holders, err := pgx.CollectRows(rows, pgx.RowToStructByPos[holder])
	if err != nil {
		return fmt.Errorf("list baseline agents: %w", err)
	}
	for _, h := range holders {
		if err := allocateBaselineQuota(ctx, tx, pe.ID, h.AgentID, h.Tier, pe.BudgetAcceleratorHours, pe.DefaultQuotaTier); err != nil {
			return err
		}
	}
	return nil
}

// StageRedistribution describes one resource dimension's move across a stage boundary: the cut
// agents' unspent allocation, plus the share of the platform budget withheld for the incoming
// stage, split evenly across survivors (domain.StageCredit).
//
// UsedByAgent carries observed consumption per cut agent, which is metrics-store data and so
// cannot be read under the Postgres lock. That is safe in a way the allocation is not: observed
// usage only ever moves up, whereas an allocation can move in either direction, and
// deriving "unspent" from an allocation read before the lock mints or destroys hours relative to
// the platform budget. The allocation side is therefore read inside the transaction.
type StageRedistribution struct {
	ResourceType domain.ResourceType
	Budget       float64
	ReleaseFrac  float64
	UsedByAgent  map[string]float64
	// UsedBurstByAgent is the same observed figure for burst-tier work, which is where a
	// burst-only agent's whole allocation and consumption live.
	UsedBurstByAgent map[string]float64
}

// StartParticipant is one signed-up agent and everything the allocation needs to know about it,
// resolved inside the start transaction.
type StartParticipant struct {
	AgentID string
	// Role says whether the agent is a researcher, which the run's ladder is sized by.
	Role        domain.SignupRole
	AgentExists bool
	HasTop3     bool
	// QuotaTierOverride is what domain.ResolveQuotaTier needs to decide which column this
	// participant's share lands in. Read here, inside the start transaction, because the signup
	// set is: resolving the tier from a separate query could pair one transaction's participants
	// with another's overrides.
	QuotaTierOverride domain.QuotaTier
}

// sharesBudgetSQL is domain.AgentSignup.SharesBudget of the signup s: a researcher or any other
// agent the start gives a share.
const sharesBudgetSQL = `s.role NOT IN ('` + string(domain.SignupRoleBaseline) + `', '` + string(domain.SignupRolePlanner) + `')`

// runOverAtSQL is the end of the run of the running pe: duration_hours after its starts_at, the
// moment the run started.
const runOverAtSQL = `pe.starts_at + make_interval(secs => pe.duration_hours * 3600)`

// startClock records that a running platform experiment that waits for a researcher's machine
// has one at work, once the machine asked for under name for agentID is reported running:
// started_at is now, and it closes at the end of its run. It writes nothing for one that has a
// researcher at work.
func (s *PlatformExperimentsStore) startClock(ctx context.Context, platformExpID, agentID, name string) error {
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments pe
SET started_at = NOW(), ends_at = `+runOverAtSQL+`, updated_at = NOW()
WHERE pe.id = $1 AND pe.status = 'running' AND pe.started_at IS NULL
  AND EXISTS (SELECT 1 FROM experiment_signups s
              WHERE s.platform_experiment_id = pe.id AND s.agent_id = $2 AND s.hosted AND s.instance_name = $3
                AND s.instance_state = '`+domain.InstanceRunning+`' AND `+sharesBudgetSQL+`)`,
		platformExpID, agentID, name); err != nil {
		return fmt.Errorf("platform_experiments_store.startClock: %w", err)
	}
	return nil
}

// StartPlatformExperimentTx flips a platform experiment open->running and writes every
// participating agent's quota and the ladder the run has (domain.RunStages) in one transaction.
// The run starts now: starts_at is now, and it ends duration_hours later whatever measuring the
// baseline took. With a researcher its owner runs, started_at is now and it closes at that end.
// With only researchers the platform runs, started_at stays empty until the first one's machine
// is reported running (startClock), and it closes agentsDue from now, or at that end when sooner:
// the sweep closes one whose machines never came. quotasFor is
// called with the signup set read inside that transaction; returning an error rolls the whole
// thing back, leaving the experiment open and the call safe to retry.
//
// Atomic because the two halves used to be separate: the signup list was read, quotas were
// written, and only then was the status flipped. An agent signing up in that window was durably
// signed up with no quota row and no path to ever get one. Flipping the status inside the same
// transaction closes the window from the other side too — Signup only inserts while the
// experiment is still open, so a signup either lands before this transaction and is seen by it,
// or fails.
//
// Returns started=false when the experiment was not open.
func (s *PlatformExperimentsStore) StartPlatformExperimentTx(ctx context.Context, id string, agentsDue time.Duration, quotasFor func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
WITH at_work AS (
    SELECT EXISTS (SELECT 1 FROM experiment_signups s
                   WHERE s.platform_experiment_id = $1 AND NOT s.hosted AND `+sharesBudgetSQL+`) AS already
)
UPDATE platform_experiments pe
SET status='running', updated_at=NOW(), starts_at = NOW(),
    started_at = CASE WHEN at_work.already THEN NOW() END,
    ends_at = NOW() + make_interval(secs => CASE WHEN at_work.already THEN pe.duration_hours * 3600 ELSE LEAST($2, pe.duration_hours * 3600) END)
FROM at_work
WHERE pe.id=$1 AND pe.status='open'`, id, agentsDue.Seconds())
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil, nil
	}

	// An agent of role baseline takes no share of the run: it measured the baseline before the
	// start, against the quota it joined with. A draft's planner took part in the draft only.
	// Signups joined against the agents they name and their top-3 history, all on this
	// transaction's own connection. The caller must not issue its own queries while this
	// transaction is open: several concurrent starts would each hold a connection and wait for
	// another from the same pool, which is a deadlock rather than a slow path.
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, s.role, a.id IS NOT NULL, EXISTS (SELECT 1 FROM experiment_top3 t WHERE t.agent_id = s.agent_id),
       s.quota_tier
FROM experiment_signups s
LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1 AND `+sharesBudgetSQL+`
ORDER BY s.agent_id`, id)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
	}
	var participants []StartParticipant
	for rows.Next() {
		var p StartParticipant
		if err := rows.Scan(&p.AgentID, &p.Role, &p.AgentExists, &p.HasTop3, &p.QuotaTierOverride); err != nil {
			rows.Close()
			return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: scan signup: %w", err)
		}
		participants = append(participants, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
	}

	quotas, stages, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
	}
	if err := domain.ValidateStages(stages); err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: %w", err)
	}
	stagesJSON, err := json.Marshal(stages)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: marshal stages: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET stages=$2 WHERE id=$1`, id, stagesJSON); err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: store stages: %w", err)
	}
	for _, q := range quotas {
		if _, err := tx.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(q)...); err != nil {
			return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: upsert quota for %s: %w", q.AgentID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: commit: %w", err)
	}
	return true, quotas, nil
}

// AdvanceStage commits one stage boundary in a single Postgres transaction: it claims
// stageIndex in platform_experiment_stage_advances, records the cut agents, applies every
// zero/add quota op, and moves current_stage to stageIndex+1.
//
// All of it is one commit because quota moves are the only part that cannot be retried: re-running
// an add's delta would double-credit an agent, and there is no idempotent way to retry a
// partially-applied delta. Any crash before commit leaves the advance row absent and nothing
// applied, so the next reconcile tick redoes the whole boundary safely from scratch; any crash
// after commit has already applied everything exactly once. Job-stopping for cut agents is
// naturally idempotent and lives outside this transaction, retried every tick.
//
// Returns (false, nil) if this boundary was already committed by an earlier call — the caller
// must skip straight to retrying job-stopping instead of re-applying ops.
func (s *PlatformExperimentsStore) AdvanceStage(ctx context.Context, platformExpID string, stageIndex int, cutAgentIDs, survivorIDs []string, dims []StageRedistribution) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.AdvanceStage: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The primary key on (platform_experiment_id, stage_index) is the claim: exactly one caller
	// inserts a row for this boundary, so concurrent control-service replicas reconciling the
	// same platform experiment can never both apply the ops.
	tag, err := tx.Exec(ctx,
		`INSERT INTO platform_experiment_stage_advances (platform_experiment_id, stage_index)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		platformExpID, stageIndex,
	)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.AdvanceStage: claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // boundary already committed
	}

	for _, agentID := range cutAgentIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			platformExpID, agentID, stageIndex,
		); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: insert cut %s: %w", agentID, err)
		}
	}

	for _, dim := range dims {
		if dim.Budget <= 0 {
			continue // platform experiment does not track this dimension
		}
		guaranteed, burst := resourceQuotaColumns(dim.ResourceType)

		// Lock every cut agent's row and read its allocation here, not from a snapshot taken
		// before the transaction: a stage move committing in between would make "unspent" wrong in
		// either direction, minting or destroying hours against the platform budget.
		tiers, err := participantTiers(ctx, tx, platformExpID, cutAgentIDs)
		if err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: cut agent tiers: %w", err)
		}
		allocated := make(map[string]float64, len(cutAgentIDs))
		used := make(map[string]float64, len(cutAgentIDs))
		lockQ := fmt.Sprintf(
			`SELECT agent_id, %s, %s FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id = ANY($2) ORDER BY agent_id FOR UPDATE`,
			guaranteed, burst)
		rows, err := tx.Query(ctx, lockQ, platformExpID, cutAgentIDs)
		if err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
		}
		for rows.Next() {
			var agentID string
			var g, b float64
			if err := rows.Scan(&agentID, &g, &b); err != nil {
				rows.Close()
				return false, fmt.Errorf("platform_experiments_store.AdvanceStage: scan cut allocation: %w", err)
			}
			tier, ok := tiers[agentID]
			if !ok {
				rows.Close()
				return false, fmt.Errorf("platform_experiments_store.AdvanceStage: cut agent %s has no signup in %s", agentID, platformExpID)
			}
			// A burst-only agent holds its whole entitlement in the burst column (see
			// domain.ApplyQuotaTier), so that is the column its unspent hours are measured in.
			if tier == domain.QuotaTierGuaranteed {
				allocated[agentID], used[agentID] = g, dim.UsedByAgent[agentID]
			} else {
				allocated[agentID], used[agentID] = b, dim.UsedBurstByAgent[agentID]
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
		}
		zeroQ := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, guaranteed, burst)
		if _, err := tx.Exec(ctx, zeroQ, platformExpID, cutAgentIDs); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: zero %s: %w", dim.ResourceType, err)
		}
		if len(survivorIDs) == 0 {
			continue
		}
		credit := domain.StageCredit(dim.Budget, dim.ReleaseFrac, allocated, used, len(survivorIDs))
		if credit <= 0 {
			continue
		}
		if err := creditIntoTier(ctx, tx, platformExpID, survivorIDs, guaranteed, burst, credit, 0); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: add %s: %w", dim.ResourceType, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE platform_experiments SET current_stage=$2, updated_at=NOW() WHERE id=$1`,
		platformExpID, stageIndex+1,
	); err != nil {
		return false, fmt.Errorf("platform_experiments_store.AdvanceStage: bump current_stage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.AdvanceStage: commit: %w", err)
	}
	return true, nil
}

// ListCutAgents returns agent IDs cut at any stage boundary of a platform experiment,
// oldest cut first.
func (s *PlatformExperimentsStore) ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error) {
	const q = `SELECT agent_id, stage_index FROM platform_experiment_cuts WHERE platform_experiment_id=$1 ORDER BY cut_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListCutAgents: %w", err)
	}
	defer rows.Close()
	var out []domain.AgentCut
	for rows.Next() {
		var c domain.AgentCut
		if err := rows.Scan(&c.AgentID, &c.StageIndex); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListStageAdvances returns every boundary this platform experiment has crossed, in order.
func (s *PlatformExperimentsStore) ListStageAdvances(ctx context.Context, platformExpID string) ([]domain.StageAdvance, error) {
	const q = `SELECT stage_index, advanced_at FROM platform_experiment_stage_advances WHERE platform_experiment_id=$1 ORDER BY stage_index ASC`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListStageAdvances: %w", err)
	}
	defer rows.Close()
	var out []domain.StageAdvance
	for rows.Next() {
		var a domain.StageAdvance
		if err := rows.Scan(&a.StageIndex, &a.AdvancedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// IsAgentCut returns true if the agent was cut at a stage boundary of the given platform
// experiment. A cut is terminal for the rest of the experiment.
func (s *PlatformExperimentsStore) IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `SELECT 1 FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2`
	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsAgentCut: %w", err)
	}
	return true, nil
}

// AgentCutAt returns when the agent was cut from the platform experiment, nil when it was not.
func (s *PlatformExperimentsStore) AgentCutAt(ctx context.Context, platformExpID, agentID string) (*time.Time, error) {
	var cutAt time.Time
	err := s.pool.pool.QueryRow(ctx,
		`SELECT cut_at FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID).Scan(&cutAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentCutAt: %w", err)
	}
	return &cutAt, nil
}

// ErrPlatformExperimentNotFound reports an id no platform experiment has.
var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// PlatformExperimentRepository returns the repository a platform experiment names, empty for
// none, the project it is in and whether it is closed. ErrPlatformExperimentNotFound when there
// is no such one.
func (s *PlatformExperimentsStore) PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, closed bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `
SELECT project_id, repository, closed_at IS NOT NULL FROM platform_experiments WHERE id = $1`, id).Scan(&projectID, &repository, &closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, ErrPlatformExperimentNotFound
	}
	if err != nil {
		return "", "", false, fmt.Errorf("platform_experiments_store.PlatformExperimentRepository: %w", err)
	}
	return projectID, repository, closed, nil
}
