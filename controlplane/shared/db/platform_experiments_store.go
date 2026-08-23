package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// InTxObservationTimeout bounds every metrics-store read taken while a quota transaction holds
// its advisory lock. Those reads are HTTP calls to a separate service, and an unbounded one stalls
// admission for the whole cluster behind the lock while its connection stays checked out of a
// small pool. Failing the transaction is recoverable — the caller retries on the next tick —
// where hanging on the lock is not.
const InTxObservationTimeout = 5 * time.Second

// ErrDonorInsufficientQuota is returned by FulfillDonationTx when, at commit time under row
// locks, the donor's guaranteed allocation minus its already-reserved usage cannot cover the
// requested transfer.
var ErrDonorInsufficientQuota = errors.New("donor has insufficient guaranteed quota")

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

// CreatePlatformExperiment inserts a new platform experiment.
func (s *PlatformExperimentsStore) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error {
	const q = `
INSERT INTO platform_experiments (id, name, description, budget_accelerator_hours, budget_cpu_core_hours, budget_ram_gb_hours, budget_storage_gb_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

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
	_, err = s.pool.pool.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.BudgetCPUCoreHours, pe.BudgetRAMGBHours, pe.BudgetStorageGBHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds,
		pe.StartsAt, pe.EndsAt, string(pe.Status),
		stages, 1,
		pe.CreatedAt, pe.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: %w", err)
	}
	return nil
}

// GetPlatformExperiment fetches a single platform experiment by ID.
func (s *PlatformExperimentsStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	const q = `
SELECT id, name, description, budget_accelerator_hours, budget_cpu_core_hours, budget_ram_gb_hours, budget_storage_gb_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, stages, current_stage, summary, created_at, updated_at
FROM platform_experiments
WHERE id = $1`

	pe := &domain.PlatformExperiment{}
	var status string
	var metricsRaw, stagesRaw []byte
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&pe.ID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.BudgetCPUCoreHours, &pe.BudgetRAMGBHours, &pe.BudgetStorageGBHours, &pe.MaxAgents,
		&metricsRaw, &pe.ReportIntervalSeconds,
		&pe.StartsAt, &pe.EndsAt, &status,
		&stagesRaw, &pe.CurrentStage, &pe.Summary,
		&pe.CreatedAt, &pe.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("platform_experiments_store.Get: %w", err)
	}
	if err := json.Unmarshal(metricsRaw, &pe.Metrics); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.Get: unmarshal metrics: %w", err)
	}
	if err := json.Unmarshal(stagesRaw, &pe.Stages); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.Get: unmarshal stages: %w", err)
	}
	pe.Status = domain.PlatformExperimentStatus(status)
	return pe, nil
}

// PlatformExperimentsFilter constrains ListPlatformExperiments/CountPlatformExperiments.
// Zero values are ignored.
type PlatformExperimentsFilter struct {
	Status string
	// Search matches against name/description, case-insensitively, substring match.
	Search string
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
		n++
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
	clauses, args := platformExperimentFilterClauses(PlatformExperimentsFilter{Status: string(status)})
	return s.queryPlatformExperiments(ctx, platformExperimentSelect(clauses, ""), args...)
}

func platformExperimentSelect(clauses []string, sort string) string {
	return `
SELECT pe.id, pe.name, pe.description, pe.budget_accelerator_hours, pe.budget_cpu_core_hours, pe.budget_ram_gb_hours, pe.budget_storage_gb_hours, pe.max_agents,
       pe.metrics, pe.report_interval_seconds,
       pe.starts_at, pe.ends_at, pe.status,
       pe.stages, pe.current_stage, pe.summary,
       pe.created_at, pe.updated_at,
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
		if err := rows.Scan(
			&pe.ID, &pe.Name, &pe.Description, &pe.BudgetAcceleratorHours, &pe.BudgetCPUCoreHours, &pe.BudgetRAMGBHours, &pe.BudgetStorageGBHours, &pe.MaxAgents,
			&metricsRaw, &pe.ReportIntervalSeconds,
			&pe.StartsAt, &pe.EndsAt, &status,
			&stagesRaw, &pe.CurrentStage, &pe.Summary,
			&pe.CreatedAt, &pe.UpdatedAt,
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

// UpdatePlatformExperimentStatus transitions a platform experiment to a new status.
func (s *PlatformExperimentsStore) UpdatePlatformExperimentStatus(ctx context.Context, id string, status domain.PlatformExperimentStatus) error {
	const q = `UPDATE platform_experiments SET status = $2, updated_at = NOW() WHERE id = $1`
	_, err := s.pool.pool.Exec(ctx, q, id, string(status))
	if err != nil {
		return fmt.Errorf("platform_experiments_store.UpdateStatus: %w", err)
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

// UpdatePlatformExperiment updates mutable fields of a platform experiment.
// UpdatePlatformExperiment writes pe's editable fields, but only if the row is still in
// expectedStatus — the service reads status, decides which fields are safe to change for that
// status, then calls here; without this compare-and-swap, a concurrent Start() could flip the row
// to running between that read and this write, and a since-locked field (budget, metrics, ...)
// computed under the stale "open" decision would land after admission already used the old value.
// A mismatch (0 rows affected) means status moved: fail fast rather than silently no-op.
func (s *PlatformExperimentsStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: marshal metrics: %w", err)
	}
	const q = `UPDATE platform_experiments
SET name=$2, description=$3, budget_accelerator_hours=$4, budget_cpu_core_hours=$5, budget_ram_gb_hours=$6, budget_storage_gb_hours=$7, max_agents=$8, metrics=$9,
    report_interval_seconds=$10, starts_at=$11, ends_at=$12, updated_at=NOW()
WHERE id=$1 AND status=$13`
	tag, err := s.pool.pool.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.BudgetCPUCoreHours, pe.BudgetRAMGBHours, pe.BudgetStorageGBHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds, pe.StartsAt, pe.EndsAt, expectedStatus,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("platform_experiments_store.Update: status changed concurrently, retry")
	}
	return nil
}

// StageRedistribution describes one resource dimension's move across a stage boundary: the cut
// agents' unspent allocation, plus the share of the platform budget withheld for the incoming
// stage, split evenly across survivors.
//
// UsedByAgent carries observed consumption per cut agent, which is metrics-store data and so
// cannot be read under the Postgres lock. That is safe in a way the allocation is not: observed
// usage only ever moves up, whereas an allocation can move in either direction (a donation), and
// deriving "unspent" from an allocation read before the lock mints or destroys hours relative to
// the platform budget. The allocation side is therefore read inside the transaction.
type StageRedistribution struct {
	ResourceType domain.ResourceType
	Budget       float64
	ReleaseFrac  float64
	UsedByAgent  map[string]float64
}

// StartParticipant is one signed-up agent and everything the allocation needs to know about it,
// resolved inside the start transaction.
type StartParticipant struct {
	AgentID     string
	AgentExists bool
	HasTop3     bool
}

// StartPlatformExperimentTx flips a platform experiment open->running and writes every
// participating agent's quota in one transaction. quotasFor is called with the signup set read
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

	tag, err := tx.Exec(ctx, `UPDATE platform_experiments SET status='running', updated_at=NOW() WHERE id=$1 AND status='open'`, id)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil, nil
	}

	// Signups joined against the agents they name and their top-3 history, all on this
	// transaction's own connection. The caller must not issue its own queries while this
	// transaction is open: several concurrent starts would each hold a connection and wait for
	// another from the same pool, which is a deadlock rather than a slow path.
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, a.id IS NOT NULL, EXISTS (SELECT 1 FROM experiment_top3 t WHERE t.agent_id = s.agent_id)
FROM experiment_signups s
LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1
ORDER BY s.agent_id`, id)
	if err != nil {
		return false, nil, fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
	}
	var participants []StartParticipant
	for rows.Next() {
		var p StartParticipant
		if err := rows.Scan(&p.AgentID, &p.AgentExists, &p.HasTop3); err != nil {
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
		// before the transaction: a donation committing in between would make "unspent" wrong in
		// either direction, minting or destroying hours against the platform budget.
		allocated := make(map[string]float64, len(cutAgentIDs))
		lockQ := fmt.Sprintf(
			`SELECT agent_id, %s FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id = ANY($2) ORDER BY agent_id FOR UPDATE`,
			guaranteed)
		rows, err := tx.Query(ctx, lockQ, platformExpID, cutAgentIDs)
		if err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
		}
		for rows.Next() {
			var agentID string
			var value float64
			if err := rows.Scan(&agentID, &value); err != nil {
				rows.Close()
				return false, fmt.Errorf("platform_experiments_store.AdvanceStage: scan cut allocation: %w", err)
			}
			allocated[agentID] = value
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
		}
		release := StageReleaseTotal(dim, allocated)

		zeroQ := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, guaranteed, burst)
		if _, err := tx.Exec(ctx, zeroQ, platformExpID, cutAgentIDs); err != nil {
			return false, fmt.Errorf("platform_experiments_store.AdvanceStage: zero %s: %w", dim.ResourceType, err)
		}
		if len(survivorIDs) == 0 || release <= 0 {
			continue
		}
		addQ := fmt.Sprintf(`UPDATE agent_quotas SET %[1]s = %[1]s + $3 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, guaranteed)
		if _, err := tx.Exec(ctx, addQ, platformExpID, survivorIDs, release/float64(len(survivorIDs))); err != nil {
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

// FulfillDonationTx performs a donation transfer as a single atomic, idempotent operation: it
// locks the donation row, verifies it is still "open", locks both quota rows, checks the donor's
// headroom (guaranteed allocation minus donorUsedGuaranteed, its metrics-store reservation total
// passed in by the caller), moves the amount, and flips the donation to "fulfilled" — all in one
// transaction under the same cross-replica advisory lock admission uses. Gating the transfer on
// the donation still being "open" inside the tx is what makes a retry safe: a second call after a
// crash sees "fulfilled" and returns (false, nil) instead of transferring again.
//
// Returns fulfilled=true when the transfer was applied, false when the donation was not open
// (already fulfilled/cancelled — a no-op, not an error). ErrDonorInsufficientQuota is returned if
// the donor cannot cover the amount.
// observe reads the donor's observed+reserved usage. It is called *inside* the transaction, once
// both quota rows are locked, for the same reason AdmitExperimentTx takes one: usage read before
// the lock is a snapshot a concurrent admission can invalidate, and the headroom check would then
// pass against hours already spoken for — over-committing the donor into exhaustion evictions
// that cannot be undone, since the transfer itself is irreversible.
func (s *PlatformExperimentsStore) FulfillDonationTx(ctx context.Context, donationID, donorAgentID, recipientAgentID, platformExpID string, resourceType domain.ResourceType, amount, burstAmount float64, observe func(context.Context) (*domain.AgentQuota, error)) (fulfilled bool, err error) {
	guaranteed, burst := resourceQuotaColumns(resourceType)

	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Both agents' locks, taken in a fixed order. Two donations in opposite directions between the
	// same pair (A->B and B->A) each locked their own donor first and then reached for the other's
	// row, which is a textbook deadlock and surfaced as a 500. Sorting makes the acquisition order
	// identical for every transaction touching this pair, so one simply waits.
	lockKeys := []string{donorAgentID + "/" + platformExpID, recipientAgentID + "/" + platformExpID}
	sort.Strings(lockKeys)
	for _, key := range lockKeys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: acquire lock: %w", err)
		}
	}

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM donation_requests WHERE id=$1 FOR UPDATE`, donationID).Scan(&status); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: lock donation: %w", err)
	}
	if status != "open" {
		return false, nil
	}

	// One statement locking both rows in agent_id order, for the same reason the advisory locks
	// are sorted: two row locks taken in caller-dependent order is the deadlock.
	lockQ := fmt.Sprintf(
		`SELECT agent_id, %s, %s FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id = ANY($2) ORDER BY agent_id FOR UPDATE`,
		guaranteed, burst)
	rows, err := tx.Query(ctx, lockQ, platformExpID, []string{donorAgentID, recipientAgentID})
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: lock quota rows: %w", err)
	}
	var donorGuaranteed, donorBurst float64
	var lockedDonor, lockedRecipient bool
	for rows.Next() {
		var agentID string
		var g, b float64
		if err := rows.Scan(&agentID, &g, &b); err != nil {
			rows.Close()
			return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: scan quota row: %w", err)
		}
		switch agentID {
		case donorAgentID:
			donorGuaranteed, donorBurst = g, b
			lockedDonor = true
		case recipientAgentID:
			lockedRecipient = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: lock quota rows: %w", err)
	}
	if !lockedDonor || !lockedRecipient {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: donor or recipient has no quota row in %s", platformExpID)
	}

	observedCtx, cancelObserved := context.WithTimeout(ctx, InTxObservationTimeout)
	observed, err := observe(observedCtx)
	cancelObserved()
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: observed usage: %w", err)
	}
	if observed == nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: no observed usage for donor %s", donorAgentID)
	}
	if donorGuaranteed-observed.UsedGuaranteedAccH < amount {
		return false, ErrDonorInsufficientQuota
	}

	// Burst moves with the guaranteed hours it is derived from (burst = guaranteed x
	// BurstFraction, see domain.AllocateQuota). Moving guaranteed alone leaves the donor holding
	// burst headroom for hours it gave away and the recipient with none for hours it received.
	//
	// One figure debited and credited, never two: a donor row written before that derivation
	// existed can hold less burst than the transfer implies, and flooring only the debit would
	// credit the recipient hours the donor never gave up — minting allocation instead of moving
	// it. Capping the transfer itself keeps the two sides equal by construction.
	movedBurst := burstAmount
	if movedBurst > donorBurst {
		movedBurst = donorBurst
	}
	if movedBurst < 0 {
		movedBurst = 0
	}
	debitQ := fmt.Sprintf(`UPDATE agent_quotas SET %[1]s = %[1]s - $3, %[2]s = %[2]s - $4 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed, burst)
	if _, err := tx.Exec(ctx, debitQ, donorAgentID, platformExpID, amount, movedBurst); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: debit donor: %w", err)
	}
	creditQ := fmt.Sprintf(`UPDATE agent_quotas SET %[1]s = %[1]s + $3, %[2]s = %[2]s + $4 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed, burst)
	if _, err := tx.Exec(ctx, creditQ, recipientAgentID, platformExpID, amount, movedBurst); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: credit recipient: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE donation_requests SET status='fulfilled', updated_at=now() WHERE id=$1`, donationID); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: mark fulfilled: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.FulfillDonationTx: commit: %w", err)
	}
	return true, nil
}
