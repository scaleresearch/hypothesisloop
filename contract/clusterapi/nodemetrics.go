package clusterapi

import "time"

// NodeMetricsPush is one node's observation of the job pods running on it
// (POST /internal/node-metrics), at the time it was collected, so a push delivered late still
// lands on the instant it was observed at.
type NodeMetricsPush struct {
	Node      string      `json:"node"`
	Timestamp time.Time   `json:"timestamp"`
	Pods      []PodSample `json:"pods"`
}

// PodSample is one pod observed running, with the job it belongs to.
type PodSample struct {
	PodUID       string `json:"pod_uid"`
	ExperimentID string `json:"experiment_id,omitempty"`
}
