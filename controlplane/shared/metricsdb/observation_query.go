package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// promSeconds formats a duration as a PromQL duration literal in whole seconds — the one unit
// that's never ambiguous regardless of magnitude.
func promSeconds(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}

// isAliveOn reports whether metric{labelKey=id} has a sample within `window` of now — a plain
// last_over_time instant query, deliberately simple PromQL (no subqueries, no `or`) since not
// every PromQL-compatible backend supports the fancier constructs.
func isAliveOn(ctx context.Context, dbURL, metric, labelKey, id string, window time.Duration) (bool, error) {
	promQL := fmt.Sprintf(`last_over_time(%s{%s=%q}[%s])`, metric, labelKey, id, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return false, err
	}
	return len(samples) > 0, nil
}

// IsAlive reports whether experimentID has been observed — a CPU heartbeat or a job-reported
// training metric — within `window` of now. Used by silence detection in place of an in-memory
// last-seen map: stateless, so it gives the same answer regardless of which process asks, and
// survives a restart with no warm-up gap.
func IsAlive(ctx context.Context, dbURL, experimentID string, window time.Duration) (bool, error) {
	heartbeat, err := isAliveOn(ctx, dbURL, aliveHeartbeatMetric, "experiment_id", experimentID, window)
	if err != nil {
		return false, fmt.Errorf("metricsdb.IsAlive: heartbeat: %w", err)
	}
	if heartbeat {
		return true, nil
	}
	metric, err := isAliveOn(ctx, dbURL, "experiment_metric_value", "job_id", experimentID, window)
	if err != nil {
		return false, fmt.Errorf("metricsdb.IsAlive: experiment_metric_value: %w", err)
	}
	return metric, nil
}

// aliveGridPoints returns the set of `step`-spaced grid timestamps (Unix seconds) between since
// and now at which metric{labelKey=id} shows a sample within the preceding gapCap — the query
// itself implements the gap cap via last_over_time's range-vector window, so a genuine gap
// (reschedule, node death, partition) longer than gapCap simply produces no grid point there;
// nothing to special-case in Go.
func aliveGridPoints(ctx context.Context, dbURL, metric, labelKey, id string, since, now time.Time, gapCap, step time.Duration) (map[int64]bool, error) {
	promQL := fmt.Sprintf(`max(last_over_time(%s{%s=%q}[%s]))`, metric, labelKey, id, promSeconds(gapCap))
	series, err := QueryRange(ctx, dbURL, promQL, since, now, step)
	if err != nil {
		return nil, err
	}
	grid := make(map[int64]bool)
	for _, s := range series {
		for _, p := range s.Points {
			grid[p.Time.Unix()] = true
		}
	}
	return grid, nil
}

// unionAliveGrid merges the heartbeat and training-metric grids for experimentID over [since, now)
// — the shared core of FirstObserved and ObservedElapsedHours, so both answer from exactly the same
// definition of "alive".
func unionAliveGrid(ctx context.Context, dbURL, experimentID string, since, now time.Time, gapCap, step time.Duration) (map[int64]bool, error) {
	heartbeatGrid, err := aliveGridPoints(ctx, dbURL, aliveHeartbeatMetric, "experiment_id", experimentID, since, now, gapCap, step)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: %w", err)
	}
	metricGrid, err := aliveGridPoints(ctx, dbURL, "experiment_metric_value", "job_id", experimentID, since, now, gapCap, step)
	if err != nil {
		return nil, fmt.Errorf("experiment_metric_value: %w", err)
	}
	union := make(map[int64]bool, len(heartbeatGrid)+len(metricGrid))
	for k := range heartbeatGrid {
		union[k] = true
	}
	for k := range metricGrid {
		union[k] = true
	}
	return union, nil
}

// FirstObserved returns the timestamp of the earliest sample (heartbeat or training metric) for
// experimentID within maxLookback of now — GreptimeDB's own answer to "where did that job start",
// with no Postgres-stored StartedAt column involved. maxLookback just bounds how far back the
// query scans (a job can't have been running longer than the platform retains metrics for, or
// longer than any reasonable job ever runs); it is not a clock the accounting trusts, only a
// search window. Returns ok=false if no sample exists in that window (job never started, or
// already aged out of retention).
func FirstObserved(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, step time.Duration) (t time.Time, ok bool, err error) {
	if step <= 0 {
		return time.Time{}, false, nil
	}
	since := now.Add(-maxLookback)
	grid, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, step, step)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("metricsdb.FirstObserved: %w", err)
	}
	if len(grid) == 0 {
		return time.Time{}, false, nil
	}
	var min int64
	for k := range grid {
		if min == 0 || k < min {
			min = k
		}
	}
	return time.Unix(min, 0).UTC(), true, nil
}

// ObservedElapsedHours returns how long experimentID has been confirmed alive, in hours — the sole
// input to every quota-consumption calculation. It is a pure function of what's stored in
// GreptimeDB: two processes (or the same process before and after a restart) calling this for the
// same experiment always get the same answer, and no wall-clock start time is required as input —
// the start boundary is itself derived from the metrics (see FirstObserved), bounded by
// maxLookback so the range query stays cheap. A job with no observations at all in maxLookback
// (never started, or aged out of retention) reports zero elapsed hours, not an error.
//
// Correctness against the failure modes this exists for:
//   - An ordinary tick-to-tick gap, or a brief delivery hiccup (a retried push, a second or two
//     of daemonset restart), is shorter than gapCap, so every grid point spanning it still
//     resolves to "alive" via last_over_time's own lookback — nothing is lost.
//   - A genuine gap (preempted and rescheduled elsewhere, node died, a network partition) exceeds
//     gapCap: the grid points inside it are simply absent from the result, not charged.
//   - A distributed job's pods, spread across nodes with their own local clocks, are unioned:
//     grid points come from `max(...)` over every matching series, so if *any* pod was alive at a
//     given point the experiment counts as alive there, and wall-clock time is counted once, not
//     once per pod. Small inter-node clock skew shifts which exact grid point a given sample
//     lands on by at most the skew, which is negligible next to gapCap (seconds vs. the report
//     interval multiples gapCap is built from) — this assumes nodes are roughly NTP-synced, the
//     standard baseline for any real cluster, not sub-second-accurate. No process needs to agree
//     on "what time it is" beyond that: every timestamp compared here comes from GreptimeDB's own
//     samples, not a caller-supplied clock reading.
//   - Both the CPU heartbeat series and job-reported training metrics count as "alive" signals
//     (unioned, not required together), so a job between training-metric reports isn't mistaken
//     for not running, and a job that never emits training metrics at all still accrues time from
//     its CPU heartbeat alone.
func ObservedElapsedHours(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, gapCap, step time.Duration) (float64, error) {
	if step <= 0 {
		return 0, nil
	}
	since, ok, err := FirstObserved(ctx, dbURL, experimentID, now, maxLookback, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedElapsedHours: %w", err)
	}
	if !ok || !now.After(since) {
		return 0, nil
	}
	union, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedElapsedHours: %w", err)
	}
	return step.Seconds() * float64(len(union)) / 3600, nil
}
