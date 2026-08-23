package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// ObservedState is everything the controller reads back from the metrics store: what has actually
// been seen to happen. Postgres holds the desired state and answers what should exist; this
// answers what does, and every eviction verdict this package reaches is a function of it.
//
// It is an interface for the same reason Store is — the controller states what it needs and the
// datastore package supplies it. The alternative, and what this replaced, was a test that stood up
// an HTTP server and hand-wrote GreptimeDB result JSON, routed by matching substrings of the SQL
// the query layer happened to emit ("LAG(ts)", "CROSS JOIN present"). That fake could only answer
// one way per query, which is why the branches below it — an unreachable cluster, a workload the
// runtime confirms is gone, an absence not yet confirmed — had no tests at all.
//
// gapCap stays a parameter rather than moving into the implementation: unlike the scheduler's, the
// controller's is derived from its own silence policy (observedGapCap), and the caller already has
// it in hand at the point of the call.
type ObservedState interface {
	IsAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error)
	ObservedElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time, gapCap time.Duration) (float64, error)
	FirstObserved(ctx context.Context, experimentID string, createdAt, now time.Time, gapCap time.Duration) (time.Time, bool, error)
	LatestJobPhase(ctx context.Context, experimentID, clusterName string, window time.Duration) (workload.JobPhase, bool, error)
	ClusterSnapshotPresence(ctx context.Context, experimentID, clusterName string, createdAt, now time.Time) (metricsdb.SnapshotPresence, error)
	AnyDeclaredMetricChanged(ctx context.Context, experimentID string, metricKeys []string, window time.Duration) (reported, changed bool, err error)
	AnyDeclaredMetricReported(ctx context.Context, experimentID string, metricKeys []string, window time.Duration) (bool, error)
	HasEverReportedMetric(ctx context.Context, experimentID string, createdAt, now time.Time) (bool, error)
	TotalObservedAccH(ctx context.Context, platformExpStart time.Time, platformExpID string) (float64, error)
	PopulateUsage(ctx context.Context, platformExpStart time.Time, platformExpID string, quotas []*domain.AgentQuota) error
	BestPerAgentOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition) (map[string]metricsdb.AgentBest, []string, error)
}

// metricsDBObserver is the only production ObservedState: metricsdb answers in real time.
type metricsDBObserver struct{ url string }

func (o metricsDBObserver) IsAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error) {
	return metricsdb.IsAlive(ctx, o.url, experimentID, window)
}

func (o metricsDBObserver) ObservedElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time, gapCap time.Duration) (float64, error) {
	return metricsdb.ObservedElapsedHours(ctx, o.url, experimentID, createdAt, now, gapCap)
}

func (o metricsDBObserver) FirstObserved(ctx context.Context, experimentID string, createdAt, now time.Time, gapCap time.Duration) (time.Time, bool, error) {
	return metricsdb.FirstObserved(ctx, o.url, experimentID, createdAt, now, gapCap)
}

func (o metricsDBObserver) LatestJobPhase(ctx context.Context, experimentID, clusterName string, window time.Duration) (workload.JobPhase, bool, error) {
	return metricsdb.LatestJobPhase(ctx, o.url, experimentID, clusterName, window)
}

func (o metricsDBObserver) ClusterSnapshotPresence(ctx context.Context, experimentID, clusterName string, createdAt, now time.Time) (metricsdb.SnapshotPresence, error) {
	return metricsdb.ClusterSnapshotPresence(ctx, o.url, experimentID, clusterName, createdAt, now)
}

func (o metricsDBObserver) AnyDeclaredMetricChanged(ctx context.Context, experimentID string, metricKeys []string, window time.Duration) (bool, bool, error) {
	return metricsdb.AnyDeclaredMetricChanged(ctx, o.url, experimentID, metricKeys, window)
}

func (o metricsDBObserver) AnyDeclaredMetricReported(ctx context.Context, experimentID string, metricKeys []string, window time.Duration) (bool, error) {
	return metricsdb.AnyDeclaredMetricReported(ctx, o.url, experimentID, metricKeys, window)
}

func (o metricsDBObserver) HasEverReportedMetric(ctx context.Context, experimentID string, createdAt, now time.Time) (bool, error) {
	return metricsdb.HasEverReportedMetric(ctx, o.url, experimentID, createdAt, now)
}

func (o metricsDBObserver) TotalObservedAccH(ctx context.Context, platformExpStart time.Time, platformExpID string) (float64, error) {
	return metricsdb.TotalObservedAccH(ctx, o.url, platformExpStart, platformExpID)
}

func (o metricsDBObserver) PopulateUsage(ctx context.Context, platformExpStart time.Time, platformExpID string, quotas []*domain.AgentQuota) error {
	return metricsdb.PopulateUsage(ctx, o.url, platformExpStart, platformExpID, quotas)
}

func (o metricsDBObserver) BestPerAgentOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition) (map[string]metricsdb.AgentBest, []string, error) {
	return metricsdb.BestPerAgentOnMetric(ctx, o.url, pe, metric)
}

// observedGapCap is the longest gap between two real observations that still counts as
// continuous presence, so silence detection and observed-cost accounting agree.
func (c *Controller) observedGapCap() time.Duration {
	return time.Duration(c.silenceMultiplier * float64(c.defaultReportInterval))
}

// isAlive reports whether experimentID has a real observation (heartbeat or job-reported
// metric) within `window` of now — a stateless GreptimeDB query, not an in-memory last-seen
// map, so every process gets the same answer with no warm-up after a restart.
func (c *Controller) isAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error) {
	return c.observed.IsAlive(ctx, experimentID, window)
}

// observedElapsedHours returns how long exp has been confirmed alive, in hours — see
// metricsdb.ObservedElapsedHours for the correctness argument (multi-node, clock-skew,
// gap-capping; no stored StartedAt needed). GreptimeDB is the sole source of truth: a query
// error is returned so the caller retries next reconcile, never papered over with a guess.
func (c *Controller) observedElapsedHours(ctx context.Context, exp *domain.Experiment, now time.Time) (float64, error) {
	return c.observed.ObservedElapsedHours(ctx, exp.ID, exp.CreatedAt, now, c.observedGapCap())
}

// tickObservations is one reconcile pass's observed-elapsed reading, taken once per job and
// shared by every consumer in that pass. It is not a cache: it is built at the top of a tick,
// read within it, and discarded with it -- nothing survives to be invalidated or to drift.
//
// Reading the same fact separately per consumer was the anomaly. The stage-progress calculation,
// the job-length cap and the eviction they can trigger each queried it at their own instant, so a
// single tick could hold three different answers to "how long has this job run" and act on them
// as if they agreed.
type tickObservations struct {
	elapsedHours map[string]float64
}

// observeElapsed reads observed-elapsed once for each experiment, at one instant. A job whose
// query fails is simply absent: every consumer already skips a job it cannot measure, and one
// unreadable series must not cost the whole pass (important.md #19).
func (c *Controller) observeElapsed(ctx context.Context, exps []*domain.Experiment, now time.Time) *tickObservations {
	obs := &tickObservations{elapsedHours: make(map[string]float64, len(exps))}
	for _, exp := range exps {
		hours, err := c.observedElapsedHours(ctx, exp, now)
		if err != nil {
			c.logger.Warn("tick observations: observed elapsed unavailable, skipping this job for the pass",
				zap.String("experiment", exp.ID), zap.Error(err))
			continue
		}
		obs.elapsedHours[exp.ID] = hours
	}
	return obs
}

// hours reports the observed-elapsed reading for exp, and whether this pass has one.
func (o *tickObservations) hours(experimentID string) (float64, bool) {
	h, ok := o.elapsedHours[experimentID]
	return h, ok
}

// observedAcceleratorCost returns exp's accelerator cost so far: observed elapsed hours ×
// accelerator count × exp.AcceleratorType's registered rate. Billed flat at exp.AcceleratorType
// — the flavor admission already recorded as authoritative in Postgres (see domain.Experiment's
// AcceleratorType doc) — rather than a live per-type observation from the metrics store. That
// mechanism could bill a mid-run reschedule onto a different type correctly segment-by-segment,
// but required catching a job's RUNNING phase within a poll window to ever record anything; for
// jobs shorter than one poll tick it silently zeroed the whole run's accelerator cost (see
// services/settlement.Settler.Settle). This can't ever silently zero out, at the cost of slightly
// undercharging the rare mid-run type change.
func observedAcceleratorCost(exp *domain.Experiment, hours float64) float64 {
	if exp.AcceleratorCount <= 0 || exp.AcceleratorType == "" {
		return 0
	}
	// The same formula settlement bills by: the rate frozen into the reservation at admission,
	// not the live catalog. A mid-run rate change (or a retired flavor, which made the live
	// lookup fail and the job's cost silently drop out of the boundary) otherwise puts stage
	// progress and the budget agents are actually billed against on different numbers.
	return exp.RatedCost(exp.EstimatedCostAccH, hours)
}
