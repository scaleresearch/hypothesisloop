package metricsdb

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// The run's health series, one per agent, labelled platform_experiment_id, agent_id and role, and
// one for a hosted coordinator, of role coordinator with no agent_id. A run's total is a sum over
// them (sum by (platform_experiment_id)).
const (
	agentFirstJobSecondsMetric          = "agent_first_job_seconds"
	agentFirstCompletedJobSecondsMetric = "agent_first_completed_job_seconds"
	agentJobsInFlightMetric             = "agent_jobs_in_flight"
	agentRestartsMetric                 = "agent_restarts"
	agentCoordinatorMessagesMetric      = "agent_coordinator_messages"
	// The harness series are written from the launchers' reports (RecordHarnessState) and carry
	// the same labels, the rate-limit ones once a harness has read a rate limit.
	agentStateReportedAtMetric    = "agent_state_reported_at_seconds"
	agentSubagentsMetric          = "agent_subagents"
	agentTurnWaitingMetric        = "agent_turn_waiting"
	agentTurnSinceMetric          = "agent_turn_since_seconds"
	agentInstructionsDigestMetric = "agent_instructions_digest"
	// agentInstructionsVersionMetric is the version of its agent type the harness runs, labelled
	// agent_type besides: the history of which version each agent ran.
	agentInstructionsVersionMetric  = "agent_instructions_version"
	agentRateLimitUtilizationMetric = "agent_rate_limit_utilization"
	agentRateLimitRejectedMetric    = "agent_rate_limit_rejected"
	agentRateLimitResetsAtMetric    = "agent_rate_limit_resets_at_seconds"
)

var rateLimitMetrics = []string{agentRateLimitUtilizationMetric, agentRateLimitRejectedMetric, agentRateLimitResetsAtMetric}

func healthLabels(platformExperimentID, agentID, role string) map[string]string {
	labels := map[string]string{"platform_experiment_id": platformExperimentID, "role": role}
	if agentID != "" {
		labels["agent_id"] = agentID
	}
	return labels
}

// RecordRunHealth writes where each agent stands at at, in one request. A first-job series
// starts once there is a first job.
func (s *Store) RecordRunHealth(ctx context.Context, at time.Time, agents []domain.AgentHealth) error {
	var samples []GaugeSample
	for _, a := range agents {
		labels := healthLabels(a.PlatformExperimentID, a.AgentID, a.Role)
		gauge := func(name string, value float64) {
			samples = append(samples, GaugeSample{MetricName: name, Labels: labels, Value: value, At: at})
		}
		if a.FirstJobSeconds != nil {
			gauge(agentFirstJobSecondsMetric, *a.FirstJobSeconds)
		}
		if a.FirstCompletedSeconds != nil {
			gauge(agentFirstCompletedJobSecondsMetric, *a.FirstCompletedSeconds)
		}
		gauge(agentJobsInFlightMetric, float64(a.JobsInFlight))
		gauge(agentRestartsMetric, float64(a.Restarts))
		gauge(agentCoordinatorMessagesMetric, float64(a.CoordinatorMessages))
	}
	if err := s.WriteGaugesAt(ctx, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordRunHealth: %w", err)
	}
	return nil
}

// RecordHarnessState writes what each hosted harness last said of itself, read at at, in one
// request: its subagents, its turn, and the rate limit when it has read one.
func (s *Store) RecordHarnessState(ctx context.Context, at time.Time, states []domain.HarnessState) error {
	var samples []GaugeSample
	for _, s := range states {
		labels := healthLabels(s.PlatformExperimentID, s.AgentID, s.Role)
		gauge := func(name string, value float64) {
			samples = append(samples, GaugeSample{MetricName: name, Labels: labels, Value: value, At: at})
		}
		gauge(agentStateReportedAtMetric, float64(at.Unix()))
		gauge(agentSubagentsMetric, float64(s.Subagents))
		gauge(agentTurnWaitingMetric, boolGauge(s.TurnWaiting))
		gauge(agentTurnSinceMetric, float64(s.TurnSince.Unix()))
		if s.InstructionsDigest != "" {
			digest, err := strconv.ParseUint(s.InstructionsDigest, 16, 64)
			if err != nil {
				return fmt.Errorf("metricsdb.RecordHarnessState: instructions digest %q: %w", s.InstructionsDigest, err)
			}
			gauge(agentInstructionsDigestMetric, float64(digest))
		}
		if s.AgentTypeVersion > 0 {
			versionLabels := healthLabels(s.PlatformExperimentID, s.AgentID, s.Role)
			versionLabels["agent_type"] = s.AgentType
			samples = append(samples, GaugeSample{MetricName: agentInstructionsVersionMetric, Labels: versionLabels, Value: float64(s.AgentTypeVersion), At: at})
		}
		if s.Model == (domain.RateLimit{}) {
			continue
		}
		gauge(agentRateLimitRejectedMetric, boolGauge(s.Model.Rejected))
		if s.Model.Utilization != nil {
			gauge(agentRateLimitUtilizationMetric, *s.Model.Utilization)
		}
		if s.Model.ResetsAt != nil {
			gauge(agentRateLimitResetsAtMetric, float64(s.Model.ResetsAt.Unix()))
		}
	}
	if err := s.WriteGaugesAt(ctx, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordHarnessState: %w", err)
	}
	return nil
}

type healthSeries struct{ role, agentID string }

// latestHealth is the latest value of each of metrics, per series of platformExperimentID, since
// the run's row came into being at createdAt.
func (s *Store) latestHealth(ctx context.Context, platformExperimentID string, createdAt time.Time, metrics []string) (map[healthSeries]map[string]float64, error) {
	window := promSeconds(time.Since(ObservationWindowStart(createdAt)))
	out := map[healthSeries]map[string]float64{}
	for _, metric := range metrics {
		samples, err := s.QueryVector(ctx, fmt.Sprintf(`last_over_time(%s{platform_experiment_id=%q}[%s])`, metric, platformExperimentID, window))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", metric, err)
		}
		for _, sample := range samples {
			key := healthSeries{sample.Labels["role"], sample.Labels["agent_id"]}
			if out[key] == nil {
				out[key] = map[string]float64{}
			}
			out[key][metric] = sample.Value
		}
	}
	return out, nil
}

func unixGauge(value float64) time.Time {
	return time.Unix(int64(value), 0).UTC()
}

// refusedUntil is when the refusal of the provider a series holds ends, false for none. A refusal
// whose reset has passed is over, whether or not a reading has said so since.
func refusedUntil(values map[string]float64) (time.Time, bool) {
	resets, known := values[agentRateLimitResetsAtMetric]
	if values[agentRateLimitRejectedMetric] != 1 || !known || !unixGauge(resets).After(time.Now()) {
		return time.Time{}, false
	}
	return unixGauge(resets), true
}

// LatestRunHealth reads where each agent of platformExperimentID, and its hosted coordinator,
// last stood: the latest value of each of its health series since the run's row came into being
// at createdAt. A first-job value is nil until there is a first job. Sorted by role, then agent.
func (s *Store) LatestRunHealth(ctx context.Context, platformExperimentID string, createdAt time.Time) ([]domain.AgentHealth, error) {
	series, err := s.latestHealth(ctx, platformExperimentID, createdAt, append([]string{agentFirstJobSecondsMetric, agentFirstCompletedJobSecondsMetric,
		agentJobsInFlightMetric, agentRestartsMetric, agentCoordinatorMessagesMetric}, rateLimitMetrics...))
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LatestRunHealth: %w", err)
	}
	out := make([]domain.AgentHealth, 0, len(series))
	for key, values := range series {
		a := domain.AgentHealth{PlatformExperimentID: platformExperimentID, Role: key.role, AgentID: key.agentID,
			JobsInFlight: int(values[agentJobsInFlightMetric]), Restarts: int(values[agentRestartsMetric]), CoordinatorMessages: int(values[agentCoordinatorMessagesMetric])}
		if value, ok := values[agentFirstJobSecondsMetric]; ok {
			a.FirstJobSeconds = &value
		}
		if value, ok := values[agentFirstCompletedJobSecondsMetric]; ok {
			a.FirstCompletedSeconds = &value
		}
		if value, ok := values[agentRateLimitUtilizationMetric]; ok {
			a.RateLimitUtilization = &value
		}
		if until, ok := refusedUntil(values); ok {
			a.RateLimitedUntil = &until
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b domain.AgentHealth) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.AgentID, b.AgentID))
	})
	return out, nil
}

// LatestHarnessState reads what the harness of each hosted agent of platformExperimentID, and of
// its hosted coordinator, last said of itself since the run's row came into being at createdAt.
// One whose harness said nothing in that time is left out. Model is the rate limit last read,
// Rejected only while its reset is ahead. Sorted by role, then agent.
func (s *Store) LatestHarnessState(ctx context.Context, platformExperimentID string, createdAt time.Time) ([]domain.HarnessState, error) {
	series, err := s.latestHealth(ctx, platformExperimentID, createdAt, append([]string{agentStateReportedAtMetric,
		agentSubagentsMetric, agentTurnWaitingMetric, agentTurnSinceMetric, agentInstructionsDigestMetric}, rateLimitMetrics...))
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LatestHarnessState: %w", err)
	}
	out := make([]domain.HarnessState, 0, len(series))
	for key, values := range series {
		at, reported := values[agentStateReportedAtMetric]
		if !reported {
			continue
		}
		s := domain.HarnessState{PlatformExperimentID: platformExperimentID, Role: key.role, AgentID: key.agentID, At: unixGauge(at),
			Subagents: int(values[agentSubagentsMetric]), TurnWaiting: values[agentTurnWaitingMetric] == 1, TurnSince: unixGauge(values[agentTurnSinceMetric])}
		if value, ok := values[agentInstructionsDigestMetric]; ok {
			s.InstructionsDigest = fmt.Sprintf("%0*x", domain.InstructionsDigestLength, uint64(value))
		}
		if value, ok := values[agentRateLimitUtilizationMetric]; ok {
			s.Model.Utilization = &value
		}
		if value, ok := values[agentRateLimitResetsAtMetric]; ok {
			resets := unixGauge(value)
			s.Model.ResetsAt = &resets
		}
		_, s.Model.Rejected = refusedUntil(values)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b domain.HarnessState) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.AgentID, b.AgentID))
	})
	return out, nil
}
