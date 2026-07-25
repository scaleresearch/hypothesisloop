package metricsdb

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// acceleratorTypeMetric marks "experimentID's accelerator type became X" — written once by the
// control plane (job_watcher.go's onRunning) when admission decides a type. The control plane is
// the sole authority for this fact; nothing else duplicates it.
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
// window would make every past write "present" forever, showing two types simultaneously active
// after a reschedule. So this queries with a short window (matched to `step`) to recover each
// write as a sparse, precisely-timed event; acceleratorTypeAtGrid then resolves "which type was
// active at grid point G" by picking the most recent event at or before G, statelessly, per
// request.
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
// acceleratorTypeChanges for why this resolution happens here rather than in PromQL.
//
// changesSince must be earlier than since: the type marker is written on admission
// (job_watcher.go's onRunning, as soon as the Job has a non-terminated pod), while `since` is the
// first moment the container was observed burning CPU. Pod scheduling and image pull sit between
// the two, so the marker is routinely minutes older than the first heartbeat — querying only
// [since, now] would find no events and bill the run as untyped (free). So the event scan starts
// at changesSince and the last type at or before `since` carries forward onto the grid.
//
// A grid point before the earliest known event (marker write failed, or a re-admission marker
// lands after the new pod's first heartbeat) is backfilled with that earliest type rather than
// dropped, so an alive point is never silently omitted from AcceleratorHoursByType.
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

// AcceleratorHoursByType returns, for experimentID, confirmed-alive hours bucketed by accelerator
// type — correct across a mid-run reschedule onto a different type: each alive grid point is
// billed under whichever type was actually active there, not one flat rate for the whole run.
//
// An experiment with no accelerator-type marker at all within maxLookback (a CPU-only job) yields
// an empty map and zero accelerator cost — the only case where alive time goes unbilled. Once any
// marker exists, every alive grid point is attributed to some type (see acceleratorTypeAtGrid).
// Callers needing a total across types should use ObservedElapsedHours instead.
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
	// Scan from the same horizon FirstObserved searched, not from `since`: the marker predates
	// the first heartbeat by however long pod scheduling/image pull took (see acceleratorTypeAtGrid).
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

// ObservedAcceleratorCost returns experimentID's true accelerator cost — Σ over each accelerator
// type it actually ran on of (hours on that type × acceleratorCount × that type's rate) —
// replacing the old flat-rate formula, which mischarged any job that changed accelerator type
// mid-run (e.g. a burst job preempted and re-admitted onto a different type). acceleratorCount is
// passed in rather than read from the type series because it doesn't vary by type.
func ObservedAcceleratorCost(ctx context.Context, dbURL, experimentID string, acceleratorCount int, now time.Time, maxLookback, gapCap, step time.Duration) (float64, error) {
	byType, err := AcceleratorHoursByType(ctx, dbURL, experimentID, now, maxLookback, gapCap, step)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.ObservedAcceleratorCost: %w", err)
	}
	var cost float64
	for accType, hours := range byType {
		// LookupCost, not Cost: this is final settlement, not an estimate. A type with no
		// registered rate (removed from config after admission, or corrupted metric data) must
		// fail the computation rather than silently settle at the 1.0 (T4) fallback — the
		// caller treats an error here as "skip this refund pass, retry next reconcile".
		rate, ok := domain.AcceleratorType(accType).LookupCost()
		if !ok {
			return 0, fmt.Errorf("metricsdb.ObservedAcceleratorCost: experiment %s: no registered rate for accelerator type %q", experimentID, accType)
		}
		cost += hours * float64(acceleratorCount) * rate
	}
	return cost, nil
}
