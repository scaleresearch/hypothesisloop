package metricsdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

func RecordClusterCapacitySnapshot(ctx context.Context, dbURL string, snapshot ClusterCapacitySnapshot) error {
	labels := map[string]string{"cluster_name": snapshot.ClusterName}
	heartbeatLabels := map[string]string{"cluster_name": snapshot.ClusterName, "cluster_id": snapshot.ClusterID}
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
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterCapacitySnapshot: %w", err)
	}
	// Readers take a snapshot at its heartbeat's timestamp, and one write is not atomic across
	// metric tables, so the heartbeat is written only once every sample it vouches for is stored.
	heartbeat := []GaugeSample{{MetricName: clusterHeartbeatMetric, Labels: heartbeatLabels, Value: 1, At: snapshot.At}}
	if err := WriteGaugesAt(ctx, dbURL, heartbeat); err != nil {
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

// LiveClusterAutoscalerCapability returns, per cluster with a fresh report, whether it sits behind
// a native autoscaler. A cluster absent from the map has not reported recently and must be treated
// as not-autoscaled by the caller — never assumed capable, since the cost of the assumption is a
// speculative submit that Pends forever.
func LiveClusterAutoscalerCapability(ctx context.Context, dbURL string, window time.Duration) (map[string]bool, error) {
	values, err := lastValuePerCluster(ctx, dbURL, clusterAutoscalerEnabledMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterAutoscalerCapability: %w", err)
	}
	out := make(map[string]bool, len(values))
	for cluster, value := range values {
		out[cluster] = value != 0
	}
	return out, nil
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// LiveClusterMultiNodeCapability returns, per cluster with a fresh report, whether its runtime can
// execute a multi-node job. A cluster absent from the map has not reported recently and must be
// treated as incapable by the caller — never assumed capable, since the cost of the assumption is
// a distributed job admitted onto a runtime that will refuse it at creation time.
func LiveClusterMultiNodeCapability(ctx context.Context, dbURL string, window time.Duration) (map[string]bool, error) {
	values, err := lastValuePerCluster(ctx, dbURL, clusterMultiNodeCapableMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterMultiNodeCapability: %w", err)
	}
	out := make(map[string]bool, len(values))
	for cluster, value := range values {
		out[cluster] = value != 0
	}
	return out, nil
}

// LiveClusterIDs returns each connected cluster's runtime-derived identity (kube-system namespace
// UID / machine-id), as reported alongside its most recent heartbeat within window. A cluster
// absent from the map has either not reported recently or is running a runtime build that does not
// yet send cluster_id — callers must treat absence as "identity unknown", not as an error.
func LiveClusterIDs(ctx context.Context, dbURL string, window time.Duration) (map[string]string, error) {
	if window <= 0 {
		return nil, fmt.Errorf("metricsdb.LiveClusterIDs: freshness window must be positive")
	}
	seconds := int64(math.Ceil(window.Seconds()))
	query := fmt.Sprintf(
		`WITH latest_heartbeat AS (`+
			`SELECT cluster_name, MAX(greptime_timestamp) AS snapshot_at FROM %s `+
			`WHERE greptime_timestamp >= NOW() - INTERVAL '%d seconds' GROUP BY cluster_name`+
			`) SELECT metric.* FROM %s metric JOIN latest_heartbeat heartbeat `+
			`ON metric.cluster_name = heartbeat.cluster_name AND metric.greptime_timestamp = heartbeat.snapshot_at`,
		clusterHeartbeatMetric, seconds, clusterHeartbeatMetric,
	)
	samples, err := runClusterSnapshotQuery(ctx, dbURL, query)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterIDs: %w", err)
	}
	out := make(map[string]string, len(samples))
	for _, sample := range samples {
		cluster := sample.Labels["cluster_name"]
		clusterID := sample.Labels["cluster_id"]
		if cluster == "" || clusterID == "" {
			continue
		}
		out[cluster] = clusterID
	}
	return out, nil
}

func LiveClusterHeartbeats(ctx context.Context, dbURL string, window time.Duration) (map[string]bool, error) {
	beats, err := lastValuePerCluster(ctx, dbURL, clusterHeartbeatMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterHeartbeats: %w", err)
	}
	out := make(map[string]bool, len(beats))
	for cluster := range beats {
		out[cluster] = true
	}
	return out, nil
}

// lastValuePerCluster is the most recent value of a per-cluster gauge within window, keyed by
// cluster. Every "what does each cluster currently report for X" read goes through this — the
// heartbeat, the float capacities and the scalar (byte/count) capacities all asked the identical
// last_over_time query and then re-implemented the same missing-label and duplicate-cluster
// checks. One cluster reporting a metric twice is a real ambiguity, not something to resolve by
// whichever sample the backend happened to return last, so it stays an error here for all of them.
func lastValuePerCluster(ctx context.Context, dbURL, metricName string, window time.Duration) (map[string]float64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, metricName, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb: query %s: %w", metricName, err)
	}
	out := make(map[string]float64, len(samples))
	for _, sample := range samples {
		cluster := sample.Labels["cluster_name"]
		if cluster == "" {
			return nil, fmt.Errorf("metricsdb: %s: sample missing cluster_name", metricName)
		}
		if _, exists := out[cluster]; exists {
			return nil, fmt.Errorf("metricsdb: %s: duplicate cluster %q", metricName, cluster)
		}
		out[cluster] = sample.Value
	}
	return out, nil
}

func liveClusterFloatCapacity(ctx context.Context, dbURL, metricName string, window time.Duration) (map[string]float64, error) {
	out, err := lastValuePerCluster(ctx, dbURL, metricName, window)
	if err != nil {
		return nil, err
	}
	for cluster, value := range out {
		if value < 0 {
			return nil, fmt.Errorf("metricsdb: %s: cluster %q has negative capacity", metricName, cluster)
		}
	}
	return out, nil
}

func LiveClusterCPUCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]float64, error) {
	return liveClusterFloatCapacity(ctx, dbURL, clusterCPUAvailableMetric, window)
}

func LiveClusterCPUTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]float64, error) {
	return liveClusterFloatCapacity(ctx, dbURL, clusterCPUTotalMetric, window)
}

// clusterAcceleratorAvailableMetric/clusterAcceleratorTotalMetric hold cluster-agents'
// self-reported live per-flavor accelerator capacity (allocatable minus actually-requested,
// computed per flavor against real node/pod state — see
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

func LiveClusterNodeLabels(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]map[string]string, error) {
	samples, err := queryCompleteClusterSnapshot(ctx, dbURL, clusterNodeLabelMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterNodeLabels: %w", err)
	}
	out := make(map[string]map[string]map[string]string)
	for _, sample := range samples {
		cluster, node := sample.Labels["cluster_name"], sample.Labels["node"]
		key, value := sample.Labels["label_key"], sample.Labels["label_value"]
		if cluster == "" || node == "" || key == "" {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeLabels: sample missing cluster_name, node, or label_key")
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]map[string]string)
		}
		if out[cluster][node] == nil {
			out[cluster][node] = make(map[string]string)
		}
		if _, duplicate := out[cluster][node][key]; duplicate {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeLabels: duplicate cluster %q node %q label %q", cluster, node, key)
		}
		out[cluster][node][key] = value
	}
	return out, nil
}

// LiveClusterNodeTotalCapacity returns each node's fresh capacity available to platform-scheduled
// jobs as cluster -> node -> resource -> total amount. Mirrors LiveClusterNodeResourceCapacity's
// shape, for the total/platform-usable series rather than the free/available one.
func LiveClusterNodeTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]map[string]int64, error) {
	samples, err := queryCompleteClusterSnapshot(ctx, dbURL, clusterNodeResourceTotalMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterNodeTotalCapacity: %w", err)
	}
	out := make(map[string]map[string]map[string]int64)
	for _, sample := range samples {
		cluster, node, resource := sample.Labels["cluster_name"], sample.Labels["node"], sample.Labels["resource"]
		if cluster == "" || node == "" || resource == "" {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeTotalCapacity: sample missing cluster_name, node, or resource")
		}
		value, err := capacityInt64(sample.Value)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeTotalCapacity: cluster %q node %q resource %q: %w", cluster, node, resource, err)
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]map[string]int64)
		}
		if out[cluster][node] == nil {
			out[cluster][node] = make(map[string]int64)
		}
		if _, exists := out[cluster][node][resource]; exists {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeTotalCapacity: duplicate cluster %q node %q resource %q", cluster, node, resource)
		}
		out[cluster][node][resource] = value
	}
	return out, nil
}

// LiveClusterNodeAcceleratorCapacity returns fresh actual capacity as
// cluster -> node -> flavor -> free count.
func LiveClusterNodeAcceleratorCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]map[string]int64, error) {
	samples, err := queryCompleteClusterSnapshot(ctx, dbURL, clusterNodeAcceleratorAvailableMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterNodeAcceleratorCapacity: %w", err)
	}
	out := make(map[string]map[string]map[string]int64)
	for _, sample := range samples {
		cluster, node, acceleratorType := sample.Labels["cluster_name"], sample.Labels["node"], sample.Labels["accelerator_type"]
		if cluster == "" || node == "" || acceleratorType == "" {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeAcceleratorCapacity: sample missing cluster_name, node, or accelerator_type")
		}
		value, err := capacityInt64(sample.Value)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeAcceleratorCapacity: cluster %q node %q accelerator type %q: %w", cluster, node, acceleratorType, err)
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]map[string]int64)
		}
		if out[cluster][node] == nil {
			out[cluster][node] = make(map[string]int64)
		}
		if _, exists := out[cluster][node][acceleratorType]; exists {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeAcceleratorCapacity: duplicate cluster %q node %q accelerator type %q", cluster, node, acceleratorType)
		}
		out[cluster][node][acceleratorType] = value
	}
	return out, nil
}

// LiveClusterNodeResourceCapacity returns fresh actual capacity as
// cluster -> node -> resource -> free amount, for the fungible dimensions (see
// domain.NodeResource* for the keys).
func LiveClusterNodeResourceCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]map[string]int64, error) {
	samples, err := queryCompleteClusterSnapshot(ctx, dbURL, clusterNodeResourceAvailableMetric, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterNodeResourceCapacity: %w", err)
	}
	out := make(map[string]map[string]map[string]int64)
	for _, sample := range samples {
		cluster, node, resource := sample.Labels["cluster_name"], sample.Labels["node"], sample.Labels["resource"]
		if cluster == "" || node == "" || resource == "" {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeResourceCapacity: sample missing cluster_name, node, or resource")
		}
		value, err := capacityInt64(sample.Value)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeResourceCapacity: cluster %q node %q resource %q: %w", cluster, node, resource, err)
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]map[string]int64)
		}
		if out[cluster][node] == nil {
			out[cluster][node] = make(map[string]int64)
		}
		if _, exists := out[cluster][node][resource]; exists {
			return nil, fmt.Errorf("metricsdb.LiveClusterNodeResourceCapacity: duplicate cluster %q node %q resource %q", cluster, node, resource)
		}
		out[cluster][node][resource] = value
	}
	return out, nil
}

// LiveClusterAcceleratorTotalCapacity returns the most recently reported total (allocatable)
// accelerator capacity per cluster per flavor within window. A caller that also needs available
// capacity must use LiveClusterAcceleratorAvailableAndTotal so both come from one snapshot.
func LiveClusterAcceleratorTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	return liveClusterAcceleratorMetric(ctx, dbURL, clusterAcceleratorTotalMetric, window)
}

// LiveClusterAcceleratorAvailableAndTotal returns available and total accelerator capacity per
// cluster per flavor, both read at the cluster's latest heartbeat so they come from one snapshot.
func LiveClusterAcceleratorAvailableAndTotal(ctx context.Context, dbURL string, window time.Duration) (available, total map[string]map[string]int64, err error) {
	samples, err := queryCompleteClusterAcceleratorSnapshot(ctx, dbURL, window)
	if err != nil {
		return nil, nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorAvailableAndTotal: %w", err)
	}
	available = make(map[string]map[string]int64)
	total = make(map[string]map[string]int64)
	for _, s := range samples {
		cluster := s.Labels["cluster_name"]
		acceleratorType := s.Labels["accelerator_type"]
		kind := s.Labels["kind"]
		if cluster == "" || acceleratorType == "" {
			return nil, nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorAvailableAndTotal: sample missing cluster_name or accelerator_type")
		}
		value, err := capacityInt64(s.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorAvailableAndTotal: cluster %q accelerator type %q: %w", cluster, acceleratorType, err)
		}
		var target map[string]map[string]int64
		switch kind {
		case "available":
			target = available
		case "total":
			target = total
		default:
			return nil, nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorAvailableAndTotal: unexpected kind %q", kind)
		}
		if target[cluster] == nil {
			target[cluster] = make(map[string]int64)
		}
		if _, exists := target[cluster][acceleratorType]; exists {
			return nil, nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorAvailableAndTotal: duplicate cluster %q accelerator type %q (%s)", cluster, acceleratorType, kind)
		}
		target[cluster][acceleratorType] = value
	}
	return available, total, nil
}

// latestSnapshotFilter returns a SQL predicate selecting, per cluster, the samples written at its
// latest heartbeat within window, or "" when no cluster reported. The heartbeat is read in its own
// statement: a query is not atomic across metric tables, so joining it to a capacity table can
// pair a heartbeat with a scan taken before its samples landed. A heartbeat already read has
// every sample it vouches for stored.
func latestSnapshotFilter(ctx context.Context, dbURL string, window time.Duration) (string, error) {
	if window <= 0 {
		return "", fmt.Errorf("freshness window must be positive")
	}
	heartbeats, err := runClusterSnapshotQuery(ctx, dbURL, fmt.Sprintf(
		`SELECT cluster_name, MAX(greptime_timestamp) AS greptime_timestamp, 1.0 AS greptime_value FROM %s `+
			`WHERE greptime_timestamp >= NOW() - INTERVAL '%d seconds' GROUP BY cluster_name`,
		clusterHeartbeatMetric, int64(math.Ceil(window.Seconds())),
	))
	if err != nil {
		return "", err
	}
	snapshots := make([]string, 0, len(heartbeats))
	for _, heartbeat := range heartbeats {
		snapshots = append(snapshots, fmt.Sprintf(`(cluster_name = '%s' AND greptime_timestamp = %d)`,
			strings.ReplaceAll(heartbeat.Labels["cluster_name"], `'`, `''`), heartbeat.At.UnixMilli()))
	}
	return strings.Join(snapshots, " OR "), nil
}

// queryCompleteClusterAcceleratorSnapshot pulls the available and total accelerator metrics of
// each cluster's latest snapshot, so both numbers reflect the same reported instant. Samples are
// tagged with a synthetic "kind" label of "available" or "total".
func queryCompleteClusterAcceleratorSnapshot(ctx context.Context, dbURL string, window time.Duration) ([]VectorSample, error) {
	filter, err := latestSnapshotFilter(ctx, dbURL, window)
	if err != nil || filter == "" {
		return nil, err
	}
	query := fmt.Sprintf(
		`SELECT 'available' AS kind, cluster_name, accelerator_type, greptime_timestamp, greptime_value FROM %s WHERE %s `+
			`UNION ALL `+
			`SELECT 'total' AS kind, cluster_name, accelerator_type, greptime_timestamp, greptime_value FROM %s WHERE %s`,
		clusterAcceleratorAvailableMetric, filter, clusterAcceleratorTotalMetric, filter,
	)
	return runClusterSnapshotQuery(ctx, dbURL, query)
}

func liveClusterAcceleratorMetric(ctx context.Context, dbURL, metricName string, window time.Duration) (map[string]map[string]int64, error) {
	samples, err := queryCompleteClusterSnapshot(ctx, dbURL, metricName, window)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.liveClusterAcceleratorMetric(%s): %w", metricName, err)
	}
	out := make(map[string]map[string]int64)
	for _, s := range samples {
		cluster := s.Labels["cluster_name"]
		acceleratorType := s.Labels["accelerator_type"]
		if cluster == "" || acceleratorType == "" {
			return nil, fmt.Errorf("metricsdb.liveClusterAcceleratorMetric(%s): sample missing cluster_name or accelerator_type", metricName)
		}
		value, err := capacityInt64(s.Value)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.liveClusterAcceleratorMetric(%s): cluster %q accelerator type %q: %w", metricName, cluster, acceleratorType, err)
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]int64)
		}
		if _, exists := out[cluster][acceleratorType]; exists {
			return nil, fmt.Errorf("metricsdb.liveClusterAcceleratorMetric(%s): duplicate cluster %q accelerator type %q", metricName, cluster, acceleratorType)
		}
		out[cluster][acceleratorType] = value
	}
	return out, nil
}

func queryCompleteClusterSnapshot(ctx context.Context, dbURL, metricName string, window time.Duration) ([]VectorSample, error) {
	switch metricName {
	case clusterAcceleratorAvailableMetric, clusterAcceleratorTotalMetric, clusterNodeAcceleratorAvailableMetric, clusterNodeResourceAvailableMetric, clusterNodeResourceTotalMetric, clusterNodeLabelMetric:
	default:
		return nil, fmt.Errorf("unsupported snapshot metric %q", metricName)
	}
	filter, err := latestSnapshotFilter(ctx, dbURL, window)
	if err != nil || filter == "" {
		return nil, err
	}
	return runClusterSnapshotQuery(ctx, dbURL, fmt.Sprintf(`SELECT * FROM %s WHERE %s`, metricName, filter))
}

// runClusterSnapshotQuery executes query against GreptimeDB's SQL HTTP endpoint and decodes the
// result into VectorSamples, treating every non-timestamp/value column as a label.
func runClusterSnapshotQuery(ctx context.Context, dbURL, query string) ([]VectorSample, error) {
	u, err := url.Parse(dbURL + "/v1/sql")
	if err != nil {
		return nil, fmt.Errorf("query URL: %w", err)
	}
	params := u.Query()
	params.Set("sql", query)
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("greptimedb returned %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Code   int    `json:"code"`
		Error  string `json:"error"`
		Output []struct {
			Records *struct {
				Schema struct {
					Columns []struct {
						Name string `json:"name"`
					} `json:"column_schemas"`
				} `json:"schema"`
				Rows [][]json.RawMessage `json:"rows"`
			} `json:"records"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// Metric tables are created by their first write, so before any cluster has reported there
	// is no snapshot: no rows, the same answer querySQLRows gives.
	if result.Code == greptimeCodeTableNotFound {
		return nil, nil
	}
	if result.Error != "" {
		return nil, fmt.Errorf("greptimedb query failed (%d): %s", result.Code, result.Error)
	}
	if len(result.Output) != 1 || result.Output[0].Records == nil {
		return nil, fmt.Errorf("expected one records result, got %d", len(result.Output))
	}
	records := result.Output[0].Records
	columnIndex := make(map[string]int, len(records.Schema.Columns))
	for i, column := range records.Schema.Columns {
		if column.Name == "" {
			return nil, fmt.Errorf("column %d has no name", i)
		}
		if _, duplicate := columnIndex[column.Name]; duplicate {
			return nil, fmt.Errorf("duplicate column %q", column.Name)
		}
		columnIndex[column.Name] = i
	}
	timestampIndex, hasTimestamp := columnIndex["greptime_timestamp"]
	valueIndex, hasValue := columnIndex["greptime_value"]
	if !hasTimestamp || !hasValue {
		return nil, fmt.Errorf("result missing greptime_timestamp or greptime_value")
	}
	out := make([]VectorSample, 0, len(records.Rows))
	for rowIndex, row := range records.Rows {
		if len(row) != len(records.Schema.Columns) {
			return nil, fmt.Errorf("row %d has %d values for %d columns", rowIndex, len(row), len(records.Schema.Columns))
		}
		var timestampMillis int64
		if err := json.Unmarshal(row[timestampIndex], &timestampMillis); err != nil {
			return nil, fmt.Errorf("row %d timestamp: %w", rowIndex, err)
		}
		var value json.Number
		if err := json.Unmarshal(row[valueIndex], &value); err != nil {
			return nil, fmt.Errorf("row %d value: %w", rowIndex, err)
		}
		floatValue, err := strconv.ParseFloat(value.String(), 64)
		if err != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
			return nil, fmt.Errorf("row %d invalid value %q", rowIndex, value)
		}
		labels := make(map[string]string, len(columnIndex)-2)
		for name, index := range columnIndex {
			if name == "greptime_timestamp" || name == "greptime_value" {
				continue
			}
			var label string
			if err := json.Unmarshal(row[index], &label); err != nil {
				return nil, fmt.Errorf("row %d label %q: %w", rowIndex, name, err)
			}
			labels[name] = label
		}
		out = append(out, VectorSample{Labels: labels, Value: floatValue, At: time.UnixMilli(timestampMillis).UTC()})
	}
	return out, nil
}

// clusterRAMAvailableMetric/clusterRAMTotalMetric and their storage equivalents hold
// cluster-agents' self-reported live RAM/ephemeral-storage capacity in bytes. These are hard
// physical-fit dimensions and live only in metrics storage.
const (
	clusterRAMAvailableMetric     = "cluster_ram_available_bytes"
	clusterRAMTotalMetric         = "cluster_ram_total_bytes"
	clusterStorageAvailableMetric = "cluster_storage_available_bytes"
	clusterStorageTotalMetric     = "cluster_storage_total_bytes"
)

// liveClusterScalarCapacity returns the most recently reported value of availMetric per cluster,
// restricted to samples within window of now.
func liveClusterScalarCapacity(ctx context.Context, dbURL, availMetric string, window time.Duration) (map[string]int64, error) {
	raw, err := lastValuePerCluster(ctx, dbURL, availMetric, window)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(raw))
	for cluster, v := range raw {
		value, err := capacityInt64(v)
		if err != nil {
			return nil, fmt.Errorf("metricsdb: %s: cluster %q: %w", availMetric, cluster, err)
		}
		out[cluster] = value
	}
	return out, nil
}

func capacityInt64(value float64) (int64, error) {
	if value < 0 || value != math.Trunc(value) || value > math.MaxInt64 {
		return 0, fmt.Errorf("capacity must be a non-negative integer, got %v", value)
	}
	return int64(value), nil
}

func LiveClusterRAMCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterRAMAvailableMetric, window)
}

// LiveClusterRAMTotalCapacity is LiveClusterRAMCapacity's total-capacity counterpart
// (allocatable, not allocatable-minus-requested).
func LiveClusterRAMTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterRAMTotalMetric, window)
}

func LiveClusterStorageCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterStorageAvailableMetric, window)
}

// LiveClusterStorageTotalCapacity is LiveClusterStorageCapacity's total-capacity counterpart
// (allocatable, not allocatable-minus-requested).
func LiveClusterStorageTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterStorageTotalMetric, window)
}
