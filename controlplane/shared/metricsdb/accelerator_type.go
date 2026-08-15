package metricsdb

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// acceleratorTypeMetric marks "experimentID's accelerator type became X". Written continuously
// by the cluster agent's status push while a job is observed running (see
// metricsdb.RecordJobStatuses) — the only writer; nothing on the control-plane side duplicates
// it. Read back by LatestAcceleratorType for job_watcher's onRunning admission-consistency check
// (did the job actually land on the flavor admission decided?). It is not used for billing —
// see services/settlement.Settler.Settle and controller.observedAcceleratorCost for why a live
// per-type-and-grid-point observation proved too unreliable for that (a job shorter than one
// poll tick never gets an observation at all) and what replaced it.
const acceleratorTypeMetric = "experiment_accelerator_type_active"

// RecordAcceleratorType marks experimentID as running on acceleratorType as of `at`. Exported
// for tests/tools that want to seed this metric directly; production writes go through
// RecordJobStatuses instead.
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
