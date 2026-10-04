package metricsdb

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The run's health series, one per agent, labelled platform_experiment_id, agent_id and role. A
// run's total is a sum over its agents (sum by (platform_experiment_id)).
const (
	agentFirstJobSecondsMetric          = "agent_first_job_seconds"
	agentFirstCompletedJobSecondsMetric = "agent_first_completed_job_seconds"
	agentJobsInFlightMetric             = "agent_jobs_in_flight"
	agentRestartsMetric                 = "agent_restarts"
	agentCoordinatorMessagesMetric      = "agent_coordinator_messages"
)

// RecordRunHealth writes where each agent of a platform experiment stands at at, in one request.
// A first-job series starts once there is a first job.
func RecordRunHealth(ctx context.Context, dbURL, platformExpID string, at time.Time, agents []domain.AgentHealth) error {
	var samples []GaugeSample
	for _, a := range agents {
		labels := map[string]string{"platform_experiment_id": platformExpID, "agent_id": a.AgentID, "role": string(a.Role)}
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
