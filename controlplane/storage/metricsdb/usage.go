package metricsdb

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// usedHoursMetric is the single gauge every agent-quota "used" bucket is stored under,
// distinguished by labels — the metrics DB is the sole store for consumption; Postgres only
// ever holds the guaranteed/burst allocation (the operator-set capacity setting), never a copy
// of how much of it has been used.
const usedHoursMetric = "agent_quota_used_hours"

// Every sample is an observed terminal cost. Outstanding estimates are scheduler state and are
// derived from non-terminal experiment rows in PostgreSQL; they are never copied into metrics.
const kindObserved = "observed"

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
func (s *Store) SetObservedBatch(ctx context.Context, agentID, platformExpID, experimentID string, tier domain.CapacityTier, amounts map[domain.ResourceType]float64) error {
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0, len(amounts))
	for resourceType, amount := range amounts {
		if amount < 0 {
			return fmt.Errorf("metricsdb.SetObservedBatch: negative %s amount", resourceType)
		}
		samples = append(samples, GaugeSample{MetricName: usedHoursMetric,
			Labels: labelsFor(agentID, platformExpID, experimentID, resourceType, tier), Value: amount, At: now})
	}
	return s.WriteGaugesAt(ctx, samples)
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
func (s *Store) PopulateUsage(ctx context.Context, platformExpStart time.Time, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (agent_id, resource_type, tier) (last_over_time(%s{platform_experiment_id=%q, kind=%q}[%s]))`,
		usedHoursMetric, platformExpID, kindObserved, ObservedLookback(platformExpStart))
	samples, err := s.QueryVector(ctx, promQL)
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
		applyUsedSample(q, domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

// PopulateUsageOne fills a single quota's Used* fields from the metrics DB, summed across every
// job the agent has run in this platform experiment. See PopulateUsage for platformExpStart.
func (s *Store) PopulateUsageOne(ctx context.Context, platformExpStart time.Time, q *domain.AgentQuota) error {
	if q == nil {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (resource_type, tier) (last_over_time(%s{agent_id=%q, platform_experiment_id=%q, kind=%q}[%s]))`,
		usedHoursMetric, q.AgentID, q.PlatformExperimentID, kindObserved, ObservedLookback(platformExpStart))
	samples, err := s.QueryVector(ctx, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsageOne: %w", err)
	}
	for _, s := range samples {
		applyUsedSample(q, domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

// SettledCostForJob returns one job's settled cost per resource dimension, or ok=false when
// nothing has been settled for it yet (it is still running, or its terminal write has not landed).
// This is the number the platform actually billed — read from the same absolute-set series
// settlement writes, never recomputed from an estimate.
func (s *Store) SettledCostForJob(ctx context.Context, platformExpStart time.Time, experimentID string) (map[domain.ResourceType]float64, bool, error) {
	promQL := fmt.Sprintf(`last_over_time(%s{experiment_id=%q, kind=%q}[%s])`,
		usedHoursMetric, experimentID, kindObserved, ObservedLookback(platformExpStart))
	samples, err := s.QueryVector(ctx, promQL)
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

func applyUsedSample(q *domain.AgentQuota, tier domain.CapacityTier, value float64) {
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
func (s *Store) TotalObservedAccH(ctx context.Context, platformExpStart time.Time, platformExpID string) (float64, error) {
	// See PopulateUsage for why the lookback is sized off the platform experiment's own start
	// time rather than a fixed constant.
	promQL := fmt.Sprintf(`sum(last_over_time(%s{platform_experiment_id=%q, resource_type=%q, kind=%q}[%s]))`,
		usedHoursMetric, platformExpID, string(domain.ResourceAcceleratorHours), kindObserved, ObservedLookback(platformExpStart))
	samples, err := s.QueryVector(ctx, promQL)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.TotalObservedAccH: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}

// SettledAccHByJob returns each job's settled accelerator cost in a platform experiment, keyed by
// experiment id. A requeue settles the interrupted stint while the job stays alive, so a reader
// adding a running job's cumulative observed cost on top of settled usage must subtract that
// job's entry here or the earlier stints count twice.
func (s *Store) SettledAccHByJob(ctx context.Context, platformExpStart time.Time, platformExpID string) (map[string]float64, error) {
	promQL := fmt.Sprintf(`sum by (experiment_id) (last_over_time(%s{platform_experiment_id=%q, resource_type=%q, kind=%q}[%s]))`,
		usedHoursMetric, platformExpID, string(domain.ResourceAcceleratorHours), kindObserved, ObservedLookback(platformExpStart))
	samples, err := s.QueryVector(ctx, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.SettledAccHByJob: %w", err)
	}
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		out[s.Labels["experiment_id"]] = s.Value
	}
	return out, nil
}
