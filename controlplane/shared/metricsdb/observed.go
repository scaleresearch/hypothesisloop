package metricsdb

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// aliveHeartbeatMetric is written once per real observation of an experiment being alive —
// a node-agent CPU sample tagged with the pod's experiment ID, or (via IsAlive/
// ObservedElapsedHours also matching experiment_metric_value) a job-reported training metric.
// This file is the only place that reads or writes "how long has this experiment really run":
// every caller (silence detection, refund/eviction accounting, phase-2 boundary, the scheduler's
// Cancel handler) calls straight into GreptimeDB, so the answer is always recomputed from what's
// actually stored — no cache, in this process or any other, to fall out of sync or get lost on
// restart.
const aliveHeartbeatMetric = "experiment_alive_heartbeat"

// RecordObservation writes a single "experimentID was alive" sample at `at` — the agent's own
// collection time, not receipt time (see WriteGaugeAt). extraLabels (pod_uid, node, ...) are
// carried for debugging only; every read here collapses across them.
func RecordObservation(ctx context.Context, dbURL, experimentID string, at time.Time, extraLabels map[string]string) error {
	return WriteGaugesAt(ctx, dbURL, []GaugeSample{observationSample(experimentID, at, extraLabels)})
}

// Observation is one pod's "experimentID was alive" reading for RecordObservations' batch write —
// the same shape as RecordObservation's arguments, just collected up front so every pod a single
// push payload reports becomes one remote-write request instead of one per pod.
type Observation struct {
	ExperimentID string
	At           time.Time
	ExtraLabels  map[string]string
}

// RecordObservations writes every observation in one Prometheus remote-write request — the batch
// form of RecordObservation, for a caller (the node-agent push handler) that already has several
// pods' readings in hand from a single payload and would otherwise issue one HTTP round trip per
// pod for what is, from GreptimeDB's point of view, one write.
func RecordObservations(ctx context.Context, dbURL string, observations []Observation) error {
	samples := make([]GaugeSample, 0, len(observations))
	for _, o := range observations {
		samples = append(samples, observationSample(o.ExperimentID, o.At, o.ExtraLabels))
	}
	return WriteGaugesAt(ctx, dbURL, samples)
}

func observationSample(experimentID string, at time.Time, extraLabels map[string]string) GaugeSample {
	labels := map[string]string{"experiment_id": experimentID}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return GaugeSample{MetricName: aliveHeartbeatMetric, Labels: labels, Value: 1, At: at}
}

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

// acceleratorTypeMetric marks "experimentID's accelerator type became X" — written once by the
// control plane (job_watcher.go's onRunning) at the moment each admission decides a type, not
// polled or re-derived anywhere else. The control plane is the sole authority for this fact (it's
// the one that decided it), so this is the one place it's written; nothing else — not a pod
// label, not a node-agent read — duplicates it.
const acceleratorTypeMetric = "experiment_accelerator_type_active"

// RecordAcceleratorType marks experimentID as running on acceleratorType as of `at` — call this
// once per admission decision (initial admission, and again on every re-admission after a
// reschedule that lands on a different type), not periodically.
func RecordAcceleratorType(ctx context.Context, dbURL, experimentID, acceleratorType string, at time.Time) error {
	if acceleratorType == "" {
		return nil
	}
	labels := map[string]string{"experiment_id": experimentID, "accelerator_type": acceleratorType}
	return WriteGaugeAt(ctx, dbURL, acceleratorTypeMetric, labels, 1, at)
}

// acceleratorTypeChange is one "type became X as of this write" event, resolved from GreptimeDB
// with its real write timestamp (not a query-grid timestamp).
type acceleratorTypeChange struct {
	At   time.Time
	Type string
}

// acceleratorTypeChanges returns every accelerator-type write for experimentID in [since, now),
// sorted oldest first, with each write's own approximate timestamp (accurate to within `step`).
// PromQL has no "hold this value until it's next overwritten" primitive — a long last_over_time
// window just makes every past write "present" forever, which would show two types as
// simultaneously active after a reschedule instead of only the current one. So this queries with
// a *short* window (matched to `step`, i.e. only wide enough to catch a write at the very next
// grid point after it happens) to recover each write as a sparse, precisely-timed event, and
// acceleratorTypeAtGrid resolves "which type was active at grid point G" by picking the most
// recent event at or before G in Go — a stateless computation over freshly-queried events, thrown
// away after the request, not a cache.
func acceleratorTypeChanges(ctx context.Context, dbURL, experimentID string, since, now time.Time, step time.Duration) ([]acceleratorTypeChange, error) {
	promQL := fmt.Sprintf(`max by (accelerator_type) (last_over_time(%s{experiment_id=%q}[%s]))`,
		acceleratorTypeMetric, experimentID, promSeconds(step))
	series, err := QueryRange(ctx, dbURL, promQL, since, now, step)
	if err != nil {
		return nil, err
	}
	var changes []acceleratorTypeChange
	for _, s := range series {
		accType := s.Labels["accelerator_type"]
		if accType == "" {
			continue
		}
		for _, p := range s.Points {
			changes = append(changes, acceleratorTypeChange{At: p.Time, Type: accType})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].At.Before(changes[j].At) })
	return changes, nil
}

// acceleratorTypeAtGrid returns, for each `step`-spaced grid timestamp in [since, now), whichever
// accelerator_type was most recently recorded for experimentID at or before that point — see
// acceleratorTypeChanges for why this resolution has to happen here rather than in PromQL itself.
//
// changesSince must be earlier than since, because the type marker is written when the job is
// *admitted* (job_watcher.go's onRunning, which fires as soon as the Job has a non-terminated pod
// — Status.Active counts Pending pods) while `since` is the first moment the container was
// actually observed burning CPU. Pod scheduling and image pull sit between the two, so the marker
// that establishes a job's type is routinely minutes older than its first heartbeat: querying the
// type series only over [since, now] finds no events at all and bills the entire run as untyped,
// i.e. free. So the event scan starts at changesSince and the last type at or before `since` is
// carried forward onto the grid.
//
// A grid point before the *earliest* known event (possible if a marker write failed, or on a
// re-admission whose marker lands after the new pod's first heartbeat) is backfilled with that
// earliest type rather than dropped — an untyped alive point would otherwise be silently omitted
// from GPUHoursByType, under-billing exactly the interval the job was demonstrably running.
func acceleratorTypeAtGrid(ctx context.Context, dbURL, experimentID string, changesSince, since, now time.Time, step time.Duration) (map[int64]string, error) {
	changes, err := acceleratorTypeChanges(ctx, dbURL, experimentID, changesSince, now, step)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, nil
	}
	grid := make(map[int64]string)
	ci := 0
	current := changes[0].Type
	for t := since; !t.After(now); t = t.Add(step) {
		for ci < len(changes) && !changes[ci].At.After(t) {
			current = changes[ci].Type
			ci++
		}
		grid[t.Unix()] = current
	}
	return grid, nil
}

// GPUHoursByType returns, for experimentID, confirmed-alive hours bucketed by accelerator type —
// the direct answer to "how many GPU-hours per GPU type did this job consume", correct across a
// mid-run reschedule onto a different type: each alive grid point is billed under whichever type
// was actually active at that point, not one flat rate for the job's whole observed lifetime.
//
// An experiment with no accelerator-type marker at all within maxLookback (a CPU-only job) yields
// an empty map and therefore zero GPU cost — that is the only case where alive time goes unbilled.
// Once any marker exists, every alive grid point is attributed to some type (see
// acceleratorTypeAtGrid), so observed runtime can never be silently dropped from a GPU job's bill.
// Callers that need a total across types should use ObservedElapsedHours, which doesn't require a
// type label at all.
func GPUHoursByType(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, gapCap, step time.Duration) (map[string]float64, error) {
	if step <= 0 {
		return nil, nil
	}
	since, ok, err := FirstObserved(ctx, dbURL, experimentID, now, maxLookback, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.GPUHoursByType: %w", err)
	}
	if !ok || !now.After(since) {
		return nil, nil
	}
	aliveGrid, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, gapCap, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.GPUHoursByType: %w", err)
	}
	// Scan for type markers from the same horizon FirstObserved searched, not from `since`: the
	// marker predates the first heartbeat by however long the pod took to schedule and pull its
	// image (see acceleratorTypeAtGrid).
	typeGrid, err := acceleratorTypeAtGrid(ctx, dbURL, experimentID, now.Add(-maxLookback), since, now, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.GPUHoursByType: %w", err)
	}
	hours := make(map[string]float64)
	for ts := range aliveGrid {
		accType, ok := typeGrid[ts]
		if !ok {
			continue
		}
		hours[accType] += step.Seconds() / 3600
	}
	return hours, nil
}

// ObservedGPUCost returns experimentID's true GPU cost — Σ over each accelerator type it actually
// ran on of (hours on that type × gpuCount × that type's own rate) — replacing the old
// elapsedHours × gpuCount × exp.GPUType.Cost() flat-rate formula, which silently mischarged any
// job that changed accelerator type mid-run (a real scenario: a burst job preempted and later
// re-admitted can land on a different type than it started on). gpuCount is passed in rather than
// read from the type series because it doesn't vary by type — a job requests N accelerators and
// keeps requesting N regardless of which type fills that request.
func ObservedGPUCost(ctx context.Context, dbURL, experimentID string, gpuCount int, now time.Time, maxLookback, gapCap, step time.Duration) (float64, error) {
	byType, err := GPUHoursByType(ctx, dbURL, experimentID, now, maxLookback, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedGPUCost: %w", err)
	}
	var cost float64
	for accType, hours := range byType {
		// LookupCost, not Cost: this is final settlement, not a pre-flight estimate. A type
		// with no registered rate (removed from openresearch.yaml after the job was admitted
		// onto it, or corrupted metric data) must fail the whole computation rather than
		// silently settle those hours at the arbitrary 1.0 (T4) fallback rate — the caller
		// already treats an error here as "skip this refund pass, retry next reconcile",
		// exactly the same contract as any other query error.
		rate, ok := domain.GPUType(accType).LookupCost()
		if !ok {
			return 0, fmt.Errorf("metricsdb.ObservedGPUCost: experiment %s: no registered rate for accelerator type %q", experimentID, accType)
		}
		cost += hours * float64(gpuCount) * rate
	}
	return cost, nil
}
