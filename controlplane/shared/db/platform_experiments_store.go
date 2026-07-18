package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
INSERT INTO platform_experiments (id, name, description, budget_t4_hours, budget_cpu_core_hours, budget_ram_gb_hours, budget_storage_gb_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, phase, phase2_triggered_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Create: marshal metrics: %w", err)
	}
	phase := pe.Phase
	if phase == 0 {
		phase = 1
	}
	_, err = s.pool.pool.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetT4Hours, pe.BudgetCPUCoreHours, pe.BudgetRAMGBHours, pe.BudgetStorageGBHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds,
		pe.StartsAt, pe.EndsAt, string(pe.Status),
		phase, pe.Phase2TriggeredAt,
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
SELECT id, name, description, budget_t4_hours, budget_cpu_core_hours, budget_ram_gb_hours, budget_storage_gb_hours, max_agents, metrics, report_interval_seconds, starts_at, ends_at, status, phase, phase2_triggered_at, created_at, updated_at
FROM platform_experiments
WHERE id = $1`

	pe := &domain.PlatformExperiment{}
	var status string
	var metricsRaw []byte
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&pe.ID, &pe.Name, &pe.Description, &pe.BudgetT4Hours, &pe.BudgetCPUCoreHours, &pe.BudgetRAMGBHours, &pe.BudgetStorageGBHours, &pe.MaxAgents,
		&metricsRaw, &pe.ReportIntervalSeconds,
		&pe.StartsAt, &pe.EndsAt, &status,
		&pe.Phase, &pe.Phase2TriggeredAt,
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
	pe.Status = domain.PlatformExperimentStatus(status)
	if pe.Phase == 0 {
		pe.Phase = 1
	}
	return pe, nil
}

// ListPlatformExperiments returns all platform experiments, optionally filtered by status.
// Pass empty string to return all.
func (s *PlatformExperimentsStore) ListPlatformExperiments(ctx context.Context, statusFilter string) ([]*domain.PlatformExperiment, error) {
	var (
		q    string
		args []any
	)
	base := `
SELECT pe.id, pe.name, pe.description, pe.budget_t4_hours, pe.budget_cpu_core_hours, pe.budget_ram_gb_hours, pe.budget_storage_gb_hours, pe.max_agents,
       pe.metrics, pe.report_interval_seconds,
       pe.starts_at, pe.ends_at, pe.status,
       pe.phase, pe.phase2_triggered_at,
       pe.created_at, pe.updated_at,
       COUNT(es.agent_id) AS signup_count
FROM platform_experiments pe
LEFT JOIN experiment_signups es ON es.platform_experiment_id = pe.id`

	if statusFilter != "" {
		q = base + `
WHERE pe.status = $1
GROUP BY pe.id
ORDER BY pe.created_at DESC`
		args = []any{statusFilter}
	} else {
		q = base + `
GROUP BY pe.id
ORDER BY pe.created_at DESC`
	}

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.List: %w", err)
	}
	defer rows.Close()

	var out []*domain.PlatformExperiment
	for rows.Next() {
		pe := &domain.PlatformExperiment{}
		var status string
		var metricsRaw []byte
		if err := rows.Scan(
			&pe.ID, &pe.Name, &pe.Description, &pe.BudgetT4Hours, &pe.BudgetCPUCoreHours, &pe.BudgetRAMGBHours, &pe.BudgetStorageGBHours, &pe.MaxAgents,
			&metricsRaw, &pe.ReportIntervalSeconds,
			&pe.StartsAt, &pe.EndsAt, &status,
			&pe.Phase, &pe.Phase2TriggeredAt,
			&pe.CreatedAt, &pe.UpdatedAt,
			&pe.SignupCount,
		); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.List: scan: %w", err)
		}
		if err := json.Unmarshal(metricsRaw, &pe.Metrics); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.List: unmarshal metrics: %w", err)
		}
		pe.Status = domain.PlatformExperimentStatus(status)
		if pe.Phase == 0 {
			pe.Phase = 1
		}
		out = append(out, pe)
	}
	return out, rows.Err()
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

// UpdatePlatformExperiment updates mutable fields of a platform experiment.
func (s *PlatformExperimentsStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: marshal metrics: %w", err)
	}
	const q = `UPDATE platform_experiments
SET name=$2, description=$3, budget_t4_hours=$4, budget_cpu_core_hours=$5, budget_ram_gb_hours=$6, budget_storage_gb_hours=$7, max_agents=$8, metrics=$9,
    report_interval_seconds=$10, starts_at=$11, ends_at=$12, updated_at=NOW()
WHERE id=$1`
	_, err = s.pool.pool.Exec(ctx, q,
		pe.ID, pe.Name, pe.Description, pe.BudgetT4Hours, pe.BudgetCPUCoreHours, pe.BudgetRAMGBHours, pe.BudgetStorageGBHours, pe.MaxAgents,
		metrics, pe.ReportIntervalSeconds, pe.StartsAt, pe.EndsAt,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Update: %w", err)
	}
	return nil
}

// TriggerPhase2 atomically sets phase=2 and phase2_triggered_at, and records held agent IDs.
// It is a one-way operation: it only fires when phase=1 (atomic guard in WHERE clause).
// Returns false if phase 2 was already triggered (another routine beat us to it).
func (s *PlatformExperimentsStore) TriggerPhase2(ctx context.Context, platformExpID string, heldAgentIDs []string) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.TriggerPhase2: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx,
		`UPDATE platform_experiments SET phase=2, phase2_triggered_at=NOW(), updated_at=NOW() WHERE id=$1 AND phase=1`,
		platformExpID,
	)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.TriggerPhase2: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already phase 2
	}

	for _, agentID := range heldAgentIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO experiment_phase2_holds (platform_experiment_id, agent_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			platformExpID, agentID,
		); err != nil {
			return false, fmt.Errorf("platform_experiments_store.TriggerPhase2: insert hold %s: %w", agentID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.TriggerPhase2: commit: %w", err)
	}
	return true, nil
}

// ListPhase2HeldAgents returns agent IDs that are on hold for a platform experiment.
func (s *PlatformExperimentsStore) ListPhase2HeldAgents(ctx context.Context, platformExpID string) ([]string, error) {
	const q = `SELECT agent_id FROM experiment_phase2_holds WHERE platform_experiment_id=$1 ORDER BY held_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListPhase2HeldAgents: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Phase2ZeroOp strips one resource dimension's guaranteed/burst allocation from one held agent.
type Phase2ZeroOp struct {
	AgentID      string
	ResourceType domain.ResourceType
}

// Phase2AddOp adds delta to one resource dimension's guaranteed allocation for one active agent.
type Phase2AddOp struct {
	AgentID      string
	ResourceType domain.ResourceType
	Delta        float64
}

// RedistributePhase2Quota atomically applies every held-agent zero and active-agent add for a
// platform experiment's phase-2 transition, in one Postgres transaction, and durably claims that
// this platform experiment's redistribution is done — all inside the same commit. Findings.md #9:
// this is what makes redistribution crash-safe. A naive "loop over N UPDATE statements, mark done
// after" can be interrupted mid-loop, and re-running AddToAgentGuaranteedQuota's delta a second
// time would double-credit an agent — there is no idempotent way to retry a partially-applied
// delta. Wrapping every op plus the completion marker in one transaction makes the whole
// redistribution all-or-nothing: any crash before commit leaves phase2_redistributed_at NULL and
// nothing applied, so a retry (see Controller.reconcilePhase2Hold) redoes the entire thing safely
// from scratch; any crash after commit has already applied everything exactly once.
//
// Returns (false, nil) if this platform experiment's redistribution was already committed by an
// earlier call — the caller must skip straight to retrying job-stopping (idempotent on its own)
// instead of re-applying ops.
func (s *PlatformExperimentsStore) RedistributePhase2Quota(ctx context.Context, platformExpID string, zeros []Phase2ZeroOp, adds []Phase2AddOp) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// FOR UPDATE serializes concurrent callers (e.g. two control-service replicas both
	// reconciling the same platform experiment) on this row, so only one of them ever observes
	// phase2_redistributed_at IS NULL and proceeds to apply ops.
	var alreadyDone bool
	if err := tx.QueryRow(ctx,
		`SELECT phase2_redistributed_at IS NOT NULL FROM platform_experiments WHERE id=$1 FOR UPDATE`,
		platformExpID,
	).Scan(&alreadyDone); err != nil {
		return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: check: %w", err)
	}
	if alreadyDone {
		return false, nil
	}

	for _, z := range zeros {
		guaranteed, burst := resourceQuotaColumns(z.ResourceType)
		q := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed, burst)
		if _, err := tx.Exec(ctx, q, z.AgentID, platformExpID); err != nil {
			return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: zero %s/%s: %w", z.AgentID, z.ResourceType, err)
		}
	}
	for _, a := range adds {
		guaranteed, _ := resourceQuotaColumns(a.ResourceType)
		q := fmt.Sprintf(`UPDATE agent_quotas SET %[1]s = %[1]s + $3 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed)
		if _, err := tx.Exec(ctx, q, a.AgentID, platformExpID, a.Delta); err != nil {
			return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: add %s/%s: %w", a.AgentID, a.ResourceType, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE platform_experiments SET phase2_redistributed_at=NOW(), updated_at=NOW() WHERE id=$1`,
		platformExpID,
	); err != nil {
		return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: mark done: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.RedistributePhase2Quota: commit: %w", err)
	}
	return true, nil
}

// IsAgentHeld returns true if the agent is on phase-2 hold for the given platform experiment.
func (s *PlatformExperimentsStore) IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `SELECT 1 FROM experiment_phase2_holds WHERE platform_experiment_id=$1 AND agent_id=$2`
	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsAgentHeld: %w", err)
	}
	return true, nil
}

// AddToAgentGuaranteedQuota increases an agent's guaranteed allocation for resourceType.
// Used when redistributing quota from held agents to active ones (phase 2) and for donations
// (GPU-hours only, today).
func (s *PlatformExperimentsStore) AddToAgentGuaranteedQuota(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType, delta float64) error {
	guaranteed, _ := resourceQuotaColumns(resourceType)
	q := fmt.Sprintf(`UPDATE agent_quotas SET %[1]s = %[1]s + $3 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed)
	_, err := s.pool.pool.Exec(ctx, q, agentID, platformExpID, delta)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AddToAgentGuaranteedQuota: %w", err)
	}
	return nil
}

// WithAdmissionLock runs fn while holding a Postgres transaction-scoped advisory lock keyed on
// (agentID, platformExpID), so quota admission is serialized across every control-service
// replica, not just within one process. Scoped to the transaction so it auto-releases on
// commit/rollback (including a crash mid-fn) rather than needing an explicit unlock.
func (s *PlatformExperimentsStore) WithAdmissionLock(ctx context.Context, agentID, platformExpID string, fn func(ctx context.Context) error) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.WithAdmissionLock: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	key := agentID + "/" + platformExpID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("platform_experiments_store.WithAdmissionLock: acquire lock: %w", err)
	}

	if err := fn(ctx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.WithAdmissionLock: commit: %w", err)
	}
	return nil
}
