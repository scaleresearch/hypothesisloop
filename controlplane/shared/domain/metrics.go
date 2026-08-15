package domain

import "time"

// MetricDataPoint is a single metric observation during job execution.
type MetricDataPoint struct {
	ExperimentID     string    `json:"experiment_id"`
	MetricName       string    `json:"metric_name"` // Prometheus metric key
	FractionComplete float64   `json:"fraction_complete"`
	MetricValue      float64   `json:"metric_value"`
	// MetricBasis is what scale/definition MetricValue is on. "raw" (the default) means
	// unmodified; anything else means the job transformed what the number means (e.g. a
	// different denormalization reference) and it must never be silently compared to a "raw"
	// value from another run.
	MetricBasis string    `json:"metric_basis"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// MetricSeriesPoint is one (time, value) sample within an AgentMetricSeries.
type MetricSeriesPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// AgentMetricSeries is one competing job's full metric history within a platform
// experiment — the unit the leaderboard/competition dashboard plots one line per.
type AgentMetricSeries struct {
	AgentID      string              `json:"agent_id"`
	ExperimentID string              `json:"experiment_id"`
	MetricName   string              `json:"metric_name"`
	// MetricBasis is the metric_basis label shared by every point in this series — a job
	// that switches basis mid-run produces a second, distinct series rather than one series
	// mixing two scales. See MetricDataPoint.MetricBasis.
	MetricBasis string              `json:"metric_basis"`
	Points      []MetricSeriesPoint `json:"points"`
}
