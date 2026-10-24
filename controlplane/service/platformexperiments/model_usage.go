package platformexperiments

import (
	"context"
	"fmt"
	"math"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// ModelUsage is the model calls the platform made for one platform experiment's agents.
type ModelUsage struct {
	PlatformExperimentID string `json:"platform_experiment_id"`
	// Usage is one entry per agent and model, the coordinator's under agent_id coordinator.
	Usage []metricsdb.AgentModelUsage `json:"usage"`
}

// ModelUsage sums the model calls made for the platform experiment id.
func (s *Service) ModelUsage(ctx context.Context, id string) (*ModelUsage, error) {
	calls, err := s.metrics.ModelUsageOf(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ModelUsage{PlatformExperimentID: id, Usage: calls}, nil
}

// ModelBudget is what the platform experiment id may spend on the model calls the platform makes
// for it and what they cost so far.
func (s *Service) ModelBudget(ctx context.Context, id string) (domain.ModelBudget, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return domain.ModelBudget{}, fmt.Errorf("platform_experiments.ModelBudget: %w", err)
	}
	if pe == nil {
		return domain.ModelBudget{}, ErrPlatformExperimentNotFound
	}
	return s.modelBudgetOf(ctx, pe)
}

func (s *Service) modelBudgetOf(ctx context.Context, pe *domain.PlatformExperiment) (domain.ModelBudget, error) {
	calls, err := s.metrics.ModelUsageOf(ctx, pe.ID)
	if err != nil {
		return domain.ModelBudget{}, fmt.Errorf("platform_experiments.ModelBudget: %w", err)
	}
	var usd float64
	for _, c := range calls {
		usd += c.CostUSD
	}
	return domain.ModelBudget{BudgetUSD: domain.ModelBudgetUSD(pe.ModelBudgetUSD, s.cfg.DefaultModelBudgetUSD), UsedUSD: math.Round(usd*100) / 100}, nil
}
