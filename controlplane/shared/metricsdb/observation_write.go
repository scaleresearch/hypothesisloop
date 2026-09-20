package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// aliveHeartbeatMetric is written once per real observation of an experiment being alive —
// a node-agent CPU sample tagged with the pod's experiment ID, or (via IsAlive/
// ObservedElapsedHours also matching experiment_metric_value) a job-reported training metric.
// This file is the only place that reads or writes "how long has this experiment really run":
// every caller (silence detection, refund/eviction accounting, stage boundaries, the scheduler's
// Cancel handler) calls straight into GreptimeDB, so the answer is always recomputed from what's
// actually stored — no cache, in this process or any other, to fall out of sync or get lost on
// restart.
const aliveHeartbeatMetric = "experiment_alive_heartbeat"

// The two series a job's own reporting writes: its metric values, and how far through the run each
// was recorded. Exported because the writer (registry.RecordMetric) and every reader must name the
// same series — a mistyped metric name in a PromQL selector is not an error, it is an empty result,
// which reads downstream as "this run produced no measurements".
const (
	ExperimentMetricValue    = "experiment_metric_value"
	ExperimentMetricFraction = "experiment_metric_fraction"
)

// The observation tables as remote write would create them on a first sample, declared up front
// with every label a reader filters on. ObserveSpan reads both series in one statement, which
// fails as a whole while either table is missing, so a store where no job had reported a metric
// yet billed every job as never observed.
var ensureObservationTablesSQL = []string{
	`CREATE TABLE IF NOT EXISTS greptime_physical_table (
	greptime_timestamp TIMESTAMP(3) NOT NULL,
	greptime_value DOUBLE NULL,
	TIME INDEX (greptime_timestamp)
) ENGINE=metric WITH ("physical_metric_table" = "")`,
	`CREATE TABLE IF NOT EXISTS ` + aliveHeartbeatMetric + ` (
	greptime_timestamp TIMESTAMP(3) NOT NULL,
	greptime_value DOUBLE NULL,
	experiment_id STRING NULL,
	TIME INDEX (greptime_timestamp),
	PRIMARY KEY (experiment_id)
) ENGINE=metric WITH ("on_physical_table" = "greptime_physical_table")`,
	`CREATE TABLE IF NOT EXISTS ` + ExperimentMetricValue + ` (
	greptime_timestamp TIMESTAMP(3) NOT NULL,
	greptime_value DOUBLE NULL,
	agent_id STRING NULL,
	job_id STRING NULL,
	metric_basis STRING NULL,
	metric_name STRING NULL,
	platform_experiment_id STRING NULL,
	TIME INDEX (greptime_timestamp),
	PRIMARY KEY (agent_id, job_id, metric_basis, metric_name, platform_experiment_id)
) ENGINE=metric WITH ("on_physical_table" = "greptime_physical_table")`,
}

// EnsureObservationTables creates the two series ObserveSpan reads, so that a missing one is a
// fault and never an empty answer. Idempotent; run once at startup.
func EnsureObservationTables(ctx context.Context, dbURL string) error {
	for _, ddl := range ensureObservationTablesSQL {
		if err := execSQL(ctx, dbURL, ddl); err != nil {
			return fmt.Errorf("metricsdb.EnsureObservationTables: %w", err)
		}
	}
	return nil
}

// Observation is one pod's "experimentID was alive" reading at its collection time. ExtraLabels
// (pod_uid, node, ...) are carried for debugging only; every read collapses across them.
type Observation struct {
	ExperimentID string
	At           time.Time
	ExtraLabels  map[string]string
}

// RecordObservations writes every observation in one Prometheus remote-write request, for a caller (the node-agent push handler) that already has several
// pods' readings in hand from a single payload and would otherwise issue one HTTP round trip per
// pod for what is, from GreptimeDB's point of view, one write.
func RecordObservations(ctx context.Context, dbURL string, observations []Observation) error {
	samples := make([]GaugeSample, 0, len(observations))
	for _, o := range observations {
		samples = append(samples, observationSample(o.ExperimentID, o.At, o.ExtraLabels))
	}
	return WriteGaugesAt(ctx, dbURL, samples)
}

func observationSample(experimentID string, at time.Time, extraLabels map[string]string) GaugeSample {
	labels := map[string]string{"experiment_id": experimentID}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return GaugeSample{MetricName: aliveHeartbeatMetric, Labels: labels, Value: 1, At: at}
}
