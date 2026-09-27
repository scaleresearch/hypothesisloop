package metricsdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

// The labels a node sample carries beside the node when the runtime reported the machine behind
// it. A label is written only for what the runtime said, so a sample from a runtime that names
// the node alone has none of them.
const (
	machineInstanceLabel    = "machine_instance_id"
	machineHostLabel        = "machine_host_id"
	machineAcceleratorLabel = "machine_accelerator_model"
	machineCPULabel         = "machine_cpu_model"
	machinePriceLabel       = "machine_price_per_hour_usd"
)

func nodeLabels(status JobStatusSample) map[string]string {
	labels := map[string]string{"experiment_id": status.ExperimentID, "node": status.AdmittedNode}
	machine := status.AdmittedMachine
	if machine == nil {
		return labels
	}
	said := map[string]string{
		machineInstanceLabel: machine.InstanceID, machineHostLabel: machine.HostID,
		machineAcceleratorLabel: machine.AcceleratorModel, machineCPULabel: machine.CPUModel,
	}
	if machine.PricePerHourUSD > 0 {
		said[machinePriceLabel] = strconv.FormatFloat(machine.PricePerHourUSD, 'f', -1, 64)
	}
	for label, value := range said {
		if value != "" {
			labels[label] = value
		}
	}
	return labels
}

// LatestRanOn returns where experimentID's workload was last observed since its row was created:
// the newest node sample, with the machine behind the node when the runtime reported one. nil
// when no node was ever recorded.
func LatestRanOn(ctx context.Context, dbURL, experimentID string, createdAt, now time.Time) (*domain.RanOn, error) {
	since := ObservationWindowStart(createdAt)
	if !now.After(since) {
		return nil, fmt.Errorf("metricsdb.LatestRanOn: %s was created after now", experimentID)
	}
	// Every column, since the machine's are there only once a runtime has reported one.
	query := fmt.Sprintf(
		`SELECT * FROM %s WHERE experiment_id = %s AND greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond `+
			`ORDER BY greptime_timestamp DESC LIMIT 1`,
		experimentNodeMetric, sqlQuote(experimentID), since.UnixMilli(), now.UnixMilli())
	samples, err := runClusterSnapshotQuery(ctx, dbURL, query)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LatestRanOn: %w", err)
	}
	if len(samples) == 0 || samples[0].Labels["node"] == "" {
		return nil, nil
	}
	labels := samples[0].Labels
	ranOn := &domain.RanOn{Node: labels["node"]}
	machine := domain.JobMachine{
		InstanceID: labels[machineInstanceLabel], HostID: labels[machineHostLabel],
		AcceleratorModel: labels[machineAcceleratorLabel], CPUModel: labels[machineCPULabel],
	}
	if price := labels[machinePriceLabel]; price != "" {
		if machine.PricePerHourUSD, err = strconv.ParseFloat(price, 64); err != nil {
			return nil, fmt.Errorf("metricsdb.LatestRanOn: price of %s: %w", experimentID, err)
		}
	}
	if machine != (domain.JobMachine{}) {
		ranOn.Machine = &machine
	}
	return ranOn, nil
}

// LatestExperimentNode returns the most recently recorded node for experimentID since its row was
// created, or ("", false, nil) if nothing was ever recorded — e.g. a job that never got far enough
// to schedule a pod, or one whose samples have aged out of retention.
func LatestExperimentNode(ctx context.Context, dbURL, experimentID string, createdAt, now time.Time) (node string, found bool, err error) {
	since := ObservationWindowStart(createdAt)
	if !now.After(since) {
		return "", false, fmt.Errorf("metricsdb.LatestExperimentNode: %s was created after now", experimentID)
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	// Ordered by each node's newest *real* sample. The range-query form this replaced asked with
	// a step equal to the whole lookback, which put every node's last point on the same grid
	// timestamp — so "which is latest" compared equal for all of them and the winner was whichever
	// series came back first. After a reschedule that is as likely to name the node the job left
	// as the one it moved to, and the disbalance evictor attributes jobs to nodes with this.
	// Columns named as the shared row reader expects (greptime_timestamp/greptime_value); the
	// value itself is unused here, only the ordering. Bounded by absolute timestamps rather than
	// the database's own NOW(), so the window is the one the caller asked for and does not shift
	// with clock difference or query delay.
	query := fmt.Sprintf(
		`SELECT node, MAX(greptime_timestamp) AS greptime_timestamp, 1 AS greptime_value FROM %s `+
			`WHERE experiment_id = %s AND greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond `+
			`GROUP BY node ORDER BY 2 DESC LIMIT 1`,
		experimentNodeMetric, quote(experimentID), since.UnixMilli(), now.UnixMilli())
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
