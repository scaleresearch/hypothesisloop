package db

import (
	"context"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
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
