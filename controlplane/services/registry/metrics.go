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
	"sort"
	"strconv"
	"time"
	"unicode"

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
// Labels: job_id, platform_experiment_id, agent_id, metric_name, metric_basis.
// These labels enable stage-boundary ranking queries to filter and group by experiment/agent.
//
// basis records what scale/definition the value is on — "raw" (the default) means the loss/
// metric target has not been transformed. A job that changes what its reported number means
// (e.g. denormalizing against a different reference than usual) must set a non-"raw" basis, or
// its values silently rank against unmodified runs on the wrong scale — this is what let a
// ~20% "win" that was actually ~2x worse into a ranking undetected. Never inferred: only the
// job that knows it changed the scale can say so.
func (s *Service) RecordMetric(ctx context.Context, experimentID, metricName, basis string, fractionComplete, value float64) error {
	if basis == "" {
		basis = "raw"
	}
	// metric_basis becomes a Prometheus label, so it must stay small and bounded — unconstrained
	// free text here is unbounded label cardinality in the metrics store.
	if len(basis) > 64 {
		return fmt.Errorf("%w: metric_basis must be at most 64 characters", ErrInvalidMetric)
	}
	for _, r := range basis {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return fmt.Errorf("%w: metric_basis must be printable ASCII", ErrInvalidMetric)
		}
	}
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
		"metric_basis":           basis,
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

// prometheusSeries is one label-identified series within a PromQL range-query result: one
// distinct combination of labels (job_id, agent_id, metric_name, ...) with its own ordered
// samples. A job that retried onto a different agent produces two prometheusSeries for the
// same job_id+metric_name, distinguished by agent_id — that label difference is the real
// attempt identity, not something to be inferred later from flattened data.
type prometheusSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]interface{}  `json:"values"`
}

// prometheusRangeResult is the response shape for a PromQL range query (matrix), which
// returns every sample in the window — an instant query only ever returns the single latest
// value per series, which cannot show trend-over-time.
type prometheusRangeResult struct {
	Data struct {
		Result []prometheusSeries `json:"result"`
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
		basis := r.Metric["metric_basis"]
		if basis == "" {
			basis = "raw"
		}
		series := &domain.AgentMetricSeries{
			AgentID:      r.Metric["agent_id"],
			ExperimentID: r.Metric["job_id"],
			MetricName:   r.Metric["metric_name"],
			MetricBasis:  basis,
		}
		for _, v := range r.Values {
			var ts float64
			switch t := v[0].(type) {
			case float64:
				ts = t
			}
			val, ok := parseSampleValue(v[1])
			if !ok {
				continue
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
	// The window is the experiment's own lifetime, read from Postgres — never a constant. A fixed
	// lookback silently truncates: this endpoint reads *finished* experiments' results, which
	// outlive the job by the length of the run, so a 24h bound made every result of a multi-day
	// run unreadable while the samples sat intact in the metrics store, and "no measurements" was
	// indistinguishable from "measurements older than the bound".
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	if exp == nil {
		return nil, fmt.Errorf("registry.GetTimeseries: experiment %s not found", experimentID)
	}
	end := time.Now().UTC()
	start := exp.CreatedAt

	valueSeries, err := s.queryRange(ctx, fmt.Sprintf(`experiment_metric_value{job_id=%q}`, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	fractionSeries, err := s.queryRange(ctx, fmt.Sprintf(`experiment_metric_fraction{job_id=%q}`, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	// Index fraction samples by (agent_id, metric_name, timestamp) rather than by timestamp
	// alone: RecordMetric writes both gauges for the same labels at the same instant, so this
	// recovers the per-attempt, per-metric fraction alongside each value sample without
	// collapsing distinct agent_id/metric_name label-sets (distinct retry attempts, or a job
	// reporting more than one metric_name in the same second) onto one shared timestamp map.
	fractionByAgentTS := make(map[string]map[int64]float64)
	for _, r := range fractionSeries.Data.Result {
		key := r.Metric["agent_id"] + "\x00" + r.Metric["metric_name"]
		m, ok := fractionByAgentTS[key]
		if !ok {
			m = make(map[int64]float64)
			fractionByAgentTS[key] = m
		}
		for _, v := range r.Values {
			var ts float64
			if t, ok := v[0].(float64); ok {
				ts = t
			}
			val, ok := parseSampleValue(v[1])
			if !ok {
				continue
			}
			m[int64(ts)] = val
		}
	}

	out := latestAttemptOnly(experimentID, valueSeries.Data.Result, fractionByAgentTS)
	// Points from the surviving attempt's series (one per metric_name) are appended series by
	// series; sort once here, at the one place all consumers read from, so every chart gets a
	// single time-ordered line instead of every caller re-sorting defensively.
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt.Before(out[j].RecordedAt) })
	return out, nil
}

// latestAttemptOnly picks, for each metric_name, the PromQL series representing the surviving
// retry attempt and converts only its points to MetricDataPoints — discarding superseded
// attempts before their points ever get merged into one flat, label-blind array. A retried job
// keeps the dead attempt's points in the metrics store under the same job_id; if the retry was
// rescheduled onto a different agent (a documented occurrence — "per-retry agent_id variants"),
// the dead and surviving attempts are two distinct, correctly label-identified series, and the
// surviving one is simply the series whose points start latest. Only when a single series exists
// per metric_name (a true same-agent in-place restart, where the runtime reused its own pod) is
// there no label to key on; there, and only there, we fall back to cutting the series at its own
// last fraction_complete regression.
func latestAttemptOnly(experimentID string, valueResult []prometheusSeries, fractionByAgentTS map[string]map[int64]float64) []*domain.MetricDataPoint {
	type series struct {
		agentID    string
		metricName string
		firstTS    float64
		points     []*domain.MetricDataPoint
	}

	byMetric := make(map[string][]*series)
	var order []string
	for _, r := range valueResult {
		metricName := r.Metric["metric_name"]
		agentID := r.Metric["agent_id"]
		basis := r.Metric["metric_basis"]
		if basis == "" {
			basis = "raw"
		}
		fractions := fractionByAgentTS[agentID+"\x00"+metricName]

		sr := &series{agentID: agentID, metricName: metricName}
		for _, v := range r.Values {
			var ts float64
			if t, ok := v[0].(float64); ok {
				ts = t
			}
			val, ok := parseSampleValue(v[1])
			if !ok {
				continue
			}
			if len(sr.points) == 0 {
				sr.firstTS = ts
			}
			sr.points = append(sr.points, &domain.MetricDataPoint{
				ExperimentID:     experimentID,
				MetricName:       metricName,
				MetricValue:      val,
				MetricBasis:      basis,
				FractionComplete: fractions[int64(ts)],
				RecordedAt:       time.Unix(int64(ts), 0).UTC(),
			})
		}
		if len(sr.points) == 0 {
			continue
		}
		if _, ok := byMetric[metricName]; !ok {
			order = append(order, metricName)
		}
		byMetric[metricName] = append(byMetric[metricName], sr)
	}

	var out []*domain.MetricDataPoint
	for _, name := range order {
		attempts := byMetric[name]
		if len(attempts) > 1 {
			// Multiple label-distinct series for one metric_name: keep the one that started
			// most recently, since that is the attempt that superseded the others.
			latest := attempts[0]
			for _, a := range attempts[1:] {
				if a.firstTS > latest.firstTS {
					latest = a
				}
			}
			out = append(out, latest.points...)
			continue
		}
		// Single series: same-agent in-place restart is the only case left that can still show
		// a fraction_complete regression, so cut narrowly at its own last regression point.
		points := attempts[0].points
		start := 0
		for i := 1; i < len(points); i++ {
			if points[i].FractionComplete < points[i-1].FractionComplete {
				start = i
			}
		}
		out = append(out, points[start:]...)
	}
	return out
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

// parseSampleValue converts one Prometheus sample value. A sample the backend cannot express as
// a number is skipped rather than recorded as 0: these values feed rankings and stage cuts, where
// a spurious 0 reads as a real result — a perfect score on any minimized metric.
func parseSampleValue(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
