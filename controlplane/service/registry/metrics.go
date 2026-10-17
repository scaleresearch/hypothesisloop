package registry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
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
	// Both become Prometheus labels, so each is held to a short printable value. How many
	// distinct names one job may report is not bounded.
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

	at := time.Now().UTC()
	if err := s.metrics.RecordJobMetric(ctx, metricsdb.JobMetric{
		JobID: experimentID, PlatformExperimentID: exp.PlatformExperimentID, AgentID: exp.AgentID,
		Name: metricName, Basis: basis, Value: value, FractionComplete: fractionComplete, At: at,
	}); err != nil {
		return fmt.Errorf("registry.RecordMetric: %w", err)
	}
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
	// underperformMinRemaining is how much of the run must be left, at the pace of the progress
	// the job itself reported, for the advisory to be sent: cancelling a job that ends before its
	// agent has read the advisory saves nothing, and reading it costs the agent a turn.
	underperformMinRemaining = 2 * time.Minute
)

// checkUnderperformance compares this job's just-recorded ranking-metric sample to its cohort
// (every other job in the same platform experiment reporting the same metric, each taken at the
// progress this job has reached) and, if it is the
// clear straggler with underperformMinRemaining of its run still ahead, marks the job's row. The
// first mark emits job.underperforming to the owning agent: once per job, never a command. See
// domain.EventJobUnderperforming.
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
	if exp.PlatformExperimentID == "" {
		return
	}
	if fractionComplete < underperformMinFraction {
		return
	}
	// A requeued job is not placed, and a sample from the workload it left advises nothing.
	if exp.SubmittedAt == nil {
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

	// Both reads end at the sample's own instant, so they share one grid and join on timestamp.
	lookback := underperformSiblingStaleness + 5*time.Minute
	series, err := s.platformExperimentSeries(ctx, metricsdb.ExperimentMetricValue, exp.PlatformExperimentID, metricName, lookback, at)
	if err != nil {
		s.logger.Warn("registry: underperformance check", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}
	fractions, err := s.platformExperimentSeries(ctx, metricsdb.ExperimentMetricFraction, exp.PlatformExperimentID, metricName, lookback, at)
	if err != nil {
		s.logger.Warn("registry: underperformance check", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}
	// A sibling that completed and broke a constraint ranks nowhere, so it is no cohort to trail.
	// One still running has not been judged. The window is the check's own: a sibling whose last
	// sample is older is left out below whatever it found.
	completed, err := s.store.CompletedJobIDs(ctx, exp.PlatformExperimentID)
	if err != nil {
		s.logger.Warn("registry: underperformance check", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}
	rankable, err := s.metrics.RankableJobs(ctx, &domain.PlatformExperiment{ID: exp.PlatformExperimentID, Metrics: metrics, CreatedAt: at.Add(-lookback)}, completed)
	if err != nil {
		s.logger.Warn("registry: underperformance check", zap.String("experiment_id", exp.ID), zap.Error(err))
		return
	}
	type attempt struct{ job, agent string }
	progress := make(map[attempt]map[int64]float64, len(fractions))
	for _, f := range fractions {
		byTime := make(map[int64]float64, len(f.Points))
		for _, p := range f.Points {
			byTime[p.Timestamp.UnixMilli()] = p.Value
		}
		progress[attempt{f.ExperimentID, f.AgentID}] = byTime
	}

	// The pace is the job's own progress since its earliest sample in the window. Counted from
	// admission it would include the wait for a machine to start, and a job minutes from its end
	// would read as having most of its run ahead. A job with no earlier sample has no pace yet.
	first, from := at, fractionComplete
	for ms, f := range progress[attempt{exp.ID, exp.AgentID}] {
		if t := time.UnixMilli(ms); t.Before(first) {
			first, from = t, f
		}
	}
	if fractionComplete <= from {
		return
	}
	if remaining := time.Duration(float64(at.Sub(first)) * (1 - fractionComplete) / (fractionComplete - from)); remaining < underperformMinRemaining {
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
		if s.ExperimentID == exp.ID || completed[s.ExperimentID] && !rankable[s.ExperimentID] {
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
		// The sibling is measured where this job is now: a metric that climbs all run long would
		// otherwise call every late starter a straggler. One that never came within a checkpoint
		// of this progress inside the window is left out.
		reached, gap := false, underperformCheckpointWidth
		var atSameProgress float64
		for _, p := range s.Points {
			f, ok := progress[attempt{s.ExperimentID, s.AgentID}][p.Timestamp.UnixMilli()]
			if d := math.Abs(f - fractionComplete); ok && d <= gap {
				reached, gap, atSameProgress = true, d, p.Value
			}
		}
		if !reached {
			continue
		}
		siblingScores = append(siblingScores, score(atSameProgress))
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
	if err := s.store.MarkUnderperforming(ctx, exp.ID, metricName); err != nil {
		s.logger.Warn("registry: mark underperformance", zap.String("experiment_id", exp.ID), zap.Error(err))
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
