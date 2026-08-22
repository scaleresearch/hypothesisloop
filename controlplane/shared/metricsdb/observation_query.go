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
	metric, err := isAliveOn(ctx, dbURL, ExperimentMetricValue, "job_id", experimentID, window)
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
	reported, err := isAliveOn(ctx, dbURL, ExperimentMetricValue, "job_id", experimentID, maxLookback)
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
	metricGrid, err := aliveGridPoints(ctx, dbURL, ExperimentMetricValue, "job_id", experimentID, since, now, gapCap, step)
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
	count, err := metricSampleCount(ctx, dbURL, experimentID, metricKey, window)
	if err != nil {
		return false, false, err
	}
	if count < 2 {
		// Zero samples, or one sample only: with one point real progress can't be ruled out yet,
		// and it can't be confirmed either — treat exactly like "not reported", not like "stuck".
		return false, false, nil
	}
	sel := fmt.Sprintf(`%s{job_id=%q, metric_name=%q}[%s]`, ExperimentMetricValue, experimentID, metricKey, promSeconds(window))
	spreadQL := fmt.Sprintf(`max_over_time(%s) - min_over_time(%s)`, sel, sel)
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

// AnyDeclaredMetricReported reports whether experimentID has posted even one sample of any of
// metricKeys within window. Deliberately separate from AnyDeclaredMetricChanged: that one folds a
// single sample into reported=false, because one point cannot prove or disprove progress. For
// "has this job ever reported at all", one point is a complete answer — and conflating the two
// would condemn a job that reported once as never having reported.
func AnyDeclaredMetricReported(ctx context.Context, dbURL, experimentID string, metricKeys []string, window time.Duration) (bool, error) {
	for _, key := range metricKeys {
		count, err := metricSampleCount(ctx, dbURL, experimentID, key, window)
		if err != nil {
			return false, fmt.Errorf("metricsdb.AnyDeclaredMetricReported: %q: %w", key, err)
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

// metricSampleCount is how many samples of one declared metric experimentID posted within window.
// Shared because "did this job report at all" and "did the value move" both start from the same
// count query, and had drifted into two separately-built copies of it.
func metricSampleCount(ctx context.Context, dbURL, experimentID, metricKey string, window time.Duration) (float64, error) {
	promQL := fmt.Sprintf(`count_over_time(%s{job_id=%q, metric_name=%q}[%s])`,
		ExperimentMetricValue, experimentID, metricKey, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return 0, err
	}
	var max float64
	for _, s := range samples {
		if s.Value > max {
			max = s.Value
		}
	}
	return max, nil
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
	b, err := sampleBounds(ctx, dbURL, experimentID, now.Add(-maxLookback), now, step)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("metricsdb.FirstObserved: %w", err)
	}
	return b.first, b.ok, nil
}

// sampleBounds is the earliest and latest grid timestamp backed by a real sample in [since, now).
// Asked with a range selector equal to step so a point appears only where a sample genuinely
// landed, never where last_over_time carried one forward.
//
// Both ends come from one grid because both callers want the same query: FirstObserved needs the
// minimum to place the start boundary, and elapsed-hours counting needs the maximum to know where
// counting stops being observation and starts being extrapolation. Fetching them separately meant
// querying the same two series twice with identical parameters.
type sampleRange struct {
	first, last time.Time
	ok          bool
}

func sampleBounds(ctx context.Context, dbURL, experimentID string, since, now time.Time, step time.Duration) (sampleRange, error) {
	if step <= 0 || !now.After(since) {
		return sampleRange{}, nil
	}
	grid, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, step, step)
	if err != nil {
		return sampleRange{}, err
	}
	if len(grid) == 0 {
		return sampleRange{}, nil
	}
	var min, max int64
	for k := range grid {
		if min == 0 || k < min {
			min = k
		}
		if k > max {
			max = k
		}
	}
	return sampleRange{first: time.Unix(min, 0).UTC(), last: time.Unix(max, 0).UTC(), ok: true}, nil
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
	hours, _, err := ObservedElapsed(ctx, dbURL, experimentID, now, maxLookback, gapCap, step)
	return hours, err
}

// ObservedElapsed is ObservedElapsedHours plus whether the job has ever been observed at all —
// the two facts every billing caller needs, from one pass over the metrics. Zero hours is
// ambiguous on its own: a job seen once and a job never seen both report 0, and callers bill them
// differently (a never-seen job keeps its admission estimate). Asking FirstObserved separately to
// tell them apart re-ran a query this already performs.
func ObservedElapsed(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, gapCap, step time.Duration) (float64, bool, error) {
	if step <= 0 {
		return 0, false, nil
	}
	b, err := sampleBounds(ctx, dbURL, experimentID, now.Add(-maxLookback), now, step)
	if err != nil {
		return 0, false, fmt.Errorf("metricsdb.ObservedElapsed: %w", err)
	}
	if !b.ok || !now.After(b.first) {
		return 0, b.ok, nil
	}
	hours, err := elapsedHours(ctx, dbURL, experimentID, b.first, now, gapCap, step, b.last)
	if err != nil {
		return 0, false, fmt.Errorf("metricsdb.ObservedElapsed: %w", err)
	}
	return hours, true, nil
}

// ObservedElapsedHoursSince is ObservedElapsedHours over an explicit start boundary, for the one
// caller that must not measure from the job's first-ever observation: preemption rescales a
// victim's remaining estimate, and a job on its second stint has already had its estimate reduced
// once, so charging it again for the hours of its first stint would subtract the same time twice.
func ObservedElapsedHoursSince(ctx context.Context, dbURL, experimentID string, since, now time.Time, gapCap, step time.Duration) (float64, error) {
	if step <= 0 || !now.After(since) {
		return 0, nil
	}
	b, err := sampleBounds(ctx, dbURL, experimentID, since, now, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedElapsedHoursSince: %w", err)
	}
	if !b.ok {
		return 0, nil
	}
	return elapsedHours(ctx, dbURL, experimentID, since, now, gapCap, step, b.last)
}

// elapsedHours counts the alive grid over [since, now), stopping at last — the newest real
// sample, supplied by the caller that already knows it.
func elapsedHours(ctx context.Context, dbURL, experimentID string, since, now time.Time, gapCap, step time.Duration, last time.Time) (float64, error) {
	union, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.elapsedHours: %w", err)
	}
	// Grid points are boundaries, not durations: n points delimit n-1 intervals of `step`. Adding
	// one more counts the closing boundary as a whole interval, which for a job observed exactly
	// once bills a full step for a single instant.
	//
	// The gap cap does the rest of the damage on its own. last_over_time keeps emitting points for
	// gapCap after the final real sample, which mid-run is exactly right — a blip shorter than
	// gapCap is a reporting hiccup, not a stopped job. Past the last sample there is nothing to
	// bridge to, so the same rule invents time nobody observed, and how much depends only on how
	// long after the job died the query happens to run. Billing must not depend on that: settling
	// promptly and settling an hour late have to produce the same number. So trailing points are
	// cut back to one step past the newest real sample — the same one step of benefit of the doubt
	// every other interval gets, and no more.
	if len(union) == 0 {
		return 0, nil
	}
	counted := 0
	cutoff := last.Unix()
	for t := range union {
		if t <= cutoff {
			counted++
		}
	}
	if counted <= 1 {
		return 0, nil
	}
	return step.Seconds() * float64(counted-1) / 3600, nil
}

// ObservedStintElapsedHours returns how long experimentID has been confirmed alive since it last
// started running — the current stint only, not its whole life.
//
// The two differ exactly when a job has been requeued: preemption returns a victim to QUEUED with
// its duration and all four resource estimates rescaled down to what is left. On the next
// preemption that already-shortened estimate is the baseline, so the hours to subtract are the
// ones consumed since it resumed. Measuring from the job's first observation instead makes the
// two bases disagree, and a job preempted twice has its remaining estimate — and therefore the
// budget it reserves — collapse to the floor while it still has most of its work to do.
//
// A job that has never been down reports its full observed elapsed time, which is the same
// number by definition.
func ObservedStintElapsedHours(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, gapCap, step time.Duration) (float64, error) {
	if step <= 0 {
		return 0, nil
	}
	first, ok, err := FirstObserved(ctx, dbURL, experimentID, now, maxLookback, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedStintElapsedHours: %w", err)
	}
	if !ok || !now.After(first) {
		return 0, nil
	}
	// The end of the most recent not-running stretch is where this stint began. A mid-run gap
	// longer than gapCap (node death, partition) also resets it, which undercounts the hours to
	// subtract and so leaves the reservation slightly larger — the safe direction for a number
	// that gates admission.
	downAt, wasDown, err := LastNotAlive(ctx, dbURL, experimentID, first, now, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedStintElapsedHours: %w", err)
	}
	since := first
	if wasDown {
		since = downAt.Add(step)
	}
	return ObservedElapsedHoursSince(ctx, dbURL, experimentID, since, now, gapCap, step)
}
