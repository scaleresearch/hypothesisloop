package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// ErrInvalidMetric is returned by RecordMetric when an ingested sample is malformed or refers
// to a job that cannot legitimately be reporting progress. Callers surface it as a 4xx rather
// than a 500: the client sent bad data, the server did not fail. Rejecting these at the door
// keeps NaN/Inf and late/forged samples out of the metrics store, where they would otherwise
// poison stage-boundary rankings and silence/decline detection.
var ErrInvalidMetric = errors.New("invalid metric")

// RecordMetric pushes a metric datapoint directly to GreptimeDB via Prometheus remote write.
// Labels: job_id, platform_experiment_id, agent_id, metric_name.
// These labels enable stage-boundary ranking queries to filter and group by experiment/agent.
func (s *Service) RecordMetric(ctx context.Context, experimentID, metricName string, fractionComplete, value float64) error {
	// Reject malformed values before touching the store: NaN/Inf and out-of-range fractions
	// would silently corrupt percentile rankings and completion-progress reads.
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%w: metric_value must be a finite number", ErrInvalidMetric)
	}
	if math.IsNaN(fractionComplete) || math.IsInf(fractionComplete, 0) || fractionComplete < 0 || fractionComplete > 1 {
		return fmt.Errorf("%w: fraction_complete must be in [0,1]", ErrInvalidMetric)
	}

	// Look up experiment to get agent_id and platform_experiment_id for labels. A sample must
	// belong to a real, still-active job: an unknown id or a terminal job reporting progress is
	// a forged/late sample and must not enter the store.
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return fmt.Errorf("registry.RecordMetric: get experiment: %w", err)
	}
	if exp == nil {
		return fmt.Errorf("%w: experiment %s not found", ErrInvalidMetric, experimentID)
	}
	if exp.Status.IsTerminal() {
		return fmt.Errorf("%w: experiment %s is %s (terminal); progress metrics are not accepted", ErrInvalidMetric, experimentID, exp.Status)
	}

	labels := map[string]string{
		"job_id":                 experimentID,
		"platform_experiment_id": exp.PlatformExperimentID,
		"agent_id":               exp.AgentID,
		"metric_name":            metricName,
	}

	// Both samples share one timestamp so GetTimeseries/GetPlatformExperimentTimeseries can
	// zip the two series back together index-for-index (both are pushed at the same cadence).
	at := time.Now().UTC()
	samples := []metricsdb.GaugeSample{
		{MetricName: "experiment_metric_value", Labels: labels, Value: value, At: at},
		{MetricName: "experiment_metric_fraction", Labels: labels, Value: fractionComplete, At: at},
	}
	if err := metricsdb.WriteGaugesAt(ctx, s.metricsDBURL, samples); err != nil {
		return fmt.Errorf("registry.RecordMetric: %w", err)
	}
	return nil
}

// prometheusRangeResult is the response shape for a PromQL range query (matrix), which
// returns every sample in the window — an instant query only ever returns the single latest
// value per series, which cannot show trend-over-time.
type prometheusRangeResult struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]interface{}  `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// GetPlatformExperimentTimeseries returns the full metric history (not just the latest
// value) for every job in platformExpID, one AgentMetricSeries per job — the data a
// competing-agents-over-time dashboard plots. Uses a PromQL range query, since an instant
// query (GetTimeseries) only ever returns the latest sample per series.
func (s *Service) GetPlatformExperimentTimeseries(ctx context.Context, platformExpID, metricName string, lookback time.Duration) ([]*domain.AgentMetricSeries, error) {
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	end := time.Now().UTC()
	start := end.Add(-lookback)
	step := lookback / 500 // cap at ~500 points/series regardless of window size
	if step < time.Second {
		step = time.Second
	}
	// A short-lived job (demo runs report every few seconds and finish in minutes) would
	// otherwise get 1-2 buckets under a long lookback window and look like a single dot;
	// cap the step so recent, fast-moving experiments still render with real resolution.
	const maxStep = 15 * time.Second
	if step > maxStep {
		step = maxStep
	}

	query := fmt.Sprintf(`experiment_metric_value{platform_experiment_id=%q, metric_name=%q}`, platformExpID, metricName)
	apiURL := fmt.Sprintf("%s/v1/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=%ds",
		s.metricsDBURL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: query prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: read body: %w", err)
	}
	var result prometheusRangeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: decode response: %w", err)
	}

	out := make([]*domain.AgentMetricSeries, 0, len(result.Data.Result))
	for _, r := range result.Data.Result {
		series := &domain.AgentMetricSeries{
			AgentID:      r.Metric["agent_id"],
			ExperimentID: r.Metric["job_id"],
			MetricName:   r.Metric["metric_name"],
		}
		for _, v := range r.Values {
			var ts float64
			switch t := v[0].(type) {
			case float64:
				ts = t
			}
			var val float64
			if s, ok := v[1].(string); ok {
				fmt.Sscanf(s, "%f", &val)
			}
			series.Points = append(series.Points, domain.MetricSeriesPoint{
				Timestamp: time.Unix(int64(ts), 0).UTC(),
				Value:     val,
			})
		}
		out = append(out, series)
	}
	return out, nil
}

// GetTimeseries queries GreptimeDB (via its Prometheus-compatible PromQL API) for the
// latest metric values for an experiment.
func (s *Service) GetTimeseries(ctx context.Context, experimentID string) ([]*domain.MetricDataPoint, error) {
	// Range query, not instant: a job's metric history spans however long it's been running,
	// and a single job rarely runs longer than a day — 24h back is a safe, generous window.
	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)

	valueSeries, err := s.queryRange(ctx, fmt.Sprintf(`experiment_metric_value{job_id=%q}`, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	fractionByTS, err := s.queryFractionsByTimestamp(ctx, fmt.Sprintf(`experiment_metric_fraction{job_id=%q}`, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}

	var out []*domain.MetricDataPoint
	for _, r := range valueSeries.Data.Result {
		for _, v := range r.Values {
			var ts float64
			if t, ok := v[0].(float64); ok {
				ts = t
			}
			var val float64
			if s, ok := v[1].(string); ok {
				fmt.Sscanf(s, "%f", &val)
			}
			out = append(out, &domain.MetricDataPoint{
				ExperimentID:     experimentID,
				MetricName:       r.Metric["metric_name"],
				MetricValue:      val,
				FractionComplete: fractionByTS[int64(ts)],
				RecordedAt:       time.Unix(int64(ts), 0).UTC(),
			})
		}
	}
	return out, nil
}

// queryRange runs a PromQL range query against GreptimeDB and returns the raw matrix result.
func (s *Service) queryRange(ctx context.Context, query string, start, end time.Time) (*prometheusRangeResult, error) {
	apiURL := fmt.Sprintf("%s/v1/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=5s",
		s.metricsDBURL, url.QueryEscape(query), start.Unix(), end.Unix())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var result prometheusRangeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// queryFractionsByTimestamp runs a PromQL range query and indexes the results by unix timestamp,
// so GetTimeseries can zip a value sample back up with the fraction_complete recorded alongside
// it (see RecordMetric, which writes both gauges at the same instant).
func (s *Service) queryFractionsByTimestamp(ctx context.Context, query string, start, end time.Time) (map[int64]float64, error) {
	result, err := s.queryRange(ctx, query, start, end)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]float64)
	for _, r := range result.Data.Result {
		for _, v := range r.Values {
			var ts float64
			if t, ok := v[0].(float64); ok {
				ts = t
			}
			var val float64
			if s, ok := v[1].(string); ok {
				fmt.Sscanf(s, "%f", &val)
			}
			out[int64(ts)] = val
		}
	}
	return out, nil
}
