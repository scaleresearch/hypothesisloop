package metricsdb

import (
	"context"
	"fmt"
	"math"
	"strings"
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
	if maxLookback <= 0 {
		return "", false, nil
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	// Ordered by each node's newest *real* sample. The range-query form this replaced asked with
	// a step equal to the whole lookback, which put every node's last point on the same grid
	// timestamp — so "which is latest" compared equal for all of them and the winner was whichever
	// series came back first. After a reschedule that is as likely to name the node the job left
	// as the one it moved to, and the disbalance evictor attributes jobs to nodes with this.
	query := fmt.Sprintf(
		`SELECT node, MAX(greptime_timestamp) AS last_seen FROM %s `+
			`WHERE experiment_id = %s AND greptime_timestamp >= NOW() - INTERVAL '%d seconds' `+
			`GROUP BY node ORDER BY last_seen DESC LIMIT 1`,
		experimentNodeMetric, quote(experimentID), int64(math.Ceil(maxLookback.Seconds())))
	samples, err := runClusterSnapshotQuery(ctx, dbURL, query)
	if err != nil {
		return "", false, fmt.Errorf("metricsdb.LatestExperimentNode: %w", err)
	}
	for _, sample := range samples {
		if n := sample.Labels["node"]; n != "" {
			return n, true, nil
		}
	}
	return "", false, nil
}
