package registry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// ErrInvalidMetric is returned by RecordMetric when an ingested sample is malformed or refers
// to a job that cannot legitimately be reporting progress. Callers surface it as a 4xx rather
// than a 500: the client sent bad data, the server did not fail. Rejecting these at the door
// keeps NaN/Inf and late/forged samples out of the metrics store, where they would otherwise
// poison stage-boundary rankings and silence/decline detection.
var ErrInvalidMetric = errors.New("invalid metric")

// ErrExperimentNotFound is returned when an operation names an experiment that does not exist.
// A distinct sentinel from ErrInvalidMetric so the handlers can answer 404 rather than
// reporting a caller's typo as a server fault or as a malformed metric.
var ErrExperimentNotFound = errors.New("experiment not found")

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
	// Both become Prometheus labels, so both must stay small and bounded — unconstrained free
	// text is unbounded label cardinality in the metrics store. metric_name matters at least as
	// much as metric_basis: a job embedding a step counter in the name creates one series per
	// sample, degrading the exact ranking and silence queries this check protects.
	if err := validMetricLabel("metric_name", metricName); err != nil {
		return err
	}
	if err := validMetricLabel("metric_basis", basis); err != nil {
		return err
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
		return fmt.Errorf("%w: %s", ErrExperimentNotFound, experimentID)
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
		{MetricName: metricsdb.ExperimentMetricValue, Labels: labels, Value: value, At: at},
		{MetricName: metricsdb.ExperimentMetricFraction, Labels: labels, Value: fractionComplete, At: at},
	}
	if err := metricsdb.WriteGaugesAt(ctx, s.metricsDBURL, samples); err != nil {
		return fmt.Errorf("registry.RecordMetric: %w", err)
	}
	s.notifyMetricArrived(ctx, experimentID, exp.PlatformExperimentID, exp.AgentID, metricName, fractionComplete)
	return nil
}

// notifyMetricArrived tells subscribers that a sample landed, and tells them nothing about it
// beyond where to find it: experiment, metric name, how far through the run it was. The value
// itself stays in the metrics store, which is the only place it is ever kept — a subscriber that
// wants the number reads it from there. Emitted after the write, never before: an event for a
// sample the store rejected would report a state nothing holds.
//
// A failure to notify is logged and not returned. The metric is already durably stored, and
// refusing an accepted push because a transient stream could not be told about it would turn a
// notification outage into lost measurements.
func (s *Service) notifyMetricArrived(ctx context.Context, experimentID, platformExperimentID, agentID, metricName string, fractionComplete float64) {
	if s.events == nil {
		return
	}
	err := s.events.NotifyEvent(ctx, db.Event{
		Kind:                 db.EventMetricPoint,
		Subject:              experimentID,
		Value:                metricName,
		Detail:               strconv.FormatFloat(fractionComplete, 'g', -1, 64),
		PlatformExperimentID: platformExperimentID,
		AgentID:              agentID,
		Cursor:               db.NewCursor(time.Now().UTC()),
	})
	if err != nil {
		s.logger.Warn("registry: notify metric arrival", zap.String("experiment_id", experimentID), zap.Error(err))
	}
}

// validMetricLabel bounds a value that becomes a Prometheus label.
func validMetricLabel(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidMetric, field)
	}
	if len(value) > 64 {
		return fmt.Errorf("%w: %s must be at most 64 characters", ErrInvalidMetric, field)
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return fmt.Errorf("%w: %s must be printable ASCII", ErrInvalidMetric, field)
		}
	}
	return nil
}

// Range queries return one metricsdb.RangeSeries per distinct label combination (job_id,
// agent_id, metric_name, ...). A job that retried onto a different agent produces two series for
// the same job_id+metric_name, distinguished by agent_id — that label difference is the real
// attempt identity, not something to be inferred later from flattened data.

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

	query := fmt.Sprintf(`%s{platform_experiment_id=%q, metric_name=%q}`, metricsdb.ExperimentMetricValue, platformExpID, metricName)
	result, err := metricsdb.QueryRange(ctx, s.metricsDBURL, query, start, end, step)
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
		return nil, fmt.Errorf("%w: %s", ErrExperimentNotFound, experimentID)
	}
	end := time.Now().UTC()
	start := exp.CreatedAt

	valueSeries, err := s.queryRange(ctx, fmt.Sprintf(`%s{job_id=%q}`, metricsdb.ExperimentMetricValue, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	fractionSeries, err := s.queryRange(ctx, fmt.Sprintf(`%s{job_id=%q}`, metricsdb.ExperimentMetricFraction, experimentID), start, end)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: %w", err)
	}
	// Index fraction samples by (agent_id, metric_name, timestamp) rather than by timestamp
	// alone: RecordMetric writes both gauges for the same labels at the same instant, so this
	// recovers the per-attempt, per-metric fraction alongside each value sample without
	// collapsing distinct agent_id/metric_name label-sets (distinct retry attempts, or a job
	// reporting more than one metric_name in the same second) onto one shared timestamp map.
	fractionByAgentTS := make(map[string]map[int64]float64)
	for _, r := range fractionSeries {
		key := r.Labels["agent_id"] + "\x00" + r.Labels["metric_name"]
		m, ok := fractionByAgentTS[key]
		if !ok {
			m = make(map[int64]float64)
			fractionByAgentTS[key] = m
		}
		for _, p := range r.Points {
			m[p.Time.Unix()] = p.Value
		}
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
			ts := p.Time.Unix()
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

// queryRange runs a PromQL range query against GreptimeDB on the metric-store's own client, which
// checks the transport status and the Prometheus envelope's status/resultType. A hand-rolled
// client here used to decode a backend error into an empty successful matrix, so "the metrics
// store rejected this query" reached the API as "this run produced no measurements".
// queryRange reads one series over [start, end) at a step sized to the window, the same cap
// GetPlatformExperimentTimeseries uses. A fixed 5s step is fine for a short run and absurd for a
// long one: a fortnight of samples is a quarter of a million points per series, on an endpoint a
// dashboard calls.
func (s *Service) queryRange(ctx context.Context, query string, start, end time.Time) ([]metricsdb.RangeSeries, error) {
	step := end.Sub(start) / 500 // ~500 points/series regardless of window size
	if step < 5*time.Second {
		step = 5 * time.Second
	}
	return metricsdb.QueryRange(ctx, s.metricsDBURL, query, start, end, step)
}
