package domain

import "time"

// MetricDataPoint is a single metric observation during job execution.
type MetricDataPoint struct {
	ExperimentID     string  `json:"experiment_id"`
	MetricName       string  `json:"metric_name"` // Prometheus metric key
	FractionComplete float64 `json:"fraction_complete"`
	MetricValue      float64 `json:"metric_value"`
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
	AgentID      string `json:"agent_id"`
	ExperimentID string `json:"experiment_id"`
	MetricName   string `json:"metric_name"`
	// MetricBasis is the metric_basis label shared by every point in this series — a job
	// that switches basis mid-run produces a second, distinct series rather than one series
	// mixing two scales. See MetricDataPoint.MetricBasis.
	MetricBasis string              `json:"metric_basis"`
	Points      []MetricSeriesPoint `json:"points"`
}

// JobMetric is the one value a job is read by on a declared metric: the last it reported, which
// is what the code it ran ends with. Standings rank it on a ranking metric and a bound is held
// against it on a constraint. A job read by the best point of its curve ranked on its luckiest
// evaluation, and a baseline read so stood higher than its code measures.
type JobMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Basis string  `json:"basis"`
}
