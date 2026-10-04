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
	queued, err := s.store.ListExperiments(ctx, domain.ExperimentFilter{Scope: domain.AllTenants, Status: domain.StatusQueued})
	if err != nil {
		return fmt.Errorf("scheduler: list queued for reprioritize: %w", err)
	}

	// Read once per platform experiment with a queued job, for every job of it in this pass, and
	// the quota once per agent of it.
	active := map[string][]*domain.Experiment{}
	quotas := map[string]*domain.AgentQuota{}
	for _, exp := range queued {
		activeExps, read := active[exp.PlatformExperimentID]
		if !read {
			activeExps, err = s.store.GetRunningAndQueued(ctx, exp.PlatformExperimentID)
			if err != nil {
				s.logger.Warn("reprioritize: read the platform experiment's active jobs failed; leaving stale priority",
					zap.String("exp", exp.ID), zap.Error(err))
				continue
			}
			active[exp.PlatformExperimentID] = activeExps
		}
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
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		aq, read := quotas[key]
		if !read {
			if aq, err = s.agentQuota(ctx, exp); err != nil {
				s.logger.Warn("reprioritize: compute priority failed; leaving stale priority",
					zap.String("exp", exp.ID), zap.Error(err))
				continue
			}
			quotas[key] = aq
		}
		score := s.priority(exp, noveltyScore, aq)
		if score == exp.PriorityScore {
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
	aq, err := s.agentQuota(ctx, exp)
	if err != nil {
		return 0, err
	}
	return s.priority(exp, novelty, aq), nil
}

// agentQuota is the quota computePriority scores exp against, nil when it has none.
func (s *Service) agentQuota(ctx context.Context, exp *domain.Experiment) (*domain.AgentQuota, error) {
	if exp.PlatformExperimentID == "" {
		return nil, nil
	}
	aq, err := s.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("scheduler: get agent quota for %s: %w", exp.ID, err)
	}
	return aq, nil
}

func (s *Service) priority(exp *domain.Experiment, novelty float64, aq *domain.AgentQuota) float64 {
	w := s.weights

	costFraction := 0.0
	if aq != nil {
		costFraction = aq.DominantCostFraction(exp)
	}
	costEfficiency := 1.0 / (1.0 + costFraction)

	return w.W1Novelty*novelty + w.W3CostEfficiency*costEfficiency
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
