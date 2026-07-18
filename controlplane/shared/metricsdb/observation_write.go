package metricsdb

import (
	"context"
	"time"
)

// aliveHeartbeatMetric is written once per real observation of an experiment being alive —
// a node-agent CPU sample tagged with the pod's experiment ID, or (via IsAlive/
// ObservedElapsedHours also matching experiment_metric_value) a job-reported training metric.
// This file is the only place that reads or writes "how long has this experiment really run":
// every caller (silence detection, refund/eviction accounting, phase-2 boundary, the scheduler's
// Cancel handler) calls straight into GreptimeDB, so the answer is always recomputed from what's
// actually stored — no cache, in this process or any other, to fall out of sync or get lost on
// restart.
const aliveHeartbeatMetric = "experiment_alive_heartbeat"

// RecordObservation writes a single "experimentID was alive" sample at `at` — the agent's own
// collection time, not receipt time (see WriteGaugeAt). extraLabels (pod_uid, node, ...) are
// carried for debugging only; every read here collapses across them.
func RecordObservation(ctx context.Context, dbURL, experimentID string, at time.Time, extraLabels map[string]string) error {
	return WriteGaugesAt(ctx, dbURL, []GaugeSample{observationSample(experimentID, at, extraLabels)})
}

// Observation is one pod's "experimentID was alive" reading for RecordObservations' batch write —
// the same shape as RecordObservation's arguments, just collected up front so every pod a single
// push payload reports becomes one remote-write request instead of one per pod.
type Observation struct {
	ExperimentID string
	At           time.Time
	ExtraLabels  map[string]string
}

// RecordObservations writes every observation in one Prometheus remote-write request — the batch
// form of RecordObservation, for a caller (the node-agent push handler) that already has several
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
