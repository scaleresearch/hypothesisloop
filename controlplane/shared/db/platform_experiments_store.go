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
       pe.coordinator_instance_restarts, pe.coordinator_instance_stopped, pe.coordinator_instance_exited_at, pe.coordinator_instance_exit_log,
       pe.coordinator_instance_seen_at, pe.coordinator_instance_asked_at, pe.coordinator_instance_launch,
       pe.coordinator_instance_state, pe.coordinator_instance_progress_at, pe.coordinator_instance_end_reason,
       ` + domain.InstanceReleasedSQL("pe.closed_at")

func (s *PlatformExperimentsStore) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error {
	return insertPlatformExperiment(ctx, s.pool.pool, pe)
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
INSERT INTO platform_experiments (id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, created_at, updated_at, max_concurrent_accelerators, default_quota_tier, coordinator_hosted, coordinator_launcher, coordinator_model, max_hosted, repository, repository_url, agent_image, goal, created_by_kind, created_by_id, duration_hours)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)`

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
		pe.Goal, string(createdBy.Kind), createdBy.ID, pe.DurationHours,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: %w", err)
	}
	return nil
}

// GetPlatformExperiment fetches a single platform experiment by ID.
func (s *PlatformExperimentsStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	q := `
SELECT id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, summary, created_at, updated_at, max_concurrent_accelerators, default_quota_tier, pe.shared,
       ` + coordinatorColumns + `, pe.max_hosted, pe.repository, pe.repository_url, pe.agent_image, pe.goal, pe.created_by_kind, pe.created_by_id, pe.proposal, pe.proposed_at, pe.declined_reason,
       pe.baseline_job_id, pe.job_estimated_duration_hours, pe.closed_at, pe.close_reason, pe.closed_by_kind, pe.closed_by_id,
       pe.duration_hours, pe.started_at
FROM platform_experiments pe
WHERE id = $1`

	pe := &domain.PlatformExperiment{}
	var status string
	var metricsRaw, stagesRaw []byte
	var coordinator domain.HostedCoordinator
	var createdBy, closedBy domain.Actor
	var proposal []byte
	var closesAt time.Time
	var startedAt, answeredAt *time.Time
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&pe.ID, &pe.ProjectID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.MaxAgents,
		&metricsRaw, &pe.ReportIntervalSeconds,
		&pe.StartsAt, &closesAt, &status,
		&stagesRaw, &pe.CurrentStage, &pe.Summary,
		&pe.CreatedAt, &pe.UpdatedAt, &pe.MaxConcurrentAccelerators, &pe.DefaultQuotaTier, &pe.Shared,
		&pe.CoordinatorHosted, &coordinator.Launcher, &coordinator.Model, &coordinator.Instance.Name, &coordinator.Instance.Restarts,
		&coordinator.Instance.Stopped, &coordinator.Instance.ExitedAt, &coordinator.Instance.ExitLog,
		&coordinator.Instance.SeenAt, &coordinator.Instance.AskedAt, &coordinator.Instance.Launch, &coordinator.Instance.Reported, &coordinator.Instance.ProgressAt, &coordinator.Instance.EndReason, &coordinator.Instance.Released, &pe.MaxHosted, &pe.Repository, &pe.RepositoryURL, &pe.AgentImage,
		&pe.Goal, &createdBy.Kind, &createdBy.ID, &proposal, &answeredAt, &pe.DeclinedReason,
		&pe.BaselineJobID, &pe.JobEstimatedDurationHours, &pe.ClosedAt, &pe.CloseReason, &closedBy.Kind, &closedBy.ID,
		&pe.DurationHours, &startedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("platform_experiments_store.Get: %w", err)
	}
	if pe.CoordinatorHosted {
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

// defaultPlatformExperimentListLimit and maxPlatformExperimentListLimit bound every
// ListPlatformExperiments read the same way ListAgents/ListHypotheses bound their own — see
// agents_store.go/hypotheses_store.go. Limit<=0 must not mean "unbounded".
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultPlatformExperimentListLimit = 20
	maxPlatformExperimentListLimit     = 200
)

// ListPlatformExperiments returns platform experiments matching the given filter. filter.Limit
// is defaulted and clamped to [1, maxPlatformExperimentListLimit]; limit<=0 uses the default.
func (s *PlatformExperimentsStore) ListPlatformExperiments(ctx context.Context, filter PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error) {
	if filter.Limit <= 0 {
		filter.Limit = defaultPlatformExperimentListLimit
	} else if filter.Limit > maxPlatformExperimentListLimit {
		filter.Limit = maxPlatformExperimentListLimit
	}

	clauses, args := platformExperimentFilterClauses(filter)
	n := len(args) + 1

	q := platformExperimentSelect(clauses, filter.Sort)

	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, filter.Limit)
		n++
	}
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
       pe.stages, pe.current_stage, pe.summary,
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
// asked, nil when the platform closes it at its end time. One already closed keeps what it has.
func (s *PlatformExperimentsStore) ClosePlatformExperiment(ctx context.Context, id string, by *domain.Actor) error {
	reason, actor := "", domain.Actor{}
	if by != nil {
		reason, actor = string(domain.CloseByRequest), *by
	}
	// What the platform closes is a run at its end, a draft nobody proposed for inside the
	// planning limit or whose answer no person took up, or an open one that was not running by
	// its baseline deadline.
	const q = `
UPDATE platform_experiments
SET status = 'closed', updated_at = NOW(), closed_at = NOW(),
    close_reason = CASE WHEN $2 <> '' THEN $2
                        WHEN status = 'draft' AND proposed_at IS NULL THEN '` + string(domain.ClosePlanningLimitReached) + `'
                        WHEN status = 'draft' THEN '` + string(domain.CloseProposalExpired) + `'
                        WHEN status = 'open' THEN '` + string(domain.CloseBaselineLimitReached) + `'
                        ELSE '` + string(domain.CloseEndTimeReached) + `' END,
    closed_by_kind = $3, closed_by_id = $4
WHERE id = $1 AND status <> 'closed'`
	if _, err := s.pool.pool.Exec(ctx, q, id, reason, string(actor.Kind), actor.ID); err != nil {
		return fmt.Errorf("platform_experiments_store.Close: %w", err)
	}
	return nil
}

// SetPlatformExperimentSummary records the operator's narrative verdict on a run. Separate from
// UpdatePlatformExperiment because it is the one field that is written *after* the experiment
// closes, when everything else is frozen.
func (s *PlatformExperimentsStore) SetPlatformExperimentSummary(ctx context.Context, id, summary string) error {
	const q = `UPDATE platform_experiments SET summary = $2, updated_at = NOW() WHERE id = $1`
	tag, err := s.pool.pool.Exec(ctx, q, id, summary)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.SetSummary: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("platform_experiments_store.SetSummary: not_found")
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
// The quota of an agent of role baseline is a share of the budget over max_agents, so in an open
// experiment, where both change, it is written again from the row as patched in the same
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
	const q = `UPDATE platform_experiments
SET name=$2, description=$3, budget_accelerator_hours=$4, max_agents=$5, metrics=$6,
    report_interval_seconds=$7, starts_at=$8, ends_at=$9, updated_at=NOW(),
    max_concurrent_accelerators=$11, baseline_job_id=$12, job_estimated_duration_hours=$13, duration_hours=$14
WHERE id=$1 AND status=$10`
	tag, err := tx.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds, pe.StartsAt, pe.ClosesAt(),
		expectedStatus,
		pe.MaxConcurrentAccelerators, pe.BaselineJobID, pe.JobEstimatedDurationHours, pe.DurationHours,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("platform_experiments_store.Update: status changed concurrently, retry")
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

// reallocateBaselineQuotas writes the quota of every agent of role baseline of pe again, from
// pe's budget and max_agents.
func reallocateBaselineQuotas(ctx context.Context, tx pgx.Tx, pe *domain.PlatformExperiment) error {
	if len(pe.Stages) == 0 {
		return fmt.Errorf("platform experiment %s has no stages to take the first one's share from", pe.ID)
	}
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
		if err := allocateBaselineQuota(ctx, tx, pe.ID, h.AgentID, h.Tier, pe.BudgetAcceleratorHours, pe.Stages[0], pe.MaxAgents, pe.DefaultQuotaTier); err != nil {
			return err
		}
	}
	return nil
}

// StageRedistribution describes one resource dimension's move across a stage boundary: the cut
// agents' unspent allocation, plus the share of the platform budget withheld for the incoming
// stage, split evenly across survivors.
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
	AgentID     string
	AgentExists bool
	HasTop3     bool
	// QuotaTierOverride is what domain.ResolveQuotaTier needs to decide which column this
	// participant's share lands in. Read here, inside the start transaction, because the signup
	// set is: resolving the tier from a separate query could pair one transaction's participants
	// with another's overrides.
	QuotaTierOverride domain.QuotaTier
}

// StartPlatformExperimentTx flips a platform experiment open->running and writes every
// participating agent's quota in one transaction. The run's clock starts here: started_at is
// now and ends_at duration_hours later, so nothing of the time it was open is taken from it. quotasFor is called with the signup set read
// inside that transaction; returning an error rolls the whole thing back, leaving the experiment
// open and the call safe to retry.
//
// Atomic because the two halves used to be separate: the signup list was read, quotas were
// written, and only then was the status flipped. An agent signing up in that window was durably
// signed up with no quota row and no path to ever get one. Flipping the status inside the same
// transaction closes the window from the other side too — Signup only inserts while the
// experiment is still open, so a signup either lands before this transaction and is seen by it,
// or fails.
//
// Returns started=false when the experiment was not open.
func (s *PlatformExperimentsStore) StartPlatformExperimentTx(ctx context.Context, id string, quotasFor func(participants []StartParticipant) ([]*domain.AgentQuota, error)) (bool, []*domain.AgentQuota, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
UPDATE platform_experiments
SET status='running', started_at=NOW(), ends_at=NOW() + make_interval(secs => duration_hours * 3600), updated_at=NOW()
WHERE id=$1 AND status='open'`, id)
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
SELECT s.agent_id, a.id IS NOT NULL, EXISTS (SELECT 1 FROM experiment_top3 t WHERE t.agent_id = s.agent_id),
       s.quota_tier
FROM experiment_signups s
LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1 AND s.role NOT IN ('`+string(domain.SignupRoleBaseline)+`', '`+string(domain.SignupRolePlanner)+`')
ORDER BY s.agent_id`, id)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
	}
	var participants []StartParticipant
	for rows.Next() {
		var p StartParticipant
		if err := rows.Scan(&p.AgentID, &p.AgentExists, &p.HasTop3, &p.QuotaTierOverride); err != nil {
			rows.Close()
			return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: scan signup: %w", err)
		}
		participants = append(participants, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
	}

	quotas, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
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

// StageReleaseTotal is what returns to the survivors at a boundary: the share of the platform
// budget withheld for the incoming stage, plus whatever the cut agents did not spend. An agent
// that overspent its guarantee contributes nothing rather than a negative.
func StageReleaseTotal(dim StageRedistribution, allocatedByAgent map[string]float64) float64 {
	release := dim.Budget * dim.ReleaseFrac
	for agentID, allocated := range allocatedByAgent {
		if unspent := allocated - dim.UsedByAgent[agentID]; unspent > 0 {
			release += unspent
		}
	}
	return release
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
		dim.UsedByAgent = used
		release := StageReleaseTotal(dim, allocated)

		zeroQ := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, guaranteed, burst)
		if _, err := tx.Exec(ctx, zeroQ, platformExpID, cutAgentIDs); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: zero %s: %w", dim.ResourceType, err)
		}
		if len(survivorIDs) == 0 || release <= 0 {
			continue
		}
		if err := creditIntoTier(ctx, tx, platformExpID, survivorIDs, guaranteed, burst,
			domain.RoundHours(release/float64(len(survivorIDs))), 0); err != nil {
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
// none, and the project it is in. ErrPlatformExperimentNotFound when there is no such one.
func (s *PlatformExperimentsStore) PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, err error) {
	err = s.pool.pool.QueryRow(ctx, `
SELECT project_id, repository FROM platform_experiments WHERE id = $1`, id).Scan(&projectID, &repository)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrPlatformExperimentNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("platform_experiments_store.PlatformExperimentRepository: %w", err)
	}
	return projectID, repository, nil
}
