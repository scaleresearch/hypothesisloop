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
//
// Deprecated: callers that also need total capacity must use
// LiveClusterAcceleratorAvailableAndTotal instead — reading available and total via two separate
// queries lets each pick a different "latest heartbeat" instant (e.g. while a cluster-agent is
// mid-registration and a new snapshot lands between the two round trips), which can publish
// available > total even though no such state ever existed in storage.
func LiveClusterAcceleratorCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	return liveClusterAcceleratorMetric(ctx, dbURL, clusterAcceleratorAvailableMetric, window)
}

// LiveClusterAcceleratorTotalCapacity is LiveClusterAcceleratorCapacity's total-capacity
// counterpart (allocatable, not allocatable-minus-requested) — same per-cluster-per-flavor
// shape and freshness gating.
//
// Deprecated: see LiveClusterAcceleratorCapacity — pair it with LiveClusterAcceleratorAvailableAndTotal
// instead of calling this alongside LiveClusterAcceleratorCapacity as two separate queries.
func LiveClusterAcceleratorTotalCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	return liveClusterAcceleratorMetric(ctx, dbURL, clusterAcceleratorTotalMetric, window)
}

// LiveClusterAcceleratorAvailableAndTotal returns available and total accelerator capacity per
// cluster per flavor from a single query against one shared "latest heartbeat per cluster"
// snapshot, so the two numbers always come from the same instant. Reading them via two separate
// queries (the old LiveClusterAcceleratorCapacity + LiveClusterAcceleratorTotalCapacity pairing)
// let each pick its own latest-heartbeat snapshot independently; if a new snapshot lands between
// the two round trips — most visibly while a cluster-agent is still registering and pushing its
// first few snapshots in quick succession — available could be read from a newer instant than
// total, transiently publishing the impossible available > total.
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

// queryCompleteClusterAcceleratorSnapshot is queryCompleteClusterSnapshot generalized to pull
// both the available and total accelerator metrics in one query, joined against one shared
// latest_heartbeat CTE so both numbers reflect the exact same reported instant per cluster.
// Samples are tagged with a synthetic "kind" label of "available" or "total".
func queryCompleteClusterAcceleratorSnapshot(ctx context.Context, dbURL string, window time.Duration) ([]VectorSample, error) {
	if window <= 0 {
		return nil, fmt.Errorf("freshness window must be positive")
	}
	seconds := int64(math.Ceil(window.Seconds()))
	query := fmt.Sprintf(
		`WITH latest_heartbeat AS (`+
			`SELECT cluster_name, MAX(greptime_timestamp) AS snapshot_at FROM %s `+
			`WHERE greptime_timestamp >= NOW() - INTERVAL '%d seconds' GROUP BY cluster_name`+
			`) `+
			`SELECT 'available' AS kind, avail.cluster_name, avail.accelerator_type, avail.greptime_timestamp, avail.greptime_value `+
			`FROM %s avail JOIN latest_heartbeat h ON avail.cluster_name = h.cluster_name AND avail.greptime_timestamp = h.snapshot_at `+
			`UNION ALL `+
			`SELECT 'total' AS kind, tot.cluster_name, tot.accelerator_type, tot.greptime_timestamp, tot.greptime_value `+
			`FROM %s tot JOIN latest_heartbeat h ON tot.cluster_name = h.cluster_name AND tot.greptime_timestamp = h.snapshot_at`,
		clusterHeartbeatMetric, seconds, clusterAcceleratorAvailableMetric, clusterAcceleratorTotalMetric,
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
	return runClusterSnapshotQuery(ctx, dbURL, query)
}

// runClusterSnapshotQuery executes query against GreptimeDB's SQL HTTP endpoint and decodes the
// result into VectorSamples, treating every non-timestamp/value column as a label. Shared by
// queryCompleteClusterSnapshot and queryCompleteClusterAcceleratorSnapshot.
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
