package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Range queries return one metricsdb.RangeSeries per distinct label combination (job_id,
// agent_id, metric_name, ...). A job that retried onto a different agent produces two series for
// the same job_id+metric_name, distinguished by agent_id — that label difference is the real
// attempt identity, not something to be inferred later from flattened data.

// GetPlatformExperimentTimeseries returns the full metric history (not just the latest
// value) for every job in platformExpID, one AgentMetricSeries per job — the data a
// competing-agents-over-time dashboard plots. Uses a PromQL range query, since an instant
// query (GetTimeseries) only ever returns the latest sample per series. The window is the lookback
// before end; an end that is zero or still ahead means now.
func (s *Service) GetPlatformExperimentTimeseries(ctx context.Context, platformExpID, metricName string, lookback time.Duration, end time.Time) ([]*domain.AgentMetricSeries, error) {
	return s.platformExperimentSeries(ctx, metricsdb.ExperimentMetricValue, platformExpID, metricName, lookback, end)
}

// platformExperimentSeries reads one of the two gauges a metric sample is written as: its value
// or the fraction_complete it was reported at.
func (s *Service) platformExperimentSeries(ctx context.Context, gauge, platformExpID, metricName string, lookback time.Duration, end time.Time) ([]*domain.AgentMetricSeries, error) {
	if now := time.Now().UTC(); end.IsZero() || end.After(now) {
		end = now
	}
	start := end.Add(-lookback)
	step := lookback / 500 // cap at ~500 points/series regardless of window size
	if step < time.Second {
		step = time.Second
	}

	query := fmt.Sprintf(`%s{platform_experiment_id=%q, metric_name=%q}`, gauge, platformExpID, metricName)
	result, err := s.metrics.QueryRange(ctx, query, start, end, step)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: %w", err)
	}

	out := make([]*domain.AgentMetricSeries, 0, len(result))
	for _, r := range result {
		basis := r.Labels["metric_basis"]
		if basis == "" {
			basis = "raw"
		}
		series := &domain.AgentMetricSeries{
			AgentID:      r.Labels["agent_id"],
			ExperimentID: r.Labels["job_id"],
			MetricName:   r.Labels["metric_name"],
			MetricBasis:  basis,
		}
		for _, p := range r.Points {
			series.Points = append(series.Points, domain.MetricSeriesPoint{
				Timestamp: p.Time,
				Value:     p.Value,
			})
		}
		out = append(out, series)
	}
	return out, nil
}

// GetTimeseries returns the metric samples an experiment recorded, as recorded.
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
		return nil, fmt.Errorf("%w: %s", ErrExperimentNotFound, experimentID)
	}
	end := time.Now().UTC()
	start := exp.CreatedAt

	// The samples as recorded, never a grid resampling of them. Every node of a distributed job
	// reports under one label set, so its ranks' samples land microseconds apart in one series;
	// a range query keeps only the last one in each step and repeats it, which turned three
	// ranks reporting three different values into one value reported all run long.
	recorded, err := s.metrics.ExperimentMetricSamples(ctx, experimentID, start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	valueSeries := seriesByLabels(recorded.Values)
	// Index fraction samples by (agent_id, metric_name, timestamp) rather than by timestamp
	// alone: RecordMetric writes both gauges for the same labels at the same instant, so this
	// recovers the per-attempt, per-metric fraction alongside each value sample without
	// collapsing distinct agent_id/metric_name label-sets (distinct retry attempts, or a job
	// reporting more than one metric_name in the same second) onto one shared timestamp map.
	// Keyed to the millisecond, which is the resolution the pair was written at: at second
	// resolution two ranks reporting different fractions inside one second overwrite each other.
	fractionByAgentTS := make(map[string]map[int64]float64)
	for _, sample := range recorded.Fractions {
		key := sample.Labels["agent_id"] + "\x00" + sample.Labels["metric_name"]
		m, ok := fractionByAgentTS[key]
		if !ok {
			m = make(map[int64]float64)
			fractionByAgentTS[key] = m
		}
		m[sample.At.UnixMilli()] = sample.Value
	}

	out := latestAttemptOnly(experimentID, valueSeries, fractionByAgentTS)
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
func latestAttemptOnly(experimentID string, valueResult []metricsdb.RangeSeries, fractionByAgentTS map[string]map[int64]float64) []*domain.MetricDataPoint {
	type series struct {
		agentID    string
		metricName string
		firstTS    int64
		points     []*domain.MetricDataPoint
	}

	byMetric := make(map[string][]*series)
	var order []string
	for _, r := range valueResult {
		metricName := r.Labels["metric_name"]
		agentID := r.Labels["agent_id"]
		basis := r.Labels["metric_basis"]
		if basis == "" {
			basis = "raw"
		}
		fractions := fractionByAgentTS[agentID+"\x00"+metricName]

		sr := &series{agentID: agentID, metricName: metricName}
		for _, p := range r.Points {
			ts := p.Time.UnixMilli()
			if len(sr.points) == 0 {
				sr.firstTS = ts
			}
			sr.points = append(sr.points, &domain.MetricDataPoint{
				ExperimentID:     experimentID,
				MetricName:       metricName,
				MetricValue:      p.Value,
				MetricBasis:      basis,
				FractionComplete: fractions[ts],
				RecordedAt:       p.Time,
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

// seriesByLabels folds recorded samples back into one series per distinct label combination —
// the same shape a range query hands back, and the shape latestAttemptOnly reasons about, but
// built from the samples themselves so nothing is dropped or repeated on the way. Series appear
// in the order their first sample did, and each keeps its samples in the order they were taken.
func seriesByLabels(samples []metricsdb.VectorSample) []metricsdb.RangeSeries {
	byKey := make(map[string]*metricsdb.RangeSeries, len(samples))
	var order []string
	for _, sample := range samples {
		names := make([]string, 0, len(sample.Labels))
		for name := range sample.Labels {
			names = append(names, name)
		}
		sort.Strings(names)
		var key strings.Builder
		for _, name := range names {
			key.WriteString(name)
			key.WriteByte('=')
			key.WriteString(sample.Labels[name])
			key.WriteByte('\x00')
		}
		series, ok := byKey[key.String()]
		if !ok {
			series = &metricsdb.RangeSeries{Labels: sample.Labels}
			byKey[key.String()] = series
			order = append(order, key.String())
		}
		series.Points = append(series.Points, metricsdb.RangePoint{Time: sample.At, Value: sample.Value})
	}
	out := make([]metricsdb.RangeSeries, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}
