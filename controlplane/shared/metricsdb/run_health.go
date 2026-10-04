package metricsdb

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	// The rate-limit series are written from the launchers' reports (RecordRateLimits) and carry
	// the same labels.
	agentRateLimitUtilizationMetric = "agent_rate_limit_utilization"
	agentRateLimitRejectedMetric    = "agent_rate_limit_rejected"
	agentRateLimitResetsAtMetric    = "agent_rate_limit_resets_at_seconds"
)

func healthLabels(platformExperimentID, agentID, role string) map[string]string {
	labels := map[string]string{"platform_experiment_id": platformExperimentID, "role": role}
	if agentID != "" {
		labels["agent_id"] = agentID
	}
	return labels
}

// RecordRunHealth writes where each agent stands at at, in one request. A first-job series
// starts once there is a first job.
func RecordRunHealth(ctx context.Context, dbURL string, at time.Time, agents []domain.AgentHealth) error {
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
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordRunHealth: %w", err)
	}
	return nil
}

// RecordRateLimits writes the rate limit each hosted agent's harness last read, at at, in one
// request: whether the provider refuses its calls, and the utilization and reset time a reading
// holds.
func RecordRateLimits(ctx context.Context, dbURL string, at time.Time, readings []domain.RateLimitReading) error {
	var samples []GaugeSample
	for _, r := range readings {
		labels := healthLabels(r.PlatformExperimentID, r.AgentID, r.Role)
		gauge := func(name string, value float64) {
			samples = append(samples, GaugeSample{MetricName: name, Labels: labels, Value: value, At: at})
		}
		rejected := 0.0
		if r.Rejected {
			rejected = 1
		}
		gauge(agentRateLimitRejectedMetric, rejected)
		if r.Utilization != nil {
			gauge(agentRateLimitUtilizationMetric, *r.Utilization)
		}
		if r.ResetsAt != nil {
			gauge(agentRateLimitResetsAtMetric, float64(r.ResetsAt.Unix()))
		}
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordRateLimits: %w", err)
	}
	return nil
}

// LatestRunHealth reads where each agent of platformExperimentID, and its hosted coordinator,
// last stood: the latest value of each of its health series since the run's row came into being
// at createdAt. A first-job value is nil until there is a first job. Sorted by role, then agent.
func LatestRunHealth(ctx context.Context, dbURL, platformExperimentID string, createdAt time.Time) ([]domain.AgentHealth, error) {
	window := promSeconds(time.Since(ObservationWindowStart(createdAt)))
	type series struct{ role, agentID string }
	agents := map[series]*domain.AgentHealth{}
	rejected, resetsAt := map[series]bool{}, map[series]time.Time{}
	for _, metric := range []string{agentFirstJobSecondsMetric, agentFirstCompletedJobSecondsMetric, agentJobsInFlightMetric, agentRestartsMetric, agentCoordinatorMessagesMetric,
		agentRateLimitUtilizationMetric, agentRateLimitRejectedMetric, agentRateLimitResetsAtMetric} {
		samples, err := QueryVector(ctx, dbURL, fmt.Sprintf(`last_over_time(%s{platform_experiment_id=%q}[%s])`, metric, platformExperimentID, window))
		if err != nil {
			return nil, fmt.Errorf("metricsdb.LatestRunHealth: %s: %w", metric, err)
		}
		for _, sample := range samples {
			key := series{sample.Labels["role"], sample.Labels["agent_id"]}
			a := agents[key]
			if a == nil {
				a = &domain.AgentHealth{PlatformExperimentID: platformExperimentID, Role: key.role, AgentID: key.agentID}
				agents[key] = a
			}
			switch value := sample.Value; metric {
			case agentFirstJobSecondsMetric:
				a.FirstJobSeconds = &value
			case agentFirstCompletedJobSecondsMetric:
				a.FirstCompletedSeconds = &value
			case agentJobsInFlightMetric:
				a.JobsInFlight = int(value)
			case agentRestartsMetric:
				a.Restarts = int(value)
			case agentCoordinatorMessagesMetric:
				a.CoordinatorMessages = int(value)
			case agentRateLimitUtilizationMetric:
				a.RateLimitUtilization = &value
			case agentRateLimitRejectedMetric:
				rejected[key] = value == 1
			case agentRateLimitResetsAtMetric:
				resetsAt[key] = time.Unix(int64(value), 0).UTC()
			}
		}
	}
	// A refusal whose reset has passed is over, whether or not a reading has said so since.
	for key, a := range agents {
		if until, ok := resetsAt[key]; rejected[key] && ok && until.After(time.Now()) {
			a.RateLimitedUntil = &until
		}
	}
	out := make([]domain.AgentHealth, 0, len(agents))
	for _, a := range agents {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b domain.AgentHealth) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.AgentID, b.AgentID))
	})
	return out, nil
}
