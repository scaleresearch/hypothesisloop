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

// HasEverReportedMetric reports whether experimentID has produced even one job-reported metric
// sample within maxLookback. Deliberately ignores the heartbeat: the heartbeat proves the pod
// exists, this proves the workload's own reporting path works. A job that never produced a
// single sample is not "quiet since it hung" — its metrics path is broken (wrong URL, a stale
// helper baked into the image, a swallowed exception), which needs a different fix than a hung
// process and so is worth telling apart at eviction time.
func HasEverReportedMetric(ctx context.Context, dbURL, experimentID string, maxLookback time.Duration) (bool, error) {
	reported, err := isAliveOn(ctx, dbURL, "experiment_metric_value", "job_id", experimentID, maxLookback)
	if err != nil {
		return false, fmt.Errorf("metricsdb.HasEverReportedMetric: %w", err)
	}
	return reported, nil
}

// aliveGridPoints returns the set of `step`-spaced grid timestamps (Unix seconds) between since
// and now at which metric{labelKey=id} shows a sample within the preceding gapCap. The query
// implements the gap cap via last_over_time's range-vector window, so a genuine gap (reschedule,
// node death, partition) longer than gapCap simply produces no grid point there.
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

// LastNotAlive returns the latest `step`-spaced grid timestamp in [from, to) at which the
// node-agent published no liveness heartbeat for experimentID — i.e. the end of the most recent
// stretch during which the job was not running at all.
//
// The heartbeat follows the pod, not the training process, which is what separates "this job was
// not running" (requeued after preemption, between pods) from "this job was up and simply did not
// report the metrics it owes". An absence of training metrics alone cannot tell those apart, and
// only the first is excusable — a job that is alive and silent must stay accountable.
// gapCap is the longest heartbeat gap that still counts as continuous presence, so ordinary
// reporting jitter is not mistaken for the job being down — pass the same value the rest of the
// observation code uses (Controller.observedGapCap), never the raw step, or every jittered
// interval reads as not-alive and the caller's decline check stops firing at all.
func LastNotAlive(ctx context.Context, dbURL, experimentID string, from, to time.Time, gapCap, step time.Duration) (time.Time, bool, error) {
	if step <= 0 || !to.After(from) {
		return time.Time{}, false, nil
	}
	grid, err := aliveGridPoints(ctx, dbURL, aliveHeartbeatMetric, "experiment_id", experimentID, from, to, gapCap, step)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("metricsdb.LastNotAlive: %w", err)
	}
	var last time.Time
	found := false
	for t := from; t.Before(to); t = t.Add(step) {
		if !grid[t.Unix()] {
			last = t
			found = true
		}
	}
	return last, found, nil
}

// unionAliveGrid merges the heartbeat and training-metric grids for experimentID over
// [since, now) — the shared core of FirstObserved and ObservedElapsedHours, so both answer from
// the same definition of "alive".
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

// declaredMetricSpread reports, for one declared metric key, whether experimentID has posted any
// sample within window (reported) and whether its value actually moved (changed). Two combined
// PromQL queries: count_over_time settles presence and whether there's even enough evidence to
// judge (a single point can't distinguish "just started" from "stuck at a constant" — see
// AnyDeclaredMetricChanged); max_over_time - min_over_time over the same series is non-zero iff
// the value moved between at least two points.
func declaredMetricSpread(ctx context.Context, dbURL, experimentID, metricKey string, window time.Duration) (reported, changed bool, err error) {
	countQL := fmt.Sprintf(`count_over_time(experiment_metric_value{job_id=%q, metric_name=%q}[%s])`,
		experimentID, metricKey, promSeconds(window))
	counts, err := QueryVector(ctx, dbURL, countQL)
	if err != nil {
		return false, false, err
	}
	if len(counts) == 0 || counts[0].Value == 0 {
		return false, false, nil
	}
	if counts[0].Value < 2 {
		// One sample only: real progress can't be ruled out yet, and it can't be confirmed
		// either — treat exactly like "not reported", not like "stuck".
		return false, false, nil
	}
	spreadQL := fmt.Sprintf(
		`max_over_time(experiment_metric_value{job_id=%q, metric_name=%q}[%s]) - min_over_time(experiment_metric_value{job_id=%q, metric_name=%q}[%s])`,
		experimentID, metricKey, promSeconds(window), experimentID, metricKey, promSeconds(window),
	)
	samples, err := QueryVector(ctx, dbURL, spreadQL)
	if err != nil {
		return false, false, err
	}
	if len(samples) == 0 {
		return false, false, nil
	}
	for _, s := range samples {
		if s.Value != 0 {
			return true, true, nil
		}
	}
	return true, false, nil
}

// AnyDeclaredMetricChanged reports whether a platform experiment's declared metrics are showing
// real progress for experimentID, as opposed to merely arriving. reported=false means none of
// metricKeys has posted a single sample within window yet — early warmup or a broken reporting
// path, not evidence of a stuck job, so callers must not treat that as silence. reported=true,
// changed=false means every declared metric that did report held a single constant value the
// whole window: a job whose training loop hung but keeps re-emitting the same point (e.g. a
// cached last-value) would otherwise look perpetually alive to naive "did any sample arrive"
// silence detection. changed=true as soon as any one declared metric moved.
func AnyDeclaredMetricChanged(ctx context.Context, dbURL, experimentID string, metricKeys []string, window time.Duration) (reported, changed bool, err error) {
	for _, key := range metricKeys {
		keyReported, keyChanged, err := declaredMetricSpread(ctx, dbURL, experimentID, key, window)
		if err != nil {
			return false, false, fmt.Errorf("metricsdb.AnyDeclaredMetricChanged: %q: %w", key, err)
		}
		if keyReported {
			reported = true
		}
		if keyChanged {
			return true, true, nil
		}
	}
	return reported, false, nil
}

// FirstObserved returns the timestamp of the earliest sample (heartbeat or training metric) for
// experimentID within maxLookback of now — GreptimeDB's own answer to "where did that job
// start", with no Postgres-stored StartedAt column involved. maxLookback only bounds how far
// back the query scans; it's a search window, not a clock the accounting trusts. Returns
// ok=false if no sample exists in that window (job never started, or aged out of retention).
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

// ObservedElapsedHours returns how long experimentID has been confirmed alive, in hours — the
// sole input to every quota-consumption calculation. It's a pure function of what's stored in
// GreptimeDB: two processes (or the same process before/after a restart) always get the same
// answer, with no wall-clock start time needed as input — the start boundary is itself derived
// from the metrics (see FirstObserved), bounded by maxLookback so the range query stays cheap.
// A job with no observations at all in maxLookback reports zero elapsed hours, not an error.
//
// Handles these cases:
//   - A brief gap (tick-to-tick, retried push, daemonset restart blip) shorter than gapCap still
//     resolves to "alive" via last_over_time's own lookback — nothing lost.
//   - A genuine gap (preemption, node death, partition) exceeding gapCap simply produces no grid
//     points there, so it isn't charged.
//   - A distributed job's pods are unioned via `max(...)` over every matching series, so if any
//     pod was alive at a point the experiment counts as alive there, counted once not once per
//     pod. This assumes nodes are roughly NTP-synced (the standard cluster baseline); every
//     timestamp compared here comes from GreptimeDB's own samples, not a caller-supplied clock.
//   - Both the CPU heartbeat and job-reported training-metric series count as "alive" (unioned,
//     not required together), so a job between metric reports isn't mistaken for not running,
//     and a job that never emits training metrics still accrues time from its heartbeat alone.
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
