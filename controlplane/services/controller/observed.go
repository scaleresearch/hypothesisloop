package controller

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// observedGapCap is the longest gap between two real observations that still counts as
// continuous presence, so silence detection and observed-cost accounting agree.
func (c *Controller) observedGapCap() time.Duration {
	return time.Duration(c.silenceMultiplier * float64(c.defaultReportInterval))
}

// observedStep is the grid resolution for ObservedElapsedHours — fine enough to catch every
// real report interval, coarse enough to stay cheap over a multi-hour experiment.
func (c *Controller) observedStep() time.Duration {
	if c.defaultReportInterval > 0 {
		return c.defaultReportInterval
	}
	return 10 * time.Second
}

// isAlive reports whether experimentID has a real observation (heartbeat or job-reported
// metric) within `window` of now — a stateless GreptimeDB query, not an in-memory last-seen
// map, so every process gets the same answer with no warm-up after a restart.
func (c *Controller) isAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error) {
	return metricsdb.IsAlive(ctx, c.metricsDBURL, experimentID, window)
}

// observedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan for a job's
// first sample — a search-window ceiling no real job's runtime could exceed, keeping the query cheap.
func (c *Controller) observedMaxLookback() time.Duration {
	return 14 * 24 * time.Hour
}

// observedElapsedHours returns how long experimentID has been confirmed alive, in hours — see
// metricsdb.ObservedElapsedHours for the correctness argument (multi-node, clock-skew,
// gap-capping; no stored StartedAt needed). GreptimeDB is the sole source of truth: a query
// error is returned so the caller retries next reconcile, never papered over with a guess.
func (c *Controller) observedElapsedHours(ctx context.Context, experimentID string, now time.Time) (float64, error) {
	return metricsdb.ObservedElapsedHours(ctx, c.metricsDBURL, experimentID, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
}

// observedAcceleratorCost returns experimentID's true accelerator cost, billed per accelerator
// type it actually ran on — see metricsdb.ObservedAcceleratorCost. Replaces the flat
// elapsedHours × count × Cost() formula so a job rescheduled mid-run bills each segment correctly.
func (c *Controller) observedAcceleratorCost(ctx context.Context, experimentID string, acceleratorCount int, now time.Time) (float64, error) {
	return metricsdb.ObservedAcceleratorCost(ctx, c.metricsDBURL, experimentID, acceleratorCount, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
}
