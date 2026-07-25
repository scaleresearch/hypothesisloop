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
	NodeLabelsByNode                       map[string]map[string]string
	RAMAvailable, RAMTotal                 int64
	StorageAvailable, StorageTotal         int64
}

func RecordClusterCapacitySnapshot(ctx context.Context, dbURL string, snapshot ClusterCapacitySnapshot) error {
	labels := map[string]string{"cluster_name": snapshot.ClusterName}
	samples := []GaugeSample{
		{MetricName: clusterHeartbeatMetric, Labels: labels, Value: 1, At: snapshot.At},
		{MetricName: clusterCPUAvailableMetric, Labels: labels, Value: snapshot.CPUAvailable, At: snapshot.At},
		{MetricName: clusterCPUTotalMetric, Labels: labels, Value: snapshot.CPUTotal, At: snapshot.At},
		{MetricName: clusterRAMAvailableMetric, Labels: labels, Value: float64(snapshot.RAMAvailable), At: snapshot.At},
		{MetricName: clusterRAMTotalMetric, Labels: labels, Value: float64(snapshot.RAMTotal), At: snapshot.At},
		{MetricName: clusterStorageAvailableMetric, Labels: labels, Value: float64(snapshot.StorageAvailable), At: snapshot.At},
		{MetricName: clusterStorageTotalMetric, Labels: labels, Value: float64(snapshot.StorageTotal), At: snapshot.At},
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
	for node, labels := range snapshot.NodeLabelsByNode {
		for key, value := range labels {
			samples = append(samples, GaugeSample{MetricName: clusterNodeLabelMetric, Labels: map[string]string{"cluster_name": snapshot.ClusterName, "node": node, "label_key": key, "label_value": value}, Value: 1, At: snapshot.At})
		}
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterCapacitySnapshot: %w", err)
	}
	return nil
}

func RecordClusterHeartbeat(ctx context.Context, dbURL, clusterName string, at time.Time) error {
	return WriteGaugeAt(ctx, dbURL, clusterHeartbeatMetric, map[string]string{"cluster_name": clusterName}, 1, at)
}

func LiveClusterHeartbeats(ctx context.Context, dbURL string, window time.Duration) (map[string]bool, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, clusterHeartbeatMetric, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterHeartbeats: %w", err)
	}
	out := make(map[string]bool, len(samples))
	for _, sample := range samples {
		cluster := sample.Labels["cluster_name"]
		if cluster == "" {
			return nil, fmt.Errorf("metricsdb.LiveClusterHeartbeats: sample missing cluster_name")
		}
		if _, exists := out[cluster]; exists {
			return nil, fmt.Errorf("metricsdb.LiveClusterHeartbeats: duplicate cluster %q", cluster)
		}
		out[cluster] = true
	}
	return out, nil
}

func RecordClusterCPUCapacity(ctx context.Context, dbURL, clusterName string, available, total float64) error {
	now := time.Now().UTC()
	samples := []GaugeSample{
		{MetricName: clusterCPUAvailableMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: available, At: now},
		{MetricName: clusterCPUTotalMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: total, At: now},
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterCPUCapacity: %w", err)
	}
	return nil
}

func liveClusterFloatCapacity(ctx context.Context, dbURL, metricName string, window time.Duration) (map[string]float64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, metricName, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.liveClusterFloatCapacity(%s): %w", metricName, err)
	}
	out := make(map[string]float64, len(samples))
	for _, sample := range samples {
		cluster := sample.Labels["cluster_name"]
		if cluster == "" {
			return nil, fmt.Errorf("metricsdb.liveClusterFloatCapacity(%s): sample missing cluster_name", metricName)
		}
		if sample.Value < 0 {
			return nil, fmt.Errorf("metricsdb.liveClusterFloatCapacity(%s): cluster %q has negative capacity", metricName, cluster)
		}
		if _, exists := out[cluster]; exists {
			return nil, fmt.Errorf("metricsdb.liveClusterFloatCapacity(%s): duplicate cluster %q", metricName, cluster)
		}
		out[cluster] = sample.Value
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
	clusterNodeLabelMetric                = "cluster_node_label"
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

// RecordClusterNodeAcceleratorCapacity writes actual free accelerator counts per node and
// flavor. Node identity is observation metadata only; placement remains Kubernetes-owned.
func RecordClusterNodeAcceleratorCapacity(ctx context.Context, dbURL, clusterName string, byNode map[string]map[string]int64) error {
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0)
	for node, byType := range byNode {
		for acceleratorType, available := range byType {
			samples = append(samples, GaugeSample{
				MetricName: clusterNodeAcceleratorAvailableMetric,
				Labels:     map[string]string{"cluster_name": clusterName, "node": node, "accelerator_type": acceleratorType},
				Value:      float64(available), At: now,
			})
		}
	}
	if len(samples) == 0 {
		return nil
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterNodeAcceleratorCapacity: %w", err)
	}
	return nil
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

// RecordClusterAcceleratorCapacity writes one gauge sample per (flavor, available/total) pair for
// clusterName, all in a single remote-write batch.
func RecordClusterAcceleratorCapacity(ctx context.Context, dbURL, clusterName string, availableByFlavor, totalByFlavor map[string]int64) error {
	if len(availableByFlavor) == 0 {
		return nil
	}
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0, 2*len(availableByFlavor))
	for acceleratorType, avail := range availableByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterAcceleratorAvailableMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "accelerator_type": acceleratorType},
			Value:      float64(avail),
			At:         now,
		})
	}
	for acceleratorType, total := range totalByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterAcceleratorTotalMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "accelerator_type": acceleratorType},
			Value:      float64(total),
			At:         now,
		})
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterAcceleratorCapacity: %w", err)
	}
	return nil
}

// LiveClusterAcceleratorCapacity returns the most recently reported accelerator availability per
// cluster per flavor, restricted to samples within `window` of now — a cluster/flavor with no
// report inside the window is simply absent from the result (same freshness gating as
// metricsdb.IsAlive), so a stale or disconnected cluster contributes nothing.
func LiveClusterAcceleratorCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	return liveClusterAcceleratorMetric(ctx, dbURL, clusterAcceleratorAvailableMetric, window)
}

// LiveClusterAcceleratorTotalCapacity is LiveClusterAcceleratorCapacity's total-capacity
// counterpart (allocatable, not allocatable-minus-requested) — same per-cluster-per-flavor
// shape and freshness gating. Together the two let a caller compute both "how much is free
// right now" and "how much exists at all" per accelerator flavor per cluster.
func LiveClusterAcceleratorTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	return liveClusterAcceleratorMetric(ctx, dbURL, clusterAcceleratorTotalMetric, window)
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
	case clusterAcceleratorAvailableMetric, clusterAcceleratorTotalMetric, clusterNodeAcceleratorAvailableMetric, clusterNodeLabelMetric:
	default:
		return nil, fmt.Errorf("unsupported snapshot metric %q", metricName)
	}
	if window <= 0 {
		return nil, fmt.Errorf("freshness window must be positive")
	}
	seconds := int64(math.Ceil(window.Seconds()))
	query := fmt.Sprintf(
		`WITH latest_heartbeat AS (`+
			`SELECT cluster_name, MAX(greptime_timestamp) AS snapshot_at FROM %s `+
			`WHERE greptime_timestamp >= NOW() - INTERVAL '%d seconds' GROUP BY cluster_name`+
			`) SELECT metric.* FROM %s metric JOIN latest_heartbeat heartbeat `+
			`ON metric.cluster_name = heartbeat.cluster_name AND metric.greptime_timestamp = heartbeat.snapshot_at`,
		clusterHeartbeatMetric, seconds, metricName,
	)
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

// recordClusterScalarCapacity writes one gauge sample each for availMetric/totalMetric, scoped
// to clusterName — the shared plumbing behind RecordClusterRAMCapacity/RecordClusterStorageCapacity.
func recordClusterScalarCapacity(ctx context.Context, dbURL, clusterName, availMetric, totalMetric string, availableBytes, totalBytes int64) error {
	now := time.Now().UTC()
	samples := []GaugeSample{
		{MetricName: availMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: float64(availableBytes), At: now},
		{MetricName: totalMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: float64(totalBytes), At: now},
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.recordClusterScalarCapacity(%s): %w", availMetric, err)
	}
	return nil
}

// liveClusterScalarCapacity returns the most recently reported value of availMetric per cluster,
// restricted to samples within window of now — same staleness gating as
// LiveClusterAcceleratorCapacity.
func liveClusterScalarCapacity(ctx context.Context, dbURL, availMetric string, window time.Duration) (map[string]int64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, availMetric, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.liveClusterScalarCapacity(%s): %w", availMetric, err)
	}
	out := make(map[string]int64, len(samples))
	for _, s := range samples {
		cluster := s.Labels["cluster_name"]
		if cluster == "" {
			return nil, fmt.Errorf("metricsdb.liveClusterScalarCapacity(%s): sample missing cluster_name", availMetric)
		}
		value, err := capacityInt64(s.Value)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.liveClusterScalarCapacity(%s): cluster %q: %w", availMetric, cluster, err)
		}
		if _, exists := out[cluster]; exists {
			return nil, fmt.Errorf("metricsdb.liveClusterScalarCapacity(%s): duplicate cluster %q", availMetric, cluster)
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

// RecordClusterRAMCapacity/LiveClusterRAMCapacity report and read live per-cluster RAM
// availability in bytes (allocatable minus requested, only against nodes with an actually
// assigned scheduled pod — see workload.JobWorkloadClient.GetLiveRAMCapacity).
func RecordClusterRAMCapacity(ctx context.Context, dbURL, clusterName string, availableBytes, totalBytes int64) error {
	return recordClusterScalarCapacity(ctx, dbURL, clusterName, clusterRAMAvailableMetric, clusterRAMTotalMetric, availableBytes, totalBytes)
}

func LiveClusterRAMCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterRAMAvailableMetric, window)
}

// LiveClusterRAMTotalCapacity is LiveClusterRAMCapacity's total-capacity counterpart
// (allocatable, not allocatable-minus-requested).
func LiveClusterRAMTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterRAMTotalMetric, window)
}

// RecordClusterStorageCapacity/LiveClusterStorageCapacity are RAM's ephemeral-storage
// counterpart. Kubernetes enforces ephemeral storage through eviction rather than a memory-like
// hard cgroup ceiling.
func RecordClusterStorageCapacity(ctx context.Context, dbURL, clusterName string, availableBytes, totalBytes int64) error {
	return recordClusterScalarCapacity(ctx, dbURL, clusterName, clusterStorageAvailableMetric, clusterStorageTotalMetric, availableBytes, totalBytes)
}

func LiveClusterStorageCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterStorageAvailableMetric, window)
}

// LiveClusterStorageTotalCapacity is LiveClusterStorageCapacity's total-capacity counterpart
// (allocatable, not allocatable-minus-requested).
func LiveClusterStorageTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterStorageTotalMetric, window)
}
