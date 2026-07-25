package metricsdb

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// usedHoursMetric is the single gauge every agent-quota "used" bucket is stored under,
// distinguished by labels — the metrics DB is the sole store for consumption; Postgres only
// ever holds the guaranteed/burst allocation (the operator-set capacity setting), never a copy
// of how much of it has been used.
const usedHoursMetric = "agent_quota_used_hours"

// Every sample is an observed terminal cost. Outstanding estimates are scheduler state and are
// derived from non-terminal experiment rows in PostgreSQL; they are never copied into metrics.
const kindObserved = "observed"

// UsageTracker is the read/write path for observed terminal quota consumption (used_guaranteed_*/
// used_burst_* in the old Postgres schema).
//
// Every sample is tagged with experiment_id, so each job owns its own series — an "agent's used
// hours" bucket is never itself stored, only computed by summing per-job series at read time.
// This makes completion/eviction accounting idempotent: writing a job's final cost (SetObserved)
// is an absolute set against that job's own series, so replaying the same completion event twice
// writes the same value instead of double-refunding or double-debiting.
type UsageTracker struct {
	dbURL string
}

// NewUsageTracker constructs a tracker backed by the GreptimeDB instance at dbURL.
func NewUsageTracker(dbURL string) *UsageTracker {
	return &UsageTracker{dbURL: dbURL}
}

// URL returns the GreptimeDB base URL this tracker is backed by, for callers (e.g. db.Store)
// that need to make their own PopulateUsage/PopulateUsageOne calls against the same instance.
func (t *UsageTracker) URL() string { return t.dbURL }

func labelsFor(agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier) map[string]string {
	return map[string]string{
		"agent_id":               agentID,
		"platform_experiment_id": platformExpID,
		"experiment_id":          experimentID,
		"resource_type":          string(resourceType),
		"tier":                   string(tier),
		"kind":                   kindObserved,
	}
}

// SetObservedBatch writes every observed resource dimension for one terminal job in one remote
// write. This prevents admission from ever seeing a partially-settled resource vector.
func (t *UsageTracker) SetObservedBatch(ctx context.Context, agentID, platformExpID, experimentID string, tier domain.CapacityTier, amounts map[domain.ResourceType]float64) error {
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0, len(amounts))
	for resourceType, amount := range amounts {
		if amount < 0 {
			return fmt.Errorf("metricsdb.SetObservedBatch: negative %s amount", resourceType)
		}
		samples = append(samples, GaugeSample{MetricName: usedHoursMetric,
			Labels: labelsFor(agentID, platformExpID, experimentID, resourceType, tier), Value: amount, At: now})
	}
	return WriteGaugesAt(ctx, t.dbURL, samples)
}

// PopulateUsage fills every quotas[i].Used* field from the metrics DB with a single query for
// the whole platform experiment (summed across every settled job, grouped by
// agent/resource/tier) instead of one query per bucket per agent.
func PopulateUsage(ctx context.Context, dbURL, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (agent_id, resource_type, tier) (%s{platform_experiment_id=%q, kind=%q})`, usedHoursMetric, platformExpID, kindObserved)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsage: %w", err)
	}

	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}

	for _, s := range samples {
		q, ok := byAgent[s.Labels["agent_id"]]
		if !ok {
			continue
		}
		applyUsedSample(q, domain.ResourceType(s.Labels["resource_type"]), domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

// PopulateUsageOne fills a single quota's Used* fields from the metrics DB, summed across every
// job the agent has run in this platform experiment.
func PopulateUsageOne(ctx context.Context, dbURL string, q *domain.AgentQuota) error {
	if q == nil {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (resource_type, tier) (%s{agent_id=%q, platform_experiment_id=%q, kind=%q})`, usedHoursMetric, q.AgentID, q.PlatformExperimentID, kindObserved)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsageOne: %w", err)
	}
	for _, s := range samples {
		applyUsedSample(q, domain.ResourceType(s.Labels["resource_type"]), domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

func applyUsedSample(q *domain.AgentQuota, resourceType domain.ResourceType, tier domain.CapacityTier, value float64) {
	guaranteed := tier == domain.CapacityGuaranteed
	switch resourceType {
	case domain.ResourceCPUCoreHours:
		if guaranteed {
			q.UsedGuaranteedCPUCoreH = value
		} else {
			q.UsedBurstCPUCoreH = value
		}
	case domain.ResourceRAMGBHours:
		if guaranteed {
			q.UsedGuaranteedRAMGBH = value
		} else {
			q.UsedBurstRAMGBH = value
		}
	case domain.ResourceStorageGBHours:
		if guaranteed {
			q.UsedGuaranteedStorageGBH = value
		} else {
			q.UsedBurstStorageGBH = value
		}
	default: // domain.ResourceAcceleratorHours
		if guaranteed {
			q.UsedGuaranteedAccH = value
		} else {
			q.UsedBurstAccH = value
		}
	}
}

// TotalObservedAccH sums settled, observed accelerator cost across every agent and job in a
// platform experiment — filtered to kind=observed, so it never counts a queued or running job's
// reservation. This is the "committed" half of the phase-2 boundary check; the caller adds live
// usage of running attempts on top (see controller.checkPhase2Transition). Reading reservations
// here would let a large queued job prematurely trip phase 2 and cancel work.
func TotalObservedAccH(ctx context.Context, dbURL, platformExpID string) (float64, error) {
	promQL := fmt.Sprintf(`sum(%s{platform_experiment_id=%q, resource_type=%q, kind=%q})`, usedHoursMetric, platformExpID, string(domain.ResourceAcceleratorHours), kindObserved)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.TotalObservedAccH: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}
