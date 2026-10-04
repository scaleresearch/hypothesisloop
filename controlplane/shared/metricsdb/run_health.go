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
)

// RecordRunHealth writes where each agent stands at at, in one request. A first-job series
// starts once there is a first job.
func RecordRunHealth(ctx context.Context, dbURL string, at time.Time, agents []domain.AgentHealth) error {
	var samples []GaugeSample
	for _, a := range agents {
		labels := map[string]string{"platform_experiment_id": a.PlatformExperimentID, "role": a.Role}
		if a.AgentID != "" {
			labels["agent_id"] = a.AgentID
		}
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

// LatestRunHealth reads where each agent of platformExperimentID, and its hosted coordinator,
// last stood: the latest value of each of its health series since the run's row came into being
// at createdAt. A first-job value is nil until there is a first job. Sorted by role, then agent.
func LatestRunHealth(ctx context.Context, dbURL, platformExperimentID string, createdAt time.Time) ([]domain.AgentHealth, error) {
	window := promSeconds(time.Since(ObservationWindowStart(createdAt)))
	type series struct{ role, agentID string }
	agents := map[series]*domain.AgentHealth{}
	for _, metric := range []string{agentFirstJobSecondsMetric, agentFirstCompletedJobSecondsMetric, agentJobsInFlightMetric, agentRestartsMetric, agentCoordinatorMessagesMetric} {
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
			}
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
