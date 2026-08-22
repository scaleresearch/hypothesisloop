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
	span, err := ObserveSpan(ctx, dbURL, experimentID, now.Add(-maxLookback), now, step)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("metricsdb.FirstObserved: %w", err)
	}
	return span.First, span.Observed, nil
}

// ObservedElapsedHours returns how long experimentID has been confirmed alive, in hours — the
// sole input to every quota-consumption calculation. A pure function of what is stored in
// GreptimeDB (see ObserveSpan), so two processes, or the same process before and after a restart,
// always get the same answer, and no wall-clock start time is needed as input. maxLookback only
// bounds how far back the query scans. A job with no observations at all reports zero, not an error.
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
	span, err := ObserveSpan(ctx, dbURL, experimentID, now.Add(-maxLookback), now, gapCap)
	if err != nil {
		return 0, false, fmt.Errorf("metricsdb.ObservedElapsed: %w", err)
	}
	return span.Total.Hours(), span.Observed, nil
}

// ObservedElapsedHoursSince is ObservedElapsedHours over an explicit start boundary, for the one
// caller that must not measure from the job's first-ever observation: preemption rescales a
// victim's remaining estimate, and a job on its second stint has already had its estimate reduced
// once, so charging it again for the hours of its first stint would subtract the same time twice.
func ObservedElapsedHoursSince(ctx context.Context, dbURL, experimentID string, since, now time.Time, gapCap, step time.Duration) (float64, error) {
	span, err := ObserveSpan(ctx, dbURL, experimentID, since, now, gapCap)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedElapsedHoursSince: %w", err)
	}
	return span.Total.Hours(), nil
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
	span, err := ObserveSpan(ctx, dbURL, experimentID, now.Add(-maxLookback), now, gapCap)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedStintElapsedHours: %w", err)
	}
	return span.Stint.Hours(), nil
}
