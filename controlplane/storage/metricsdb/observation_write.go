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

// admissionMetric holds one sample per admission of an experiment, at the moment it was given
// capacity. A job requeued and admitted again within the gap cap would otherwise read as one
// unbroken run, and be billed for the time it waited in the queue.
const admissionMetric = "experiment_admitted"

// The two series a job's own reporting writes (RecordJobMetric): its metric values, and how far
// through the run each was recorded. Exported because every reader must name the same series — a
// mistyped metric name in a PromQL selector is not an error, it is an empty result, which reads
// downstream as "this run produced no measurements".
const (
	ExperimentMetricValue    = "experiment_metric_value"
	ExperimentMetricFraction = "experiment_metric_fraction"
)

// JobMetric is one value of a declared metric a job reported, and how far through its run it was.
type JobMetric struct {
	JobID, PlatformExperimentID, AgentID string
	Name, Basis                          string
	Value, FractionComplete              float64
	At                                   time.Time
}

// RecordJobMetric writes a job's metric value and its fraction complete under one timestamp, so a
// reader zips the two series back together point for point.
func (s *Store) RecordJobMetric(ctx context.Context, m JobMetric) error {
	labels := map[string]string{
		"job_id":                 m.JobID,
		"platform_experiment_id": m.PlatformExperimentID,
		"agent_id":               m.AgentID,
		"metric_name":            m.Name,
		"metric_basis":           m.Basis,
	}
	return s.writeGauges(ctx, []gauge{
		{table: experimentMetric, labels: labels, value: m.Value, at: m.At},
		{table: experimentFraction, labels: labels, value: m.FractionComplete, at: m.At},
	})
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
func (s *Store) RecordObservations(ctx context.Context, observations []Observation) error {
	samples := make([]gauge, 0, len(observations))
	for _, o := range observations {
		samples = append(samples, observationSample(o.ExperimentID, o.At, o.ExtraLabels))
	}
	return s.writeGauges(ctx, samples)
}

// RecordAdmission records that experimentID is being given capacity at `at`. Written before the
// admission is claimed: observations on either side of it are never counted as one run.
func (s *Store) RecordAdmission(ctx context.Context, experimentID string, at time.Time) error {
	return s.writeGauges(ctx, []gauge{{
		table: admission, labels: map[string]string{"experiment_id": experimentID}, value: 1, at: at,
	}})
}

func observationSample(experimentID string, at time.Time, extraLabels map[string]string) gauge {
	labels := map[string]string{"experiment_id": experimentID}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return gauge{table: aliveHeartbeat, labels: labels, value: 1, at: at}
}
