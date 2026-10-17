package platformexperiments

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// RunHealth is where each agent of a platform experiment, and its hosted coordinator, last stood,
// as the run's health series in the metrics store hold it.
type RunHealth struct {
	PlatformExperimentID string               `json:"platform_experiment_id"`
	Agents               []domain.AgentHealth `json:"agents"`
}

// Health reads the latest health values of platform experiment id from the metrics store.
func (s *PlatformExperimentsService) Health(ctx context.Context, id string) (*RunHealth, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	agents, err := metricsdb.LatestRunHealth(ctx, s.metricsDBURL, pe.ID, pe.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Health: %w", err)
	}
	return &RunHealth{PlatformExperimentID: pe.ID, Agents: agents}, nil
}
