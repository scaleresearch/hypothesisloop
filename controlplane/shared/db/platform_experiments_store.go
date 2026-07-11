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

// ZeroAgentGuaranteedQuota sets resourceType's guaranteed and burst allocation to 0 for a held
// agent. Called during phase 2 transition to strip held agents of their remaining allocation —
// callers loop over every resource dimension the platform experiment tracks, not just GPU.
func (s *PlatformExperimentsStore) ZeroAgentGuaranteedQuota(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType) error {
	guaranteed, burst := resourceQuotaColumns(resourceType)
	q := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0 WHERE agent_id=$1 AND platform_experiment_id=$2`, guaranteed, burst)
	_, err := s.pool.pool.Exec(ctx, q, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.ZeroAgentGuaranteedQuota: %w", err)
	}
	return nil
}

// ---- experiment_signups ----

// Signup records an agent's intent to participate in a platform experiment.
func (s *PlatformExperimentsStore) Signup(ctx context.Context, platformExpID, agentID string) error {
	const q = `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at)
VALUES ($1, $2, NOW())
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING`

	_, err := s.pool.pool.Exec(ctx, q, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Signup: %w", err)
	}
	return nil
}

// ListSignups returns all agent IDs signed up for a platform experiment.
func (s *PlatformExperimentsStore) ListSignups(ctx context.Context, platformExpID string) ([]string, error) {
	const q = `
SELECT agent_id FROM experiment_signups
WHERE platform_experiment_id = $1
ORDER BY signed_up_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListSignups: %w", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListSignups: scan: %w", err)
		}
		agents = append(agents, id)
	}
	return agents, rows.Err()
}

// IsSignedUp returns true if the agent is signed up for the platform experiment.
func (s *PlatformExperimentsStore) IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `
SELECT 1 FROM experiment_signups
WHERE platform_experiment_id = $1 AND agent_id = $2`

	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsSignedUp: %w", err)
	}
	return true, nil
}

// CountSignups returns how many agents have signed up for a platform experiment.
func (s *PlatformExperimentsStore) CountSignups(ctx context.Context, platformExpID string) (int, error) {
	const q = `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1`
	var n int
	if err := s.pool.pool.QueryRow(ctx, q, platformExpID).Scan(&n); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.CountSignups: %w", err)
	}
	return n, nil
}

// ---- agent_quotas ----

// UpsertAgentQuota creates or replaces an agent's quota allocation for a platform experiment.
// Populates all 4 resource dimensions (GPU is always non-zero; CPU/RAM/storage are 0 = "not
// tracked" for platform experiments that don't budget them). Only the allocation (capacity
// setting) lives here — consumption (used_*) lives solely in the metrics DB, see
// metricsdb.UsageTracker; Postgres never holds a copy of it.
func (s *PlatformExperimentsStore) UpsertAgentQuota(ctx context.Context, q *domain.AgentQuota) error {
	const sql = `
INSERT INTO agent_quotas (
	id, agent_id, platform_experiment_id,
	guaranteed_t4_hours, burst_t4_hours,
	guaranteed_cpu_core_hours, burst_cpu_core_hours,
	guaranteed_ram_gb_hours, burst_ram_gb_hours,
	guaranteed_storage_gb_hours, burst_storage_gb_hours,
	created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (agent_id, platform_experiment_id) DO UPDATE SET
	guaranteed_t4_hours         = EXCLUDED.guaranteed_t4_hours,
	burst_t4_hours              = EXCLUDED.burst_t4_hours,
	guaranteed_cpu_core_hours   = EXCLUDED.guaranteed_cpu_core_hours,
	burst_cpu_core_hours        = EXCLUDED.burst_cpu_core_hours,
	guaranteed_ram_gb_hours     = EXCLUDED.guaranteed_ram_gb_hours,
	burst_ram_gb_hours          = EXCLUDED.burst_ram_gb_hours,
	guaranteed_storage_gb_hours = EXCLUDED.guaranteed_storage_gb_hours,
	burst_storage_gb_hours      = EXCLUDED.burst_storage_gb_hours`

	_, err := s.pool.pool.Exec(ctx, sql,
		q.ID, q.AgentID, q.PlatformExperimentID,
		q.GuaranteedT4Hours, q.BurstT4Hours,
		q.GuaranteedCPUCoreHours, q.BurstCPUCoreHours,
		q.GuaranteedRAMGBHours, q.BurstRAMGBHours,
		q.GuaranteedStorageGBHours, q.BurstStorageGBHours,
		q.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.UpsertAgentQuota: %w", err)
	}
	return nil
}

// agentQuotaColumns is the canonical column list for agent_quotas SELECT queries, shared by
// GetAgentQuota/ListAgentQuotas. Allocation only — used_* is populated separately from the
// metrics DB by the caller (see metricsdb.PopulateUsage/PopulateUsageOne).
const agentQuotaColumns = `
	id, agent_id, platform_experiment_id,
	guaranteed_t4_hours, burst_t4_hours,
	guaranteed_cpu_core_hours, burst_cpu_core_hours,
	guaranteed_ram_gb_hours, burst_ram_gb_hours,
	guaranteed_storage_gb_hours, burst_storage_gb_hours,
	created_at
`

func scanAgentQuota(row rowScanner) (*domain.AgentQuota, error) {
	aq := &domain.AgentQuota{}
	err := row.Scan(
		&aq.ID, &aq.AgentID, &aq.PlatformExperimentID,
		&aq.GuaranteedT4Hours, &aq.BurstT4Hours,
		&aq.GuaranteedCPUCoreHours, &aq.BurstCPUCoreHours,
		&aq.GuaranteedRAMGBHours, &aq.BurstRAMGBHours,
		&aq.GuaranteedStorageGBHours, &aq.BurstStorageGBHours,
		&aq.CreatedAt,
	)
	return aq, err
}

// GetAgentQuota returns the quota for an agent in a platform experiment.
func (s *PlatformExperimentsStore) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	q := `SELECT` + agentQuotaColumns + `FROM agent_quotas WHERE agent_id = $1 AND platform_experiment_id = $2`

	aq, err := scanAgentQuota(s.pool.pool.QueryRow(ctx, q, agentID, platformExpID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.GetAgentQuota: %w", err)
	}
	return aq, nil
}

// ListAgentQuotas returns all quotas for a platform experiment.
func (s *PlatformExperimentsStore) ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	q := `SELECT` + agentQuotaColumns + `FROM agent_quotas WHERE platform_experiment_id = $1 ORDER BY guaranteed_t4_hours DESC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListAgentQuotas: %w", err)
	}
	defer rows.Close()

	var out []*domain.AgentQuota
	for rows.Next() {
		aq, err := scanAgentQuota(rows)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListAgentQuotas: scan: %w", err)
		}
		out = append(out, aq)
	}
	return out, rows.Err()
}

// resourceQuotaColumns returns the (guaranteed, burst) allocation column names backing one
// resource dimension's quota bucket on agent_quotas. GPU-hours is the original/default
// dimension. Consumption (used_*) has no Postgres column — see metricsdb.UsageTracker.
func resourceQuotaColumns(rt domain.ResourceType) (guaranteed, burst string) {
	switch rt {
	case domain.ResourceCPUCoreHours:
		return "guaranteed_cpu_core_hours", "burst_cpu_core_hours"
	case domain.ResourceRAMGBHours:
		return "guaranteed_ram_gb_hours", "burst_ram_gb_hours"
	case domain.ResourceStorageGBHours:
		return "guaranteed_storage_gb_hours", "burst_storage_gb_hours"
	default: // domain.ResourceGPUHours
		return "guaranteed_t4_hours", "burst_t4_hours"
	}
}

// ---- top-3 history (Domain 6) ----

// RecordTop3 records that agentID placed in the top 3 for a platform experiment.
func (s *PlatformExperimentsStore) RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error {
	const q = `
INSERT INTO experiment_top3 (platform_experiment_id, agent_id, final_metric, recorded_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET final_metric = EXCLUDED.final_metric`

	_, err := s.pool.pool.Exec(ctx, q, platformExpID, agentID, finalMetric)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RecordTop3: %w", err)
	}
	return nil
}

// HasTop3History returns true if the agent has ever placed in the top 3.
func (s *PlatformExperimentsStore) HasTop3History(ctx context.Context, agentID string) (bool, error) {
	const q = `SELECT 1 FROM experiment_top3 WHERE agent_id = $1 LIMIT 1`
	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.HasTop3History: %w", err)
	}
	return true, nil
}
