package metricsdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

const (
	clusterJobPhaseMetric      = "cluster_job_phase"
	clusterJobObservedAtMetric = "cluster_job_observed_at_seconds"
	clusterJobSnapshotMetric   = "cluster_job_status_snapshot"
)

var phaseValue = map[string]float64{
	"pending":   1,
	"running":   2,
	"succeeded": 3,
	"failed":    4,
	"gone":      5,
}

// JobStatusSample is one current cluster observation. It is telemetry, not desired state, and
// therefore lives only in the metrics store.
type JobStatusSample struct {
	ExperimentID            string
	ClusterName             string
	Phase                   string
	AdmittedAcceleratorType string
	AdmittedNode            string
	At                      time.Time
}

func RecordJobStatuses(ctx context.Context, dbURL, clusterName string, at time.Time, statuses []JobStatusSample) error {
	if clusterName == "" || at.IsZero() {
		return fmt.Errorf("metricsdb.RecordJobStatuses: cluster and snapshot time are required")
	}
	observedAt := float64(at.UnixMilli()) / 1000
	samples := []GaugeSample{{MetricName: clusterJobSnapshotMetric, Labels: map[string]string{"cluster_name": clusterName, "sample_kind": "snapshot"}, Value: observedAt, At: at}}
	for _, status := range statuses {
		value, ok := phaseValue[status.Phase]
		if !ok {
			return fmt.Errorf("metricsdb.RecordJobStatuses: invalid phase %q for %s", status.Phase, status.ExperimentID)
		}
		if status.ExperimentID == "" || status.ClusterName == "" {
			return fmt.Errorf("metricsdb.RecordJobStatuses: experiment and cluster are required")
		}
		if status.ClusterName != clusterName || !status.At.Equal(at) {
			return fmt.Errorf("metricsdb.RecordJobStatuses: every report must belong to the complete cluster snapshot")
		}
		samples = append(samples, GaugeSample{
			MetricName: clusterJobPhaseMetric,
			Labels: map[string]string{
				"experiment_id": status.ExperimentID,
				"cluster_name":  status.ClusterName,
				"sample_kind":   "phase",
			},
			Value: value,
			At:    status.At,
		})
		samples = append(samples, GaugeSample{MetricName: clusterJobObservedAtMetric, Labels: map[string]string{
			"experiment_id": status.ExperimentID, "cluster_name": status.ClusterName, "sample_kind": "observed_at",
		}, Value: observedAt, At: status.At})
		if status.AdmittedNode != "" {
			samples = append(samples, GaugeSample{MetricName: experimentNodeMetric, Labels: map[string]string{
				"experiment_id": status.ExperimentID, "node": status.AdmittedNode,
			}, Value: 1, At: status.At})
		}
		if status.AdmittedAcceleratorType != "" {
			samples = append(samples, GaugeSample{MetricName: acceleratorTypeMetric, Labels: map[string]string{
				"experiment_id": status.ExperimentID, "accelerator_type": status.AdmittedAcceleratorType,
			}, Value: 1, At: status.At})
		}
	}
	return WriteGaugesAt(ctx, dbURL, samples)
}

// LatestJobPhase returns the most recent phase observed within window. found=false means no
// complete cluster snapshot exists; callers decide whether absence is expected for their state.
func LatestJobPhase(ctx context.Context, dbURL, experimentID, clusterName string, window time.Duration) (workload.JobPhase, bool, error) {
	if window <= 0 {
		return workload.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: freshness window must be positive")
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	seconds := int64(math.Ceil(window.Seconds()))
	query := fmt.Sprintf(
		`SELECT snapshot.greptime_value AS snapshot_value, phase.greptime_value AS phase_value, observed.greptime_value AS observed_value `+
			`FROM (SELECT greptime_timestamp, greptime_value FROM %s WHERE cluster_name = %s AND greptime_timestamp >= NOW() - INTERVAL '%d seconds' ORDER BY greptime_timestamp DESC LIMIT 1) snapshot `+
			`LEFT JOIN %s phase ON phase.greptime_timestamp = snapshot.greptime_timestamp AND phase.experiment_id = %s AND phase.cluster_name = %s `+
			`LEFT JOIN %s observed ON observed.greptime_timestamp = snapshot.greptime_timestamp AND observed.experiment_id = %s AND observed.cluster_name = %s`,
		clusterJobSnapshotMetric, quote(clusterName), seconds,
		clusterJobPhaseMetric, quote(experimentID), quote(clusterName),
		clusterJobObservedAtMetric, quote(experimentID), quote(clusterName),
	)
	snapshotValue, phaseValue, observedAtValue, found, err := queryJobStatusSnapshot(ctx, dbURL, query)
	if err != nil {
		return workload.JobPhasePending, false, err
	}
	if !found {
		return workload.JobPhasePending, false, nil
	}
	if phaseValue == nil && observedAtValue == nil {
		return workload.JobPhaseGone, true, nil
	}
	if phaseValue == nil || observedAtValue == nil {
		return workload.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: incomplete phase observation for %s", experimentID)
	}
	if *observedAtValue != snapshotValue {
		return workload.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: observation timestamp for %s disagrees with its snapshot", experimentID)
	}
	switch int(*phaseValue) {
	case 1:
		return workload.JobPhasePending, true, nil
	case 2:
		return workload.JobPhaseRunning, true, nil
	case 3:
		return workload.JobPhaseSucceeded, true, nil
	case 4:
		return workload.JobPhaseFailed, true, nil
	case 5:
		return workload.JobPhaseGone, true, nil
	default:
		return workload.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: invalid value %v", *phaseValue)
	}
}

func queryJobStatusSnapshot(ctx context.Context, dbURL, query string) (float64, *float64, *float64, bool, error) {
	u, err := url.Parse(dbURL + "/v1/sql")
	if err != nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: query URL: %w", err)
	}
	params := u.Query()
	params.Set("sql", query)
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: build query: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: read query: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: greptimedb returned %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Error  string `json:"error"`
		Output []struct {
			Records *struct {
				Rows [][]*float64 `json:"rows"`
			} `json:"records"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: decode query: %w", err)
	}
	if result.Error != "" {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: greptimedb query failed: %s", result.Error)
	}
	if len(result.Output) != 1 || result.Output[0].Records == nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: expected one records result")
	}
	rows := result.Output[0].Records.Rows
	if len(rows) == 0 {
		return 0, nil, nil, false, nil
	}
	if len(rows) != 1 || len(rows[0]) != 3 || rows[0][0] == nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: malformed snapshot result")
	}
	for _, value := range rows[0] {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: snapshot result contains a non-finite value")
		}
	}
	return *rows[0][0], rows[0][1], rows[0][2], true, nil
}

// LatestAcceleratorType resolves the latest accelerator marker from the metrics store.
func LatestAcceleratorType(ctx context.Context, dbURL, experimentID string, now time.Time, window time.Duration) (domain.AcceleratorType, bool, error) {
	changes, err := acceleratorTypeChanges(ctx, dbURL, experimentID, now.Add(-window), now, time.Second)
	if err != nil {
		return "", false, err
	}
	if len(changes) == 0 {
		return "", false, nil
	}
	return domain.AcceleratorType(changes[len(changes)-1].Type), true, nil
}
