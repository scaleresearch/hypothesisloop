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

// minObservedLookback floors the last_over_time window so a platform experiment queried the
// instant it's created (start == now, or clock skew puts start a hair in the future) still gets
// a non-degenerate window instead of `[0s]`/a negative duration, which PromQL would reject or
// which would spuriously hide a sample written in the same second.
const minObservedLookback = time.Hour

// maxObservedLookback ceilings the window. A row whose start time is the zero value — which dev
// data actually contained, written before CreatedAt was populated — makes time.Since produce a
// ~2000-year duration, and GreptimeDB's PromQL parser rejects the literal outright, 500ing every
// quota and standings read for that experiment forever. A year covers any platform experiment
// that will really run, so clamping loses nothing a real caller could have wanted.
const maxObservedLookback = 365 * 24 * time.Hour

// ObservedLookback turns a platform experiment's own start time into a last_over_time window
// long enough to see every sample it could possibly have produced — never a fixed constant.
// Each settled job writes its cost exactly once, as an absolute set against its own series (see
// SetObservedBatch), so a bare instant-vector selector loses it the moment it falls outside
// Prometheus's 5-minute default staleness window: last_over_time keeps it visible, but only for
// however long the window covers. A fixed window (e.g. 30 days) just moves the cliff edge —
// platform experiments legitimately run for weeks, so any hardcoded duration eventually hides a
// real, already-settled cost from budgets and stage accounting again (see important.md: never
// bake a decision/threshold into desired state). Sizing the window to the experiment's own
// lifetime instead means it always covers every sample the experiment could have written.
//
// Exported because every reader that ranks or bills off a platform experiment's history needs the
// same window — a second, hand-picked constant elsewhere reintroduces exactly this cliff.
func ObservedLookback(start time.Time) string {
	d := time.Since(start)
	if d < minObservedLookback {
		d = minObservedLookback
	}
	if d > maxObservedLookback {
		d = maxObservedLookback
	}
	return fmt.Sprintf("%ds", int64(d.Seconds()))
}

// PopulateUsage fills every quotas[i].Used* field from the metrics DB with a single query for
// the whole platform experiment (summed across every settled job, grouped by
// agent/resource/tier) instead of one query per bucket per agent. platformExpStart is the
// platform experiment's own creation time (see ObservedLookback) — callers that already hold the
// domain.PlatformExperiment pass its CreatedAt; others fetch it once via their store, the same
// pattern registry.GetTimeseries uses to size its own query window off the real experiment.
func PopulateUsage(ctx context.Context, dbURL string, platformExpStart time.Time, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (agent_id, resource_type, tier) (last_over_time(%s{platform_experiment_id=%q, kind=%q}[%s]))`,
		usedHoursMetric, platformExpID, kindObserved, ObservedLookback(platformExpStart))
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
// job the agent has run in this platform experiment. See PopulateUsage for platformExpStart.
func PopulateUsageOne(ctx context.Context, dbURL string, platformExpStart time.Time, q *domain.AgentQuota) error {
	if q == nil {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (resource_type, tier) (last_over_time(%s{agent_id=%q, platform_experiment_id=%q, kind=%q}[%s]))`,
		usedHoursMetric, q.AgentID, q.PlatformExperimentID, kindObserved, ObservedLookback(platformExpStart))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsageOne: %w", err)
	}
	for _, s := range samples {
		applyUsedSample(q, domain.ResourceType(s.Labels["resource_type"]), domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

// SettledCostForJob returns one job's settled cost per resource dimension, or ok=false when
// nothing has been settled for it yet (it is still running, or its terminal write has not landed).
// This is the number the platform actually billed — read from the same absolute-set series
// settlement writes, never recomputed from an estimate.
func SettledCostForJob(ctx context.Context, dbURL string, platformExpStart time.Time, experimentID string) (map[domain.ResourceType]float64, bool, error) {
	promQL := fmt.Sprintf(`last_over_time(%s{experiment_id=%q, kind=%q}[%s])`,
		usedHoursMetric, experimentID, kindObserved, ObservedLookback(platformExpStart))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, false, fmt.Errorf("metricsdb.SettledCostForJob: %w", err)
	}
	if len(samples) == 0 {
		return nil, false, nil
	}
	out := make(map[domain.ResourceType]float64, len(samples))
	for _, s := range samples {
		out[domain.ResourceType(s.Labels["resource_type"])] = s.Value
	}
	return out, true, nil
}

func applyUsedSample(q *domain.AgentQuota, resourceType domain.ResourceType, tier domain.CapacityTier, value float64) {
	// resourceType is always domain.ResourceAcceleratorHours now — it's the only ResourceType left.
	_ = resourceType
	if tier == domain.CapacityGuaranteed {
		q.UsedGuaranteedAccH = value
	} else {
		q.UsedBurstAccH = value
	}
}

// TotalObservedAccH sums settled, observed accelerator cost across every agent and job in a
// platform experiment — filtered to kind=observed, so it never counts a queued or running job's
// reservation. This is the "committed" half of stage-boundary progress; the caller adds live
// usage of running attempts on top (see controller.stageProgress). Reading reservations
// here would let a large queued job prematurely trip a stage boundary and cancel work.
func TotalObservedAccH(ctx context.Context, dbURL string, platformExpStart time.Time, platformExpID string) (float64, error) {
	// See PopulateUsage for why the lookback is sized off the platform experiment's own start
	// time rather than a fixed constant.
	promQL := fmt.Sprintf(`sum(last_over_time(%s{platform_experiment_id=%q, resource_type=%q, kind=%q}[%s]))`,
		usedHoursMetric, platformExpID, string(domain.ResourceAcceleratorHours), kindObserved, ObservedLookback(platformExpStart))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.TotalObservedAccH: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}
