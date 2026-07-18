package domain

import "time"

// MetricDataPoint is a single metric observation during job execution.
type MetricDataPoint struct {
	ExperimentID     string    `json:"experiment_id"`
	MetricName       string    `json:"metric_name"` // Prometheus metric key
	FractionComplete float64   `json:"fraction_complete"`
	MetricValue      float64   `json:"metric_value"`
	RecordedAt       time.Time `json:"recorded_at"`
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
	Points       []MetricSeriesPoint `json:"points"`
}
