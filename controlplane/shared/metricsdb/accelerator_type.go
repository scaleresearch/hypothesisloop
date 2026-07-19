package metricsdb

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
// from AcceleratorHoursByType, under-billing exactly the interval the job was demonstrably running.
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

// AcceleratorHoursByType returns, for experimentID, confirmed-alive hours bucketed by accelerator type —
// the direct answer to "how many accelerator-hours per accelerator type did this job consume", correct across a
// mid-run reschedule onto a different type: each alive grid point is billed under whichever type
// was actually active at that point, not one flat rate for the job's whole observed lifetime.
//
// An experiment with no accelerator-type marker at all within maxLookback (a CPU-only job) yields
// an empty map and therefore zero accelerator cost — that is the only case where alive time goes unbilled.
// Once any marker exists, every alive grid point is attributed to some type (see
// acceleratorTypeAtGrid), so observed runtime can never be silently dropped from an accelerator job's bill.
// Callers that need a total across types should use ObservedElapsedHours, which doesn't require a
// type label at all.
func AcceleratorHoursByType(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback, gapCap, step time.Duration) (map[string]float64, error) {
	if step <= 0 {
		return nil, nil
	}
	since, ok, err := FirstObserved(ctx, dbURL, experimentID, now, maxLookback, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.AcceleratorHoursByType: %w", err)
	}
	if !ok || !now.After(since) {
		return nil, nil
	}
	aliveGrid, err := unionAliveGrid(ctx, dbURL, experimentID, since, now, gapCap, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.AcceleratorHoursByType: %w", err)
	}
	// Scan for type markers from the same horizon FirstObserved searched, not from `since`: the
	// marker predates the first heartbeat by however long the pod took to schedule and pull its
	// image (see acceleratorTypeAtGrid).
	typeGrid, err := acceleratorTypeAtGrid(ctx, dbURL, experimentID, now.Add(-maxLookback), since, now, step)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.AcceleratorHoursByType: %w", err)
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

// ObservedAcceleratorCost returns experimentID's true accelerator cost — Σ over each accelerator type it actually
// ran on of (hours on that type × acceleratorCount × that type's own rate) — replacing the old
// elapsedHours × acceleratorCount × exp.AcceleratorType.Cost() flat-rate formula, which silently mischarged any
// job that changed accelerator type mid-run (a real scenario: a burst job preempted and later
// re-admitted can land on a different type than it started on). acceleratorCount is passed in rather than
// read from the type series because it doesn't vary by type — a job requests N accelerators and
// keeps requesting N regardless of which type fills that request.
func ObservedAcceleratorCost(ctx context.Context, dbURL, experimentID string, acceleratorCount int, now time.Time, maxLookback, gapCap, step time.Duration) (float64, error) {
	byType, err := AcceleratorHoursByType(ctx, dbURL, experimentID, now, maxLookback, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedAcceleratorCost: %w", err)
	}
	var cost float64
	for accType, hours := range byType {
		// LookupCost, not Cost: this is final settlement, not a pre-flight estimate. A type
		// with no registered rate (removed from openresearch.yaml after the job was admitted
		// onto it, or corrupted metric data) must fail the whole computation rather than
		// silently settle those hours at the arbitrary 1.0 (T4) fallback rate — the caller
		// already treats an error here as "skip this refund pass, retry next reconcile",
		// exactly the same contract as any other query error.
		rate, ok := domain.AcceleratorType(accType).LookupCost()
		if !ok {
			return 0, fmt.Errorf("metricsdb.ObservedAcceleratorCost: experiment %s: no registered rate for accelerator type %q", experimentID, accType)
		}
		cost += hours * float64(acceleratorCount) * rate
	}
	return cost, nil
}
