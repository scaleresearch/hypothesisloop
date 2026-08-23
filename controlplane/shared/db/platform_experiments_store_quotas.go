package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ---- agent_quotas ----

// UpsertAgentQuota creates or replaces an agent's quota allocation for a platform experiment.
// Only the allocation (capacity setting) lives here — consumption (used_*) lives solely in the
// metrics DB, see metricsdb.UsageTracker; Postgres never holds a copy of observed consumption.
const upsertAgentQuotaSQL = `
INSERT INTO agent_quotas (
	id, agent_id, platform_experiment_id,
	guaranteed_accelerator_hours, burst_accelerator_hours,
	created_at
)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (agent_id, platform_experiment_id) DO UPDATE SET
	guaranteed_accelerator_hours         = EXCLUDED.guaranteed_accelerator_hours,
	burst_accelerator_hours              = EXCLUDED.burst_accelerator_hours`

func agentQuotaUpsertArgs(q *domain.AgentQuota) []any {
	return []any{
		q.ID, q.AgentID, q.PlatformExperimentID,
		q.GuaranteedAcceleratorHours, q.BurstAcceleratorHours,
		q.CreatedAt,
	}
}

func (s *PlatformExperimentsStore) UpsertAgentQuota(ctx context.Context, q *domain.AgentQuota) error {
	_, err := s.pool.pool.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(q)...)
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
	guaranteed_accelerator_hours, burst_accelerator_hours,
	created_at
`

func scanAgentQuota(row rowScanner) (*domain.AgentQuota, error) {
	aq := &domain.AgentQuota{}
	err := row.Scan(
		&aq.ID, &aq.AgentID, &aq.PlatformExperimentID,
		&aq.GuaranteedAcceleratorHours, &aq.BurstAcceleratorHours,
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
	q := `SELECT` + agentQuotaColumns + `FROM agent_quotas WHERE platform_experiment_id = $1 ORDER BY guaranteed_accelerator_hours DESC`

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

// AddDesiredQuotaUsage adds outstanding scheduler reservations to quotas. A non-terminal
// experiment row is the reservation: its estimates are authoritative PostgreSQL desired state,
// so no reservation series or second table is maintained elsewhere.
//
// This is an admission-side figure only ("may I start one more?"). Nothing that terminates or
// bills work may read it — see quota.GetObservedAgentQuota.
//
// The terminal-but-unsettled arm overlaps settlement's observed write by design: settlement
// writes the observed cost and then marks the row settled, two stores that cannot share a
// transaction, so in between a job counts as both a reservation and an observation. That
// over-counts, which for admission is the safe direction (it refuses work rather than
// overspending), and the settlement reconciler closes the window on retry.
func (s *PlatformExperimentsStore) AddDesiredQuotaUsage(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	const q = `
SELECT agent_id, capacity_tier,
       COALESCE(SUM(estimated_cost_acch), 0)
FROM experiments
WHERE platform_experiment_id = $1
  AND (
      status IN ('QUEUED', 'SUBMITTED', 'ADMITTED', 'RUNNING')
      OR (status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED') AND quota_settled_at IS NULL)
  )
GROUP BY agent_id, capacity_tier`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AddDesiredQuotaUsage: %w", err)
	}
	defer rows.Close()
	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, quota := range quotas {
		byAgent[quota.AgentID] = quota
	}
	for rows.Next() {
		var agentID string
		var tier domain.CapacityTier
		var accelerator float64
		if err := rows.Scan(&agentID, &tier, &accelerator); err != nil {
			return fmt.Errorf("platform_experiments_store.AddDesiredQuotaUsage: scan: %w", err)
		}
		quota := byAgent[agentID]
		if quota == nil {
			continue
		}
		if tier == domain.CapacityGuaranteed {
			quota.UsedGuaranteedAccH += accelerator
		} else {
			quota.UsedBurstAccH += accelerator
		}
	}
	return rows.Err()
}

// AddDesiredQuotaUsageOne is the single-agent counterpart to AddDesiredQuotaUsage.
func (s *PlatformExperimentsStore) AddDesiredQuotaUsageOne(ctx context.Context, quota *domain.AgentQuota) error {
	if quota == nil {
		return nil
	}
	return s.AddDesiredQuotaUsage(ctx, quota.PlatformExperimentID, []*domain.AgentQuota{quota})
}

// resourceQuotaColumns returns the (guaranteed, burst) allocation column names backing one
// resource dimension's quota bucket on agent_quotas. Accelerator-hours is the only dimension.
// Consumption (used_*) has no Postgres column — see metricsdb.UsageTracker.
func resourceQuotaColumns(rt domain.ResourceType) (guaranteed, burst string) {
	switch rt {
	default: // domain.ResourceAcceleratorHours
		return "guaranteed_accelerator_hours", "burst_accelerator_hours"
	}
}
