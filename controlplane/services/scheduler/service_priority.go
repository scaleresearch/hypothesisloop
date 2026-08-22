package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

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
		// A per-job failure here (novelty lookup, quota lookup, or the DB write) must not abort
		// reprioritization of the rest of the queue: an unrelated job's transient error would
		// otherwise leave every job after it with a stale score for the rest of this tick. Log
		// and skip just this job instead, mirroring completionFractions' per-item skip in
		// loop_preempt.go.
		noveltyScore, err := s.novelty.ComputeNovelty(ctx, exp.HypothesisID, excludeExperiment(activeExps, exp.ID))
		if err != nil {
			s.logger.Warn("reprioritize: compute novelty failed; leaving stale priority",
				zap.String("exp", exp.ID), zap.Error(err))
			continue
		}
		score, err := s.computePriority(ctx, exp, noveltyScore)
		if err != nil {
			s.logger.Warn("reprioritize: compute priority failed; leaving stale priority",
				zap.String("exp", exp.ID), zap.Error(err))
			continue
		}
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, score); err != nil {
			s.logger.Warn("reprioritize: persist priority failed; leaving stale priority",
				zap.String("exp", exp.ID), zap.Error(err))
			continue
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
// Falls back to 0 only if no quota row is found yet (aq == nil) or exp has no
// PlatformExperimentID — both mean "this job doesn't have a quota footprint yet", which is a
// legitimate 0. A GetAgentQuota *error* is different: it means the cost is unknown, not zero, so
// it must not be treated as "this job is free" (that would hand a transient lookup failure
// maximal scheduling priority). Such errors are returned to the caller, which skips updating
// this job's score for the tick rather than scoring it as costless.
//
// Note: SchedulingWeights has no W2/abuse-penalty field — abuse is handled by the controller's
// eviction guards, not by suppressing admission priority.
func (s *Service) computePriority(ctx context.Context, exp *domain.Experiment, novelty float64) (float64, error) {
	w := s.weights

	costFraction := 0.0
	if exp.PlatformExperimentID != "" {
		aq, err := s.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil {
			return 0, fmt.Errorf("scheduler: get agent quota for %s: %w", exp.ID, err)
		}
		if aq != nil {
			costFraction = aq.DominantCostFraction(exp)
		}
	}
	costEfficiency := 1.0 / (1.0 + costFraction)

	score := w.W1Novelty*novelty +
		w.W3CostEfficiency*costEfficiency

	return score, nil
}

// excludeExperiment returns exps without the row for id. Novelty is "how unlike everything else
// is this?", so a job compared against a set containing itself scores itself a duplicate.
func excludeExperiment(exps []*domain.Experiment, id string) []*domain.Experiment {
	out := make([]*domain.Experiment, 0, len(exps))
	for _, e := range exps {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}
