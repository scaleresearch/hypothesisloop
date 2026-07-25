package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// experimentNodeMetric marks "experimentID's job is running on physical node X" — written once
// per status report by the control plane (clusteragentapi.Handler.PushStatus), straight from
// what cluster-agent observed (workload.JobWorkloadClient.ResolveAdmittedAcceleratorType). This
// is the sole record of job→physical-node attribution anywhere in the system: it is not also
// stored in Postgres — per this repo's "metrics only in the metrics store, no duplicates between
// relational db and metrics storage" rule, since this is
// exactly the kind of fact (observed at runtime, needed for post-hoc traceability, never used for
// an admission/billing decision) the metrics store already owns for accelerator type.
const experimentNodeMetric = "experiment_node_active"

// RecordExperimentNode marks experimentID as running on node as of `at`. Call this every time a
// status report carries a non-empty node (not just on first observation) — a job rescheduled onto
// a different node needs a fresh marker, the same way RecordAcceleratorType is re-written on every
// re-admission.
func RecordExperimentNode(ctx context.Context, dbURL, experimentID, node string, at time.Time) error {
	if node == "" {
		return nil
	}
	labels := map[string]string{"experiment_id": experimentID, "node": node}
	return WriteGaugeAt(ctx, dbURL, experimentNodeMetric, labels, 1, at)
}

// LatestExperimentNode returns the most recently recorded node for experimentID within
// maxLookback, or ("", false, nil) if nothing was ever recorded — e.g. a job that never got far
// enough to schedule a pod, or one older than maxLookback.
func LatestExperimentNode(ctx context.Context, dbURL, experimentID string, now time.Time, maxLookback time.Duration) (node string, found bool, err error) {
	promQL := fmt.Sprintf(`max by (node) (last_over_time(%s{experiment_id=%q}[%s]))`,
		experimentNodeMetric, experimentID, promSeconds(maxLookback))
	series, err := QueryRange(ctx, dbURL, promQL, now.Add(-maxLookback), now, maxLookback)
	if err != nil {
		return "", false, fmt.Errorf("metricsdb.LatestExperimentNode: %w", err)
	}
	var latest string
	var latestAt time.Time
	for _, s := range series {
		n := s.Labels["node"]
		if n == "" || len(s.Points) == 0 {
			continue
		}
		p := s.Points[len(s.Points)-1]
		if p.Time.After(latestAt) {
			latestAt = p.Time
			latest = n
		}
	}
	if latest == "" {
		return "", false, nil
	}
	return latest, true, nil
}
