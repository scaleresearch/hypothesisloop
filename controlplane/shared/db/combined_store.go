package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Store embeds all store types providing unified access to all
// persistence operations.
type Store struct {
	*ExperimentsStore
	*AgentsStore
	*LedgerStore
	*DonationStore
	*PlatformExperimentsStore
	*ClusterQueueStore
	*HypothesesStore

	// usage is the sole read/write path for agent quota consumption (used_guaranteed_*/
	// used_burst_*), backed by the metrics DB rather than Postgres — see metricsdb.UsageTracker.
	usage *metricsdb.UsageTracker
}

// NewStore creates a Store backed by the given pool, tracking agent quota usage in the metrics
// DB at metricsDBURL.
func NewStore(pool *Pool, metricsDBURL string) *Store {
	return &Store{
		ExperimentsStore:         NewExperimentsStore(pool),
		AgentsStore:              NewAgentsStore(pool),
		LedgerStore:              NewLedgerStore(pool),
		DonationStore:            NewDonationStore(pool),
		PlatformExperimentsStore: NewPlatformExperimentsStore(pool),
		ClusterQueueStore:        NewClusterQueueStore(pool),
		HypothesesStore:          NewHypothesesStore(pool),
		usage:                    metricsdb.NewUsageTracker(metricsDBURL),
	}
}

// ---- Loop adapters ----

// LoopQuotaStore wraps PlatformExperimentsFullStore and satisfies scheduler.LoopQuotaStore.
type LoopQuotaStore struct{ *PlatformExperimentsFullStore }

// NewLoopQuotaStore returns a LoopQuotaStore.
func NewLoopQuotaStore(s *PlatformExperimentsFullStore) *LoopQuotaStore {
	return &LoopQuotaStore{PlatformExperimentsFullStore: s}
}

// GetAgentQuota overrides the embedded PlatformExperimentsStore's allocation-only read, merging
// in current usage from the metrics DB — the scheduler loop's fairness sort needs both.
func (lq *LoopQuotaStore) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := lq.PlatformExperimentsStore.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil || aq == nil {
		return aq, err
	}
	if err := metricsdb.PopulateUsageOne(ctx, lq.Store.usage.URL(), aq); err != nil {
		return nil, err
	}
	if err := lq.PlatformExperimentsStore.AddDesiredQuotaUsageOne(ctx, aq); err != nil {
		return nil, err
	}
	return aq, nil
}

// ---- Platform Experiments adapter ----

// PlatformExperimentsFullStore embeds Store and satisfies quota.PlatformExperimentsStore
// by providing both platform experiment and agent methods from a single composite.
type PlatformExperimentsFullStore struct {
	*Store
}

// NewPlatformExperimentsFullStore returns a store satisfying quota.PlatformExperimentsStore.
func NewPlatformExperimentsFullStore(s *Store) *PlatformExperimentsFullStore {
	return &PlatformExperimentsFullStore{Store: s}
}

// AdmitExperimentTx serializes, validates, and inserts one desired experiment in a single
// PostgreSQL transaction. observed contains only the metrics-store snapshot taken immediately
// before this call. An empty rejection reason means success; database failures are returned as
// errors and must never be presented as quota rejection.
func (s *PlatformExperimentsFullStore) AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observe func(context.Context) (*domain.AgentQuota, error)) (string, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("admit experiment: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	key := exp.AgentID + "/" + exp.PlatformExperimentID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return "", fmt.Errorf("admit experiment: lock: %w", err)
	}
	observed, err := observe(ctx)
	if err != nil {
		return "", fmt.Errorf("admit experiment: observed usage: %w", err)
	}

	var cut bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2
	)`, exp.PlatformExperimentID, exp.AgentID).Scan(&cut); err != nil {
		return "", fmt.Errorf("admit experiment: stage cut: %w", err)
	}
	if cut {
		return "agent was cut at a stage boundary", nil
	}

	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, exp.AgentID, exp.PlatformExperimentID))
	if err == pgx.ErrNoRows {
		return "no quota found (agent not signed up?)", nil
	}
	if err != nil {
		return "", fmt.Errorf("admit experiment: quota: %w", err)
	}

	var desiredGuaranteedAcc, desiredBurstAcc, desiredGuaranteedCPU, desiredBurstCPU float64
	var desiredGuaranteedRAM, desiredBurstRAM, desiredGuaranteedStorage, desiredBurstStorage float64
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='burst'), 0),
			COALESCE(SUM(estimated_cpu_core_hours) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_cpu_core_hours) FILTER (WHERE capacity_tier='burst'), 0),
			COALESCE(SUM(estimated_ram_gb_hours) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_ram_gb_hours) FILTER (WHERE capacity_tier='burst'), 0),
			COALESCE(SUM(estimated_storage_gb_hours) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_storage_gb_hours) FILTER (WHERE capacity_tier='burst'), 0)
		FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2
		  AND (status IN ('QUEUED','SUBMITTED','ADMITTED','RUNNING')
		       OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		exp.AgentID, exp.PlatformExperimentID).Scan(
		&desiredGuaranteedAcc, &desiredBurstAcc, &desiredGuaranteedCPU, &desiredBurstCPU,
		&desiredGuaranteedRAM, &desiredBurstRAM, &desiredGuaranteedStorage, &desiredBurstStorage); err != nil {
		return "", fmt.Errorf("admit experiment: desired usage: %w", err)
	}

	guaranteed := exp.CapacityTier == domain.CapacityGuaranteed
	usedAcc, limitAcc := observed.UsedBurstAccH+desiredBurstAcc, allocation.BurstAcceleratorHours
	usedCPU, limitCPU := observed.UsedBurstCPUCoreH+desiredBurstCPU, allocation.BurstCPUCoreHours
	usedRAM, limitRAM := observed.UsedBurstRAMGBH+desiredBurstRAM, allocation.BurstRAMGBHours
	usedStorage, limitStorage := observed.UsedBurstStorageGBH+desiredBurstStorage, allocation.BurstStorageGBHours
	if guaranteed {
		usedAcc, limitAcc = observed.UsedGuaranteedAccH+desiredGuaranteedAcc, allocation.GuaranteedAcceleratorHours
		usedCPU, limitCPU = observed.UsedGuaranteedCPUCoreH+desiredGuaranteedCPU, allocation.GuaranteedCPUCoreHours
		usedRAM, limitRAM = observed.UsedGuaranteedRAMGBH+desiredGuaranteedRAM, allocation.GuaranteedRAMGBHours
		usedStorage, limitStorage = observed.UsedGuaranteedStorageGBH+desiredGuaranteedStorage, allocation.GuaranteedStorageGBHours
	}
	if exp.EstimatedCostAccH > 0 && usedAcc+exp.EstimatedCostAccH > limitAcc {
		return fmt.Sprintf("insufficient_%s_quota: need %.2f accelerator_hours, have %.2f remaining", exp.CapacityTier, exp.EstimatedCostAccH, limitAcc-usedAcc), nil
	}
	if exp.EstimatedCPUCoreHours > 0 && usedCPU+exp.EstimatedCPUCoreHours > limitCPU {
		return fmt.Sprintf("insufficient_%s_quota: need %.2f cpu_core_hours, have %.2f remaining", exp.CapacityTier, exp.EstimatedCPUCoreHours, limitCPU-usedCPU), nil
	}
	if exp.EstimatedRAMGBHours > 0 && usedRAM+exp.EstimatedRAMGBHours > limitRAM {
		return fmt.Sprintf("insufficient_%s_quota: need %.2f ram_gb_hours, have %.2f remaining", exp.CapacityTier, exp.EstimatedRAMGBHours, limitRAM-usedRAM), nil
	}
	if exp.EstimatedStorageGBHours > 0 && usedStorage+exp.EstimatedStorageGBHours > limitStorage {
		return fmt.Sprintf("insufficient_%s_quota: need %.2f storage_gb_hours, have %.2f remaining", exp.CapacityTier, exp.EstimatedStorageGBHours, limitStorage-usedStorage), nil
	}
	if err := createExperiment(ctx, tx, exp); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("admit experiment: commit: %w", err)
	}
	return "", nil
}

// ReserveAdmittedFlavorTx rechecks the selected flavor's estimate and persists it under the
// same cross-replica quota lock used by initial admission.
func (s *PlatformExperimentsFullStore) ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, observe func(context.Context, string, string) (*domain.AgentQuota, error)) (string, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var agentID, platformExpID string
	var tier domain.CapacityTier
	var oldCost float64
	if err := tx.QueryRow(ctx, `SELECT agent_id, platform_experiment_id, capacity_tier, estimated_cost_acch FROM experiments WHERE id=$1`, experimentID).
		Scan(&agentID, &platformExpID, &tier, &oldCost); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: experiment: %w", err)
	}
	key := agentID + "/" + platformExpID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: lock: %w", err)
	}
	observed, err := observe(ctx, agentID, platformExpID)
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: observed usage: %w", err)
	}
	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, agentID, platformExpID))
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: quota: %w", err)
	}
	var desired float64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(estimated_cost_acch),0) FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2 AND capacity_tier=$3
		AND (status IN ('QUEUED','SUBMITTED','ADMITTED','RUNNING')
		OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		agentID, platformExpID, tier).Scan(&desired); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: desired usage: %w", err)
	}
	used, limit := observed.UsedBurstAccH, allocation.BurstAcceleratorHours
	if tier == domain.CapacityGuaranteed {
		used, limit = observed.UsedGuaranteedAccH, allocation.GuaranteedAcceleratorHours
	}
	projected := used + desired - oldCost + estimatedCost
	if projected > limit {
		return fmt.Sprintf("insufficient_%s_quota for selected %s: need %.2f additional accelerator_hours, have %.2f remaining", tier, acceleratorType, estimatedCost-oldCost, limit-(used+desired-oldCost)), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE experiments SET accelerator_type=$2, estimated_cost_acch=$3, updated_at=NOW() WHERE id=$1`, experimentID, string(acceleratorType), estimatedCost); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: commit: %w", err)
	}
	return "", nil
}
