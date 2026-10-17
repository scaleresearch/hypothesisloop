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

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const (
	clusterJobPhaseMetric      = "cluster_job_phase"
	clusterJobObservedAtMetric = "cluster_job_observed_at_seconds"
	clusterJobSnapshotMetric   = "cluster_job_status_snapshot"
)

// A phase is stored as its number one up, so that no stored phase is 0.
func storedPhase(p domain.JobPhase) float64 { return float64(p) + 1 }

// JobStatusSample is one current cluster observation. It is telemetry, not desired state, and
// therefore lives only in the metrics store.
type JobStatusSample struct {
	ExperimentID            string
	ClusterName             string
	Phase                   string
	AdmittedAcceleratorType string
	AdmittedNode            string
	// AdmittedMachine is what stands behind AdmittedNode. It is recorded with the node and not
	// without one.
	AdmittedMachine *domain.JobMachine
	At              time.Time
}

func (s *Store) RecordJobStatuses(ctx context.Context, clusterName string, at time.Time, statuses []JobStatusSample) error {
	if clusterName == "" || at.IsZero() {
		return fmt.Errorf("metricsdb.RecordJobStatuses: cluster and snapshot time are required")
	}
	observedAt := float64(at.UnixMilli()) / 1000
	samples := []gauge{{table: clusterJobSnapshot, labels: map[string]string{"cluster_name": clusterName, "sample_kind": "snapshot"}, value: observedAt, at: at}}
	for _, status := range statuses {
		phase, ok := clusterapi.ParseJobPhase(status.Phase)
		if !ok {
			return fmt.Errorf("metricsdb.RecordJobStatuses: invalid phase %q for %s", status.Phase, status.ExperimentID)
		}
		if status.ExperimentID == "" || status.ClusterName == "" {
			return fmt.Errorf("metricsdb.RecordJobStatuses: experiment and cluster are required")
		}
		if status.ClusterName != clusterName || !status.At.Equal(at) {
			return fmt.Errorf("metricsdb.RecordJobStatuses: every report must belong to the complete cluster snapshot")
		}
		samples = append(samples, gauge{
			table: clusterJobPhase,
			labels: map[string]string{
				"experiment_id": status.ExperimentID,
				"cluster_name":  status.ClusterName,
				"sample_kind":   "phase",
			},
			value: storedPhase(phase),
			at:    status.At,
		})
		samples = append(samples, gauge{table: clusterJobObservedAt, labels: map[string]string{
			"experiment_id": status.ExperimentID, "cluster_name": status.ClusterName, "sample_kind": "observed_at",
		}, value: observedAt, at: status.At})
		if status.AdmittedNode != "" {
			samples = append(samples, gauge{table: experimentNode, labels: nodeLabels(status), value: 1, at: status.At})
		}
		if status.AdmittedAcceleratorType != "" {
			samples = append(samples, gauge{table: acceleratorType, labels: map[string]string{
				"experiment_id": status.ExperimentID, "accelerator_type": status.AdmittedAcceleratorType,
			}, value: 1, at: status.At})
		}
	}
	return s.writeGauges(ctx, samples)
}

// SnapshotPresence answers, in one round trip, the two independent questions the eviction path
// asks before it may act on an experiment's absence.
type SnapshotPresence struct {
	// Reported is false when the cluster has pushed no snapshot at all within searchBack. Nothing
	// about the experiment can be concluded in that case — the silence is the cluster's, not the job's.
	Reported bool
	// SnapshotAge is how stale the cluster's newest snapshot is.
	SnapshotAge time.Duration
	// AbsentSnapshots counts the cluster snapshots strictly newer than the last one that still
	// carried this experiment — i.e. how many consecutive complete snapshots have now omitted it.
	// Zero while the experiment is still being reported.
	AbsentSnapshots int
	// EverPresent is false when no snapshot within searchBack carried this experiment at all, in
	// which case AbsentSnapshots counts every snapshot in the window.
	EverPresent bool
}

// GoneConfirmingSnapshots is how many consecutive complete cluster snapshots must omit an
// experiment before its absence is accepted as real.
//
// Counted in snapshots rather than measured in seconds on purpose. Absence is only ever
// established relative to a snapshot that did arrive, so a cluster that stops pushing cannot
// accumulate evidence against its own jobs, and no assumption about a cluster's local push
// cadence — which the control plane does not configure and must not guess — leaks into an
// irreversible decision. Two is the smallest count that survives a single-tick blind spot, which
// is what a routine drift-delete-then-recreate produces: the Job is genuinely absent for the one
// snapshot between its removal and its recreation, and evicting on that killed real training.
const GoneConfirmingSnapshots = 2

// ClusterSnapshotPresence reports how recently a cluster reported at all and how many of its
// most recent complete snapshots have omitted one experiment. Both facts come from the snapshot
// series' own recorded observation times, never from wall-clock arithmetic over sample
// timestamps, so a cluster outage freezes the absence count instead of growing it.
func (s *Store) ClusterSnapshotPresence(ctx context.Context, experimentID, clusterName string, createdAt, now time.Time) (SnapshotPresence, error) {
	since := ObservationWindowStart(createdAt)
	if !now.After(since) {
		return SnapshotPresence{}, fmt.Errorf("metricsdb.ClusterSnapshotPresence: %s was created after now", experimentID)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	// Both series carry the snapshot's own observation time as their value (see RecordJobStatuses),
	// so comparing values compares snapshots directly — no timestamp encoding assumptions. Bounded
	// by absolute timestamps rather than the database's own NOW(), so the window is the one the
	// caller asked for and does not shift with clock difference or query delay.
	bounds := fmt.Sprintf(`greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond`,
		since.UnixMilli(), now.UnixMilli())
	query := fmt.Sprintf(
		`WITH snaps AS (SELECT greptime_value AS v FROM %s WHERE cluster_name = %s AND %s), `+
			`present AS (SELECT MAX(greptime_value) AS v FROM %s WHERE experiment_id = %s AND cluster_name = %s AND %s) `+
			`SELECT MAX(snaps.v), MAX(present.v), SUM(CASE WHEN present.v IS NULL OR snaps.v > present.v THEN 1 ELSE 0 END) `+
			`FROM snaps CROSS JOIN present`,
		clusterJobSnapshotMetric, quote(clusterName), bounds,
		clusterJobObservedAtMetric, quote(experimentID), quote(clusterName), bounds,
	)
	row, found, err := s.querySingleRow(ctx, "metricsdb.ClusterSnapshotPresence", query, 3)
	if err != nil {
		return SnapshotPresence{}, err
	}
	// No rows, or an all-NULL aggregate row, both mean the cluster pushed nothing in the window.
	if !found || row[0] == nil {
		return SnapshotPresence{}, nil
	}
	presence := SnapshotPresence{
		Reported:    true,
		SnapshotAge: now.Sub(time.UnixMilli(int64(*row[0] * 1000)).UTC()),
		EverPresent: row[1] != nil,
	}
	if row[2] != nil {
		presence.AbsentSnapshots = int(*row[2])
	}
	return presence, nil
}

// LatestJobPhase returns the most recent phase observed within window. found=false means no
// complete cluster snapshot exists; callers decide whether absence is expected for their state.
func (s *Store) LatestJobPhase(ctx context.Context, experimentID, clusterName string, window time.Duration) (domain.JobPhase, bool, error) {
	if window <= 0 {
		return domain.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: freshness window must be positive")
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
	snapshotValue, phaseValue, observedAtValue, found, err := s.queryJobStatusSnapshot(ctx, query)
	if err != nil {
		return domain.JobPhasePending, false, err
	}
	if !found {
		return domain.JobPhasePending, false, nil
	}
	if phaseValue == nil && observedAtValue == nil {
		return domain.JobPhaseGone, true, nil
	}
	if phaseValue == nil || observedAtValue == nil {
		return domain.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: incomplete phase observation for %s", experimentID)
	}
	if *observedAtValue != snapshotValue {
		return domain.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: observation timestamp for %s disagrees with its snapshot", experimentID)
	}
	for _, phase := range clusterapi.JobPhases {
		if storedPhase(phase) == *phaseValue {
			return phase, true, nil
		}
	}
	return domain.JobPhasePending, false, fmt.Errorf("metricsdb.LatestJobPhase: invalid value %v", *phaseValue)
}

func (s *Store) queryJobStatusSnapshot(ctx context.Context, query string) (float64, *float64, *float64, bool, error) {
	row, found, err := s.querySingleRow(ctx, "metricsdb.LatestJobPhase", query, 3)
	if err != nil || !found {
		return 0, nil, nil, false, err
	}
	if row[0] == nil {
		return 0, nil, nil, false, fmt.Errorf("metricsdb.LatestJobPhase: malformed snapshot result")
	}
	return *row[0], row[1], row[2], true, nil
}

// querySingleRow runs a SQL query expected to yield exactly one row of cols numeric columns and
// returns that row. found=false means the query returned no rows at all. Individual columns may
// still be nil (SQL NULL) — that is the caller's to interpret. op names the caller in errors.
func (s *Store) querySingleRow(ctx context.Context, op, query string, cols int) ([]*float64, bool, error) {
	u, err := url.Parse(s.url + "/v1/sql")
	if err != nil {
		return nil, false, fmt.Errorf("%s: query URL: %w", op, err)
	}
	params := u.Query()
	params.Set("sql", query)
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("%s: build query: %w", op, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%s: query: %w", op, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("%s: read query: %w", op, err)
	}
	var result struct {
		Code   int    `json:"code"`
		Error  string `json:"error"`
		Output []struct {
			Records *struct {
				Rows [][]*float64 `json:"rows"`
			} `json:"records"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("%s: decode query: %w", op, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("%s: greptimedb returned %d: %s", op, resp.StatusCode, body)
	}
	if result.Error != "" {
		return nil, false, fmt.Errorf("%s: greptimedb query failed: %s", op, result.Error)
	}
	if len(result.Output) != 1 || result.Output[0].Records == nil {
		return nil, false, fmt.Errorf("%s: expected one records result", op)
	}
	rows := result.Output[0].Records.Rows
	if len(rows) == 0 {
		return nil, false, nil
	}
	if len(rows) != 1 || len(rows[0]) != cols {
		return nil, false, fmt.Errorf("%s: malformed result", op)
	}
	for _, value := range rows[0] {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return nil, false, fmt.Errorf("%s: result contains a non-finite value", op)
		}
	}
	return rows[0], true, nil
}

// LatestAcceleratorType resolves the latest accelerator marker from the metrics store.
func (s *Store) LatestAcceleratorType(ctx context.Context, experimentID string, now time.Time, window time.Duration) (domain.AcceleratorType, bool, error) {
	changes, err := s.acceleratorTypeChanges(ctx, experimentID, now.Add(-window), now, time.Second)
	if err != nil {
		return "", false, err
	}
	if len(changes) == 0 {
		return "", false, nil
	}
	return domain.AcceleratorType(changes[len(changes)-1].Type), true, nil
}
