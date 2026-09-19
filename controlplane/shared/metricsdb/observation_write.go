package metricsdb

import (
	"context"
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
