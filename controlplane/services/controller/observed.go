package controller

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

// observedAcceleratorCost returns exp's accelerator cost so far: observed elapsed hours ×
// accelerator count × exp.AcceleratorType's registered rate. Billed flat at exp.AcceleratorType
// — the flavor admission already recorded as authoritative in Postgres (see domain.Experiment's
// AcceleratorType doc) — rather than a live per-type observation from the metrics store. That
// mechanism could bill a mid-run reschedule onto a different type correctly segment-by-segment,
// but required catching a job's RUNNING phase within a poll window to ever record anything; for
// jobs shorter than one poll tick it silently zeroed the whole run's accelerator cost (see
// services/settlement.Settler.Settle). This can't ever silently zero out, at the cost of slightly
// undercharging the rare mid-run type change.
func (c *Controller) observedAcceleratorCost(ctx context.Context, exp *domain.Experiment, now time.Time) (float64, error) {
	if exp.AcceleratorCount <= 0 || exp.AcceleratorType == "" {
		return 0, nil
	}
	hours, err := c.observedElapsedHours(ctx, exp.ID, now)
	if err != nil {
		return 0, err
	}
	// The same formula settlement bills by: the rate frozen into the reservation at admission,
	// not the live catalog. A mid-run rate change (or a retired flavor, which made the live
	// lookup fail and the job's cost silently drop out of the boundary) otherwise puts stage
	// progress and the budget agents are actually billed against on different numbers.
	return exp.RatedCost(exp.EstimatedCostAccH, hours), nil
}
