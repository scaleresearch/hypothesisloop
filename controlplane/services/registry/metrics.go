package registry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
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
	s.checkUnderperformance(ctx, exp, metricName, fractionComplete, value, at)
	return nil
}

const (
	// underperformCheckpointWidth throttles the cohort comparison to roughly every 10% of
	// progress instead of every sample. It costs one range query against the metrics store, so
	// running it on every high-cadence sample would turn a reactive check into the metrics
	// store's biggest caller; a job reports fraction_complete monotonically, so checkpoints
	// this coarse are still frequent enough to catch a stretch of underperformance early.
	underperformCheckpointWidth = 0.1
	underperformCheckpointEps   = 0.01
	// underperformMinFraction skips the noisiest part of a run, where one or two early samples
	// can rank last by chance alone.
	underperformMinFraction = 0.2
	// underperformCohortMinSize is how many *other* jobs must have reported for the comparison
	// to mean anything; a "cohort" of one sibling is a coin flip, not a signal.
	underperformCohortMinSize = 3
	// underperformSeparation is how far below the cohort's worst-of-the-rest a job must fall,
	// expressed as a fraction of the cohort's own spread (best minus worst-of-the-rest), before
	// it is called out. This is scale-free — it never assumes what units the metric is in — and
	// it demands the job be clearly cut off from the pack, not merely last by a hair.
	underperformSeparation = 0.25
	// underperformSiblingStaleness bounds how old a sibling's last-reported value may be and
	// still count toward the cohort. Without this, a 24h-lookback comparison could rank a live
	// job against a sibling that finished or stalled an hour ago — a different phase of the run,
	// not a fair comparison of "right now."
	underperformSiblingStaleness = 15 * time.Minute
	// underperformMinRemaining is how much of the run must be left, at its pace since admission,
	// for the advisory to be sent: cancelling a job that ends before its agent has read the
	// advisory saves nothing, and reading it costs the agent a turn.
	underperformMinRemaining = 2 * time.Minute
)

// checkUnderperformance compares this job's just-recorded ranking-metric sample to its cohort
// (every other job in the same platform experiment reporting the same metric) and, if it is the
// clear straggler with underperformMinRemaining of its run still ahead, tells only the owning
// agent so: once per job, never a command. See db.EventJobUnderperforming.
//
// Trailing is recomputed from the metrics store every time; the only thing kept is the marker on
// the job's row saying its owner was told. A job launched against siblings that already finished
// trails from its first sample, so the check asks whether it trails now, never whether it just
// started to.
//
// The query runs the instant after this call wrote its own sample and remote-write ingestion is
// not synchronous, so a sibling's newest point can be missing. That under-fires for one
// checkpoint, the safe direction: the next checkpoint sees it.
func (s *Service) checkUnderperformance(ctx context.Context, exp *domain.Experiment, metricName string, fractionComplete, value float64, at time.Time) {
	if s.events == nil || exp.PlatformExperimentID == "" {
		return
	}
	if fractionComplete < underperformMinFraction {
		return
	}
	if exp.SubmittedAt == nil {
		return
	}
	if remaining := time.Duration(float64(at.Sub(*exp.SubmittedAt)) * (1 - fractionComplete) / fractionComplete); remaining < underperformMinRemaining {
		return
	}
	// Only evaluate near a checkpoint (see underperformCheckpointWidth) — this is the throttle,
	// derived purely from the sample itself, not from anything remembered about earlier ones.
	if rem := math.Mod(fractionComplete, underperformCheckpointWidth); rem > underperformCheckpointEps && rem < underperformCheckpointWidth-underperformCheckpointEps {
		return
	}

	metrics, found, err := s.store.GetPlatformExperimentMetrics(ctx, exp.PlatformExperimentID)
	if err != nil || !found {
		return
	}
	var direction string
	for _, m := range metrics {
		if m.Key == metricName && m.EffectiveRole() == domain.MetricRoleRanking {
			direction = m.Direction
			break
		}
	}
	if direction == "" {
		// Not a ranking metric (or not declared at all) — nothing to rank it against.
		return
	}

	series, err := s.GetPlatformExperimentTimeseries(ctx, exp.PlatformExperimentID, metricName, underperformSiblingStaleness+5*time.Minute, time.Time{})
	if err != nil {
		s.logger.Warn("registry: underperformance check", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}

	score := func(v float64) float64 {
		if direction == "minimize" {
			return -v
		}
		return v
	}

	var siblingScores []float64
	for _, s := range series {
		if s.ExperimentID == exp.ID {
			continue
		}
		if len(s.Points) == 0 {
			continue
		}
		last := s.Points[len(s.Points)-1]
		// A sibling that stopped reporting a while ago is comparing a different phase of the
		// run, not the same moment — a job that finished strong an hour ago is not evidence this
		// one is trailing right now. Silence itself is a separate, already-handled concern
		// (report_interval_seconds/silent-eviction); this check only compares live cohorts.
		if time.Since(last.Timestamp) > underperformSiblingStaleness {
			continue
		}
		siblingScores = append(siblingScores, score(last.Value))
	}
	if len(siblingScores) < underperformCohortMinSize {
		return
	}

	best, worst := siblingScores[0], siblingScores[0]
	for _, v := range siblingScores[1:] {
		best, worst = max(best, v), min(worst, v)
	}
	// The margin is a fraction of the cohort's own spread, so it assumes no unit. A cohort tied on
	// one value has no spread, and any gap at all would clear a zero margin: it is then scaled off
	// the cohort's value.
	margin := underperformSeparation * (best - worst)
	if margin <= 0 {
		margin = underperformSeparation * math.Abs(worst)
	}
	if worst-score(value) < margin {
		return
	}
	first, err := s.store.MarkUnderperforming(ctx, exp.ID)
	if err != nil {
		s.logger.Warn("registry: mark underperformance", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}
	if !first {
		return
	}

	err = s.events.NotifyEvent(ctx, db.Event{
		Kind:                 db.EventJobUnderperforming,
		Subject:              exp.ID,
		Value:                metricName,
		Detail:               "trailing its cohort on this ranking metric",
		PlatformExperimentID: exp.PlatformExperimentID,
		AgentID:              exp.AgentID,
	})
	if err != nil {
		s.logger.Warn("registry: notify underperformance", zap.String("experiment_id", exp.ID), zap.Error(err))
	}
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
// query (GetTimeseries) only ever returns the latest sample per series. The window is the lookback
// before end; an end that is zero or still ahead means now.
func (s *Service) GetPlatformExperimentTimeseries(ctx context.Context, platformExpID, metricName string, lookback time.Duration, end time.Time) ([]*domain.AgentMetricSeries, error) {
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	if now := time.Now().UTC(); end.IsZero() || end.After(now) {
		end = now
	}
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
	recorded, err := metricsdb.ExperimentMetricSamples(ctx, s.metricsDBURL, experimentID, start, end)
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
