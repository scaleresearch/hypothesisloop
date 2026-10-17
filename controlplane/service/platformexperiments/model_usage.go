package platformexperiments

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// ModelUsage is the model calls the platform made for one platform experiment's agents.
type ModelUsage struct {
	PlatformExperimentID string `json:"platform_experiment_id"`
	// Usage is one entry per agent and model, the coordinator's under agent_id coordinator.
	Usage []metricsdb.AgentModelUsage `json:"usage"`
}

// ModelUsage sums the model calls made for the platform experiment id.
func (s *PlatformExperimentsService) ModelUsage(ctx context.Context, id string) (*ModelUsage, error) {
	calls, err := s.metrics.ModelUsageOf(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ModelUsage{PlatformExperimentID: id, Usage: calls}, nil
}
