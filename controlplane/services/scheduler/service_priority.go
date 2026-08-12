package scheduler

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// RePrioritize re-evaluates and persists priority scores for all QUEUED experiments.
// It is intended to be called periodically (e.g. every minute).
func (s *Service) RePrioritize(ctx context.Context) error {
	queued, err := s.store.ListExperiments(ctx, domain.ExperimentFilter{Status: domain.StatusQueued})
	if err != nil {
		return fmt.Errorf("scheduler: list queued for reprioritize: %w", err)
	}

	activeExps, err := s.store.GetRunningAndQueued(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: get active experiments: %w", err)
	}

	for _, exp := range queued {
		noveltyScore, err := s.novelty.ComputeNovelty(ctx, exp.HypothesisID, activeExps)
		if err != nil {
			return fmt.Errorf("scheduler: compute novelty for %s: %w", exp.ID, err)
		}
		score, err := s.computePriority(ctx, exp, noveltyScore)
		if err != nil {
			return fmt.Errorf("scheduler: compute priority for %s: %w", exp.ID, err)
		}
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, score); err != nil {
			return fmt.Errorf("scheduler: update priority for %s: %w", exp.ID, err)
		}
	}
	return nil
}

// computePriority calculates the weighted priority score for an experiment.
//
// Priority = w1*novelty + w3*costEfficiency
//
// Components:
//   - novelty:        provided by the caller (already computed against active experiments)
//   - costEfficiency: 1 / (1 + dominantCostFraction), favours cheaper experiments
//
// costEfficiency used to be 1/(1+EstimatedCostAccH), so a CPU-only job (always 0 accelerator
// cost) got a maximal score regardless of its real CPU/RAM/storage footprint.
// domain.AgentQuota.DominantCostFraction fixes this by expressing "how big is this job" as a
// dimensionless fraction of the agent's own guaranteed budget, comparable across resource types.
// Falls back to 0 if no quota row is found yet or exp has no PlatformExperimentID.
//
// Note: SchedulingWeights has no W2/abuse-penalty field — abuse is handled by the controller's
// eviction guards, not by suppressing admission priority.
func (s *Service) computePriority(ctx context.Context, exp *domain.Experiment, novelty float64) (float64, error) {
	w := s.weights

	costFraction := 0.0
	if exp.PlatformExperimentID != "" {
		if aq, err := s.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID); err == nil && aq != nil {
			costFraction = aq.DominantCostFraction(exp)
		}
	}
	costEfficiency := 1.0 / (1.0 + costFraction)

	score := w.W1Novelty*novelty +
		w.W3CostEfficiency*costEfficiency

	return score, nil
}
