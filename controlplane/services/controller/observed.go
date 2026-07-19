package controller

import (
	"context"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// observedGapCap is the longest gap between two real observations that still counts as
// continuous presence — reused by every stateless metricsdb query below so silence detection and
// observed-cost accounting agree on what "still alive" means.
func (c *Controller) observedGapCap() time.Duration {
	return time.Duration(c.silenceMultiplier * float64(c.defaultReportInterval))
}

// observedStep is the grid resolution used to evaluate ObservedElapsedHours — fine enough to
// land inside every real report interval, coarse enough not to make the query expensive over a
// multi-hour experiment.
func (c *Controller) observedStep() time.Duration {
	if c.defaultReportInterval > 0 {
		return c.defaultReportInterval
	}
	return 10 * time.Second
}

// isAlive reports whether experimentID has a real observation (a node-agent heartbeat or a
// job-reported metric) within `window` of now — a stateless GreptimeDB query, not an in-memory
// last-seen map, so every process asking gets the same answer and nothing needs warming up after
// a restart.
func (c *Controller) isAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error) {
	return metricsdb.IsAlive(ctx, c.metricsDBURL, experimentID, window)
}

// observedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan to find a job's
// first sample — not a trusted clock, just a search-window ceiling no real job's runtime could
// exceed, so the range query stays cheap.
func (c *Controller) observedMaxLookback() time.Duration {
	return 14 * 24 * time.Hour
}

// observedElapsedHours returns how long experimentID has been confirmed alive, in hours — see
// metricsdb.ObservedElapsedHours for the full correctness argument (multi-node, clock-skew,
// gap-capping, and why no stored StartedAt is needed: the start boundary is derived from the
// metrics themselves). GreptimeDB is the sole source of truth with no fallback: a query error is
// returned to the caller to skip this write and retry on the next reconcile pass, never papered
// over with a wall-clock guess.
func (c *Controller) observedElapsedHours(ctx context.Context, experimentID string, now time.Time) (float64, error) {
	return metricsdb.ObservedElapsedHours(ctx, c.metricsDBURL, experimentID, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
}

// observedAcceleratorCost returns experimentID's true accelerator cost, billed per accelerator type it actually
// ran on — see metricsdb.ObservedAcceleratorCost. This replaces elapsedHours × acceleratorCount × exp.AcceleratorType.Cost()
// everywhere a job's real accelerator spend is computed, so a job rescheduled onto a different type
// mid-run is billed correctly for each segment instead of one flat rate for its whole lifetime.
func (c *Controller) observedAcceleratorCost(ctx context.Context, experimentID string, acceleratorCount int, now time.Time) (float64, error) {
	return metricsdb.ObservedAcceleratorCost(ctx, c.metricsDBURL, experimentID, acceleratorCount, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
}
