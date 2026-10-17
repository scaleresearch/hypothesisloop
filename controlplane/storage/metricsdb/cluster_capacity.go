package metricsdb

import (
	"context"
	"fmt"
	"time"
)

const (
	clusterCPUAvailableMetric = "cluster_cpu_available_cores"
	clusterCPUTotalMetric     = "cluster_cpu_total_cores"
	clusterHeartbeatMetric    = "cluster_agent_heartbeat"
)

// ClusterCapacitySnapshot is one cluster-agent observation collected during a single
// reconcile exchange. RecordClusterCapacitySnapshot persists it in one remote-write request.
type ClusterCapacitySnapshot struct {
	ClusterName                            string
	At                                     time.Time
	CPUAvailable, CPUTotal                 float64
	AcceleratorAvailable, AcceleratorTotal map[string]int64
	AcceleratorAvailableByNode             map[string]map[string]int64
	// NodeResourcesByNode carries the fungible dimensions per node — node -> resource -> free
	// amount, keyed by domain.NodeResource*.
	NodeResourcesByNode map[string]map[string]int64
	// NodeResourcesTotalByNode mirrors NodeResourcesByNode but carries each node's installed
	// (allocatable) amount rather than what's currently free — the stable per-node denominator
	// fair-share math needs, since free capacity fluctuates with what's running.
	NodeResourcesTotalByNode map[string]map[string]int64
	NodeLabelsByNode         map[string]map[string]string
	// ClusterID is the runtime-derived stable fingerprint (kube-system namespace UID / machine-id)
	// sent on every reconcile. Carried as a label on the heartbeat sample so LiveClusterIDs can
	// join a display name to the identity everything else keys on. Empty on runtimes that have not
	// been upgraded to send it yet — callers must not assume presence.
	ClusterID string
	// AgentVersion is what the cluster's agent says it runs (clusterapi.AgentVersionHeader), a label
	// on the heartbeat beside ClusterID. Empty from an agent that says nothing.
	AgentVersion string
	// MultiNodeCapable is the cluster's own report of whether its runtime can execute a job
	// spanning more than one node. A capability, not a capacity, but reported and read on the
	// same snapshot for the same reason: it is a live fact about the cluster that goes stale the
	// moment the cluster stops reporting.
	MultiNodeCapable bool
	// AutoscalerEnabled is the cluster's own report of whether its runtime sits behind a native
	// autoscaler (cluster-autoscaler / Karpenter) that will react to Pending pods. Absent/false is
	// the fail-closed default — a false positive here costs a wasted scale-up deadline per job, so
	// it is operator-set, not heuristically detected.
	AutoscalerEnabled              bool
	RAMAvailable, RAMTotal         int64
	StorageAvailable, StorageTotal int64
}

func (s *Store) RecordClusterCapacitySnapshot(ctx context.Context, snapshot ClusterCapacitySnapshot) error {
	labels := map[string]string{"cluster_name": snapshot.ClusterName}
	heartbeatLabels := map[string]string{"cluster_name": snapshot.ClusterName, "cluster_id": snapshot.ClusterID, "agent_version": snapshot.AgentVersion}
	samples := []GaugeSample{
		{MetricName: clusterCPUAvailableMetric, Labels: labels, Value: snapshot.CPUAvailable, At: snapshot.At},
		{MetricName: clusterCPUTotalMetric, Labels: labels, Value: snapshot.CPUTotal, At: snapshot.At},
		{MetricName: clusterRAMAvailableMetric, Labels: labels, Value: float64(snapshot.RAMAvailable), At: snapshot.At},
		{MetricName: clusterRAMTotalMetric, Labels: labels, Value: float64(snapshot.RAMTotal), At: snapshot.At},
		{MetricName: clusterStorageAvailableMetric, Labels: labels, Value: float64(snapshot.StorageAvailable), At: snapshot.At},
		{MetricName: clusterStorageTotalMetric, Labels: labels, Value: float64(snapshot.StorageTotal), At: snapshot.At},
		{MetricName: clusterMultiNodeCapableMetric, Labels: labels, Value: boolGauge(snapshot.MultiNodeCapable), At: snapshot.At},
		{MetricName: clusterAutoscalerEnabledMetric, Labels: labels, Value: boolGauge(snapshot.AutoscalerEnabled), At: snapshot.At},
	}
	for acceleratorType, available := range snapshot.AcceleratorAvailable {
		samples = append(samples, GaugeSample{MetricName: clusterAcceleratorAvailableMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "accelerator_type": acceleratorType}, Value: float64(available), At: snapshot.At})
	}
	for acceleratorType, total := range snapshot.AcceleratorTotal {
		samples = append(samples, GaugeSample{MetricName: clusterAcceleratorTotalMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "accelerator_type": acceleratorType}, Value: float64(total), At: snapshot.At})
	}
	for node, byType := range snapshot.AcceleratorAvailableByNode {
		for acceleratorType, available := range byType {
			samples = append(samples, GaugeSample{MetricName: clusterNodeAcceleratorAvailableMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "node": node, "accelerator_type": acceleratorType}, Value: float64(available), At: snapshot.At})
		}
	}
	for node, byResource := range snapshot.NodeResourcesByNode {
		for resource, available := range byResource {
			samples = append(samples, GaugeSample{MetricName: clusterNodeResourceAvailableMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "node": node, "resource": resource}, Value: float64(available), At: snapshot.At})
		}
	}
	for node, byResource := range snapshot.NodeResourcesTotalByNode {
		for resource, total := range byResource {
			samples = append(samples, GaugeSample{MetricName: clusterNodeResourceTotalMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "node": node, "resource": resource}, Value: float64(total), At: snapshot.At})
		}
	}
	for node, labels := range snapshot.NodeLabelsByNode {
		for key, value := range labels {
			samples = append(samples, GaugeSample{MetricName: clusterNodeLabelMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "node": node, "label_key": key, "label_value": value}, Value: 1, At: snapshot.At})
		}
	}
	if err := s.WriteGaugesAt(ctx, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterCapacitySnapshot: %w", err)
	}
	// Readers take a snapshot at its heartbeat's timestamp, and one write is not atomic across
	// metric tables, so the heartbeat is written only once every sample it vouches for is stored.
	heartbeat := []GaugeSample{{MetricName: clusterHeartbeatMetric, Labels: heartbeatLabels, Value: 1, At: snapshot.At}}
	if err := s.WriteGaugesAt(ctx, heartbeat); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterCapacitySnapshot: heartbeat: %w", err)
	}
	return nil
}

// clusterMultiNodeCapableMetric is a cluster's self-reported ability to run a job spanning more
// than one node: 1 yes, 0 no. Written on every reconcile exchange like the capacities beside it,
// so it ages out with the same freshness window — a cluster that stopped reporting is not
// credited with a capability it can no longer be asked about.
const clusterMultiNodeCapableMetric = "cluster_multi_node_capable"

// clusterAutoscalerEnabledMetric is a cluster's operator-set report of whether it sits behind a
// native autoscaler that reacts to Pending pods: 1 yes, 0 no. Written on every reconcile exchange
// like clusterMultiNodeCapableMetric beside it, so it ages out with the same freshness window.
const clusterAutoscalerEnabledMetric = "cluster_autoscaler_enabled"

// clusterAcceleratorAvailableMetric/clusterAcceleratorTotalMetric hold cluster-agents'
// self-reported live per-accelerator-type capacity (allocatable minus actually-requested,
// computed per accelerator type against real node/pod state — see
// workload.GetLiveAcceleratorCapacitySnapshot). Written on every reconcile exchange, like CPU
// capacity. Lives only in metrics storage, never duplicated into Postgres.
const (
	clusterAcceleratorAvailableMetric     = "cluster_accelerator_available_accelerators"
	clusterAcceleratorTotalMetric         = "cluster_accelerator_total_accelerators"
	clusterNodeAcceleratorAvailableMetric = "cluster_node_accelerator_available_accelerators"
	// clusterNodeResourceAvailableMetric carries the fungible dimensions per node, keyed by a
	// "resource" label, so a new dimension needs no new series. Reported alongside the per-node
	// accelerator counts because a job is placed on one node and must fit that node in *every*
	// dimension — a cluster-wide CPU total cannot answer that (see reservePlacement).
	clusterNodeResourceAvailableMetric = "cluster_node_resource_available"
	// clusterNodeResourceTotalMetric carries each node's installed (allocatable) CPU/memory/storage
	// capacity — the stable denominator fair-share math needs, as distinct from the free/available
	// series above which fluctuates with what's currently running.
	clusterNodeResourceTotalMetric = "cluster_node_resource_total"
	clusterNodeLabelMetric         = "cluster_node_label"
)

// clusterRAMAvailableMetric/clusterRAMTotalMetric and their storage equivalents hold
// cluster-agents' self-reported live RAM/ephemeral-storage capacity in bytes. These are hard
// physical-fit dimensions and live only in metrics storage.
const (
	clusterRAMAvailableMetric     = "cluster_ram_available_bytes"
	clusterRAMTotalMetric         = "cluster_ram_total_bytes"
	clusterStorageAvailableMetric = "cluster_storage_available_bytes"
	clusterStorageTotalMetric     = "cluster_storage_total_bytes"
)
