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

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// InTxObservationTimeout bounds every metrics-store read taken while a quota transaction holds
// its advisory lock. Those reads are HTTP calls to a separate service, and an unbounded one stalls
// admission for the whole cluster behind the lock while its connection stays checked out of a
// small pool. Failing the transaction is recoverable — the caller retries on the next tick —
// where hanging on the lock is not.
const InTxObservationTimeout = 5 * time.Second

// ---- platform_experiments ----

// coordinatorColumns are what a platform experiment says of the coordinator the platform runs
// for it, read from platform_experiments as pe joined to its machine as ci (coordinatorInstanceJoin).
var coordinatorColumns = `pe.coordinator_hosted, COALESCE(pe.coordinator_model, ''),
       COALESCE(ci.name, ''), COALESCE(ci.restarts, 0), COALESCE(ci.renewals, 0), COALESCE(ci.stopped, false), ci.exited_at, COALESCE(ci.exit_log, ''),
       ci.seen_at, ci.asked_at, COALESCE(ci.launch, ''), COALESCE(ci.state, ''), ci.progress_at, COALESCE(ci.end_reason, ''),
       ` + domain.InstanceReleasedSQL("pe.closed_at")

// CreatePlatformExperiment stores pe and signs up the agents it is created with, the ones of
// create made first, or stores nothing. The limits a signup holds are held here too
// (ErrMaxAgentsReached, ErrMaxHostedReached).
func (s *Store) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	return s.write(ctx, func(tx *Write) error {
		if err := insertPlatformExperiment(ctx, tx, pe); err != nil {
			return err
		}
		if len(signups) > 0 {
			if _, err := enrolTx(ctx, tx, pe.ID, create, signups, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// ProjectOrg is the org a project belongs to: ErrProjectNotFound for no such project.
func (s *Store) ProjectOrg(ctx context.Context, projectID string) (string, error) {
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

// optionalTime is t as a nullable column holds it: NULL for the zero time.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// insertPlatformExperiment inserts pe, whose coordinator runs the built-in coordinator type, and
// locks the agent type of each entry of its researchers (poolTypesTx).
func insertPlatformExperiment(ctx context.Context, tx *Write, pe *domain.PlatformExperiment) error {
	const q = `
INSERT INTO platform_experiments (id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, created_at, updated_at, max_concurrent_accelerators, default_quota_tier, coordinator_hosted, coordinator_model, max_hosted, repository, repository_url, agent_image, goal, created_by_kind, created_by_id, duration_hours, repository_account, continues, researchers,
                                  coordinator_agent_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, NULLIF($19, ''), $20, $21, $22, $23, $24, $25, $26, $27, $28, NULLIF($29, ''), $30,
        '` + domain.CoordinatorFlavor + `')`

	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: marshal metrics: %w", err)
	}
	researchers, err := poolJSON(pe.Researchers)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: marshal researchers: %w", err)
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
	_, err = tx.Exec(ctx, q,
		pe.ID, pe.ProjectID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds,
		optionalTime(pe.StartsAt), pe.ClosesAt(), string(pe.Status),
		stages, 1,
		pe.CreatedAt, pe.UpdatedAt, pe.MaxConcurrentAccelerators, string(pe.DefaultQuotaTier),
		pe.CoordinatorHosted, coordinator.Model, pe.MaxHosted,
		pe.Repository, pe.RepositoryURL, pe.AgentImage,
		pe.Goal, string(createdBy.Kind), createdBy.ID, pe.DurationHours, pe.RepositoryAccount, pe.Continues, researchers,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: %w", err)
	}
	if err := poolTypesTx(ctx, tx, pe.ID, pe.Researchers); err != nil {
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
// every other closed one of its project, not removed, that works on the same repository and ran: its
// researchers started or it declared a baseline, so a draft closed before either is none. The run
// its continues names, and each one that run carried on, is among them, since only a run that
// declared a baseline can be continued.
const priorRuns = `
SELECT prior.id FROM platform_experiments run
JOIN platform_experiments prior ON prior.project_id = run.project_id AND prior.repository = run.repository
WHERE run.id = $1 AND run.repository <> '' AND prior.id <> run.id AND prior.status = '` + string(domain.PlatformExpClosed) + `'
  AND (prior.started_at IS NOT NULL OR prior.baseline_job_id <> '') AND prior.removed_at IS NULL
ORDER BY prior.closed_at DESC, prior.id`

// PriorRuns is the ids of the prior runs of platform experiment id, empty when it has none.
func (s *Store) PriorRuns(ctx context.Context, id string) ([]string, error) {
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
func (s *Store) RunsToRank(ctx context.Context, ids []string) ([]RunToRank, error) {
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
func (s *Store) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	q := `
SELECT pe.id, pe.project_id, pe.name, pe.description, pe.budget_accelerator_hours, pe.max_agents, pe.researchers, pe.metrics, pe.report_interval_seconds, pe.starts_at, pe.ends_at, pe.status, pe.stages, pe.current_stage, ` + summarySQL + `, pe.created_at, pe.updated_at, pe.max_concurrent_accelerators, pe.default_quota_tier, pe.shared,
       ` + coordinatorColumns + `, pe.max_hosted, pe.repository, pe.repository_url, pe.agent_image, pe.goal, pe.goal_version, COALESCE(pe.continues, ''), pe.created_by_kind, pe.created_by_id, pe.proposal, pe.proposed_at, pe.declined_reason,
       pe.baseline_job_id, pe.job_estimated_duration_hours, pe.closed_at, pe.close_reason, pe.closed_by_kind, pe.closed_by_id, pe.close_message, pe.removed_at, pe.repository_error, pe.repository_account, COALESCE(pe.researchers_held, ''),
       pe.duration_hours, pe.started_at, pe.planner_ends_at, pe.accepts_at, COALESCE(ci.activity, ''), ci.activity_at,
       pe.accepted_at, pe.accepted_by_kind, pe.accepted_by_id, pe.accepted_answers,
       (SELECT pi.activity FROM experiment_signups planner JOIN hosted_instances pi USING (platform_experiment_id, agent_id) WHERE ` + plannerOfDraft + `),
       (SELECT pi.activity_at FROM experiment_signups planner JOIN hosted_instances pi USING (platform_experiment_id, agent_id) WHERE ` + plannerOfDraft + `)
FROM platform_experiments pe` + coordinatorInstanceJoin + `
WHERE pe.id = $1`

	pe := &domain.PlatformExperiment{}
	var status string
	var metricsRaw, stagesRaw []byte
	var coordinator domain.HostedCoordinator
	var createdBy, closedBy, acceptedBy domain.Actor
	var proposal, acceptedAnswers []byte
	var closesAt time.Time
	var startsAt, startedAt, answeredAt, plannerEndsAt, acceptsAt, coordinatorActivityAt, plannerActivityAt *time.Time
	var coordinatorActivity string
	var plannerActivity *string
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&pe.ID, &pe.ProjectID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.MaxAgents, &pe.Researchers,
		&metricsRaw, &pe.ReportIntervalSeconds,
		&startsAt, &closesAt, &status,
		&stagesRaw, &pe.CurrentStage, &pe.Summary,
		&pe.CreatedAt, &pe.UpdatedAt, &pe.MaxConcurrentAccelerators, &pe.DefaultQuotaTier, &pe.Shared,
		&pe.CoordinatorHosted, &coordinator.Model, &coordinator.Instance.Name, &coordinator.Instance.Restarts,
		&coordinator.Instance.Renewals, &coordinator.Instance.Stopped, &coordinator.Instance.ExitedAt, &coordinator.Instance.ExitLog,
		&coordinator.Instance.SeenAt, &coordinator.Instance.AskedAt, &coordinator.Instance.Launch, &coordinator.Instance.Reported, &coordinator.Instance.ProgressAt, &coordinator.Instance.EndReason, &coordinator.Instance.Released, &pe.MaxHosted, &pe.Repository, &pe.RepositoryURL, &pe.AgentImage,
		&pe.Goal, &pe.GoalVersion, &pe.Continues, &createdBy.Kind, &createdBy.ID, &proposal, &answeredAt, &pe.DeclinedReason,
		&pe.BaselineJobID, &pe.JobEstimatedDurationHours, &pe.ClosedAt, &pe.CloseReason, &closedBy.Kind, &closedBy.ID, &pe.CloseMessage, &pe.RemovedAt, &pe.RepositoryError, &pe.RepositoryAccount, &pe.ResearchersHeld,
		&pe.DurationHours, &startedAt, &plannerEndsAt, &acceptsAt, &coordinatorActivity, &coordinatorActivityAt,
		&pe.AcceptedAt, &acceptedBy.Kind, &acceptedBy.ID, &acceptedAnswers,
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
	pe.Actor = &createdBy
	if closedBy.Kind != "" {
		pe.ClosedBy = &closedBy
	}
	if len(proposal) > 0 {
		pe.Proposal = proposal
	}
	if acceptedBy.Kind != "" {
		pe.AcceptedBy = &acceptedBy
	}
	if len(acceptedAnswers) > 0 {
		if err := json.Unmarshal(acceptedAnswers, &pe.AcceptedAnswers); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.Get: unmarshal accepted answers: %w", err)
		}
	}
	pe.SetAnswered(answeredAt, len(proposal) > 0)
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
	if startsAt != nil {
		pe.StartsAt = *startsAt
	}
	if startedAt != nil {
		pe.StartedAt = *startedAt
	}
	pe.SetClosesAt(closesAt)
	return pe, nil
}

// PlatformExperimentExists says whether there is a platform experiment of that id, for a
// service that needs no more of it.
func (s *Store) PlatformExperimentExists(ctx context.Context, id string) (bool, error) {
	var found bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_experiments WHERE id = $1)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("platform_experiments_store.PlatformExperimentExists: %w", err)
	}
	return found, nil
}

// GetPlatformExperimentMetrics is a narrow read of just the declared MetricDefinitions, used to
// tell a ranking metric's direction apart from a constraint or attribute one (registry.Service's
// underperformance check) without pulling the whole row through that service's Store interface.
func (s *Store) GetPlatformExperimentMetrics(ctx context.Context, id string) (metrics []domain.MetricDefinition, found bool, err error) {
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
	"starts_at":  "COALESCE(pe.starts_at, '-infinity')",
	"name":       "pe.name",
	"status":     "array_position(ARRAY['draft', 'open', 'running', 'closed'], pe.status)",
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

// platformExperimentFilterClauses is the filter of a listing, which a removed platform experiment
// is in for nobody.
func platformExperimentFilterClauses(filter PlatformExperimentsFilter) ([]string, []any) {
	clauses := []string{"pe.removed_at IS NULL"}
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
func (s *Store) ListPlatformExperiments(ctx context.Context, filter PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error) {
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
// paginated query. A removed one is among them: its jobs are evicted and its spend settled as
// any other's.
func (s *Store) ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	return s.queryPlatformExperiments(ctx, platformExperimentSelect([]string{"pe.status = $1"}, ""), string(status))
}

func platformExperimentSelect(clauses []string, sort string) string {
	return `
SELECT pe.id, pe.project_id, pe.name, pe.description, pe.budget_accelerator_hours, pe.max_agents, pe.researchers,
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

func (s *Store) queryPlatformExperiments(ctx context.Context, q string, args ...any) ([]*domain.PlatformExperiment, error) {
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
		var startsAt, startedAt, answeredAt *time.Time
		var proposed bool
		if err := rows.Scan(
			&pe.ID, &pe.ProjectID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.MaxAgents, &pe.Researchers,
			&metricsRaw, &pe.ReportIntervalSeconds,
			&startsAt, &closesAt, &status,
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
		if startsAt != nil {
			pe.StartsAt = *startsAt
		}
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
func (s *Store) CountPlatformExperiments(ctx context.Context, filter PlatformExperimentsFilter) (int, error) {
	clauses, args := platformExperimentFilterClauses(filter)
	q := `SELECT COUNT(*) FROM platform_experiments pe WHERE ` + strings.Join(clauses, " AND ")
	var total int
	if err := s.pool.pool.QueryRow(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Count: %w", err)
	}
	return total, nil
}

// SetPlatformExperimentSummary records the operator's narrative verdict on a run. Separate from
// UpdatePlatformExperiment because it is the one field that is written *after* the experiment
// closes, when everything else is frozen. ErrPlatformExperimentNotFound for an id none has.
func (s *Store) SetPlatformExperimentSummary(ctx context.Context, id, summary string) error {
	const q = `UPDATE platform_experiments
SET summary = $2, summary_at = CASE WHEN summary IS DISTINCT FROM $2 THEN NOW() ELSE summary_at END, updated_at = NOW()
WHERE id = $1`
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
// recorded is announced as platform_experiment.status with the reason as its detail, and one
// that is the same writes nothing. A closed one is told nothing.
func (s *Store) TellCoordinator(ctx context.Context, id, reason string) error {
	const q = `UPDATE platform_experiments SET coordinator_reason = NULLIF($2, '')
WHERE id = $1 AND status <> 'closed' AND coordinator_reason IS DISTINCT FROM NULLIF($2, '')`
	if err := s.writeRunStatusDetail(ctx, q, id, reason); err != nil {
		return fmt.Errorf("platform_experiments_store.TellCoordinator: %w", err)
	}
	return nil
}

// writeRunStatusDetail runs q, a write of what the status of the platform experiment id says
// beside itself, to detail, and announces the status with a detail that is not empty.
func (s *Store) writeRunStatusDetail(ctx context.Context, q, id, detail string) error {
	return s.write(ctx, func(w *Write) error {
		var status string
		err := w.QueryRow(ctx, q+` RETURNING status`, id, detail).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if detail != "" {
			w.runChange(id, domain.EventPlatformExperimentStatus, status, detail)
		}
		return nil
	})
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
//
// max_agents and the researchers are held to the researchers at work under the row's lock
// (setResearchersTx): a running platform experiment loses the newest researchers above a lowered
// max_agents, and otherwise a *PoolBelowAtWorkError refuses them and nothing is written.
// researchers left nil stay as they are, capped to max_agents, so a patch that does not change
// them never writes back a pool a cut lowered since it read.
func (s *Store) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus, researchers domain.ResearcherPool) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: marshal metrics: %w", err)
	}
	return s.write(ctx, func(tx *Write) error {
		if err := writePlatformExperiment(ctx, tx, pe, metrics, expectedStatus, ""); err != nil {
			return fmt.Errorf("platform_experiments_store.Update: %w", err)
		}
		if err := setResearchersTx(ctx, tx, pe.ID, pe.MaxAgents, researchers); err != nil {
			return fmt.Errorf("platform_experiments_store.Update: %w", err)
		}
		if expectedStatus == domain.PlatformExpOpen {
			if err := reallocateBaselineQuotas(ctx, tx, pe); err != nil {
				return fmt.Errorf("platform_experiments_store.Update: %w", err)
			}
		}
		return nil
	})
}

// writePlatformExperiment writes the patchable fields of pe while it is still in expectedStatus
// and, when baseline is not empty, still has that baseline.
func writePlatformExperiment(ctx context.Context, tx *Write, pe *domain.PlatformExperiment, metrics []byte, expectedStatus domain.PlatformExperimentStatus, baseline string) error {
	const q = `UPDATE platform_experiments pe
SET name=$2, description=$3, budget_accelerator_hours=$4, max_agents=$5, metrics=$6,
    report_interval_seconds=$7, starts_at=$8, ends_at=$9, updated_at=NOW(),
    max_concurrent_accelerators=$11, baseline_job_id=$12, job_estimated_duration_hours=$13, duration_hours=$14
FROM (SELECT description, duration_hours FROM platform_experiments WHERE id = $1 FOR UPDATE) old
WHERE pe.id=$1 AND pe.status=$10 AND ($15 = '' OR pe.baseline_job_id = $15)
RETURNING pe.description IS DISTINCT FROM old.description OR pe.duration_hours IS DISTINCT FROM old.duration_hours`
	var briefMoved bool
	err := tx.QueryRow(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds, optionalTime(pe.StartsAt), pe.ClosesAt(),
		expectedStatus,
		pe.MaxConcurrentAccelerators, pe.BaselineJobID, pe.JobEstimatedDurationHours, pe.DurationHours, baseline,
	).Scan(&briefMoved)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("status or baseline changed concurrently, retry")
	}
	if err != nil {
		return err
	}
	if briefMoved {
		tx.runChange(pe.ID, domain.EventPlatformExperimentDescription, "", "")
	}
	return nil
}

// ErrPlatformExperimentNotFound reports an id no platform experiment has.
var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// PlatformExperimentRepository returns the repository a platform experiment names, empty for
// none, the project it is in, whether it is closed and whether it is still a draft.
// ErrPlatformExperimentNotFound when there is no such one.
func (s *Store) PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, closed, draft bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `
SELECT project_id, repository, closed_at IS NOT NULL, status = $2 FROM platform_experiments WHERE id = $1`, id, string(domain.PlatformExpDraft)).Scan(&projectID, &repository, &closed, &draft)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, false, ErrPlatformExperimentNotFound
	}
	if err != nil {
		return "", "", false, false, fmt.Errorf("platform_experiments_store.PlatformExperimentRepository: %w", err)
	}
	return projectID, repository, closed, draft, nil
}
