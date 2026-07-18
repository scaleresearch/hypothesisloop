package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
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

// resourceCosts returns (resourceType, estimatedAmount) pairs for every dimension exp actually
// uses — GPU is always included (even 0-cost, checked elsewhere); CPU/RAM/storage only appear
// when non-zero (the submission set them and they were estimated in Submit).
func resourceCosts(exp *domain.Experiment) []struct {
	resourceType domain.ResourceType
	amount       float64
} {
	return []struct {
		resourceType domain.ResourceType
		amount       float64
	}{
		{domain.ResourceGPUHours, exp.EstimatedCostT4H},
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours},
	}
}

// debitAllResources debits every non-zero resource dimension exp uses. If a later dimension's
// debit fails, the earlier ones already committed for this call are rolled back to 0 — the
// reservation is being abandoned entirely, so the job's own series should read "never happened",
// not "refunded" — so a rejected submission never leaves a partial debit behind.
func (s *Service) debitAllResources(ctx context.Context, exp *domain.Experiment) error {
	costs := resourceCosts(exp)
	for i, c := range costs {
		if c.amount <= 0 {
			continue
		}
		if err := s.quota.CheckAndDebitQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, c.resourceType, exp.CapacityTier, c.amount); err != nil {
			for _, prior := range costs[:i] {
				if prior.amount > 0 {
					_ = s.quota.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, prior.resourceType, exp.CapacityTier, 0)
				}
			}
			return err
		}
	}
	return nil
}

// refundAllResources overwrites each non-zero estimated dimension exp uses with its true
// observed cost, not an amount to refund. GPU-hours uses gpuCost directly — an absolute amount
// already computed per accelerator type actually used (see metricsdb.ObservedGPUCost), since a
// single flat rate over observedFraction would mischarge any job that ran on more than one
// accelerator type. CPU/RAM/storage have no per-type rate tiers, so they still use
// observedFraction × estimate. observedFraction=0 (and gpuCost=0) for a job that never ran
// (cancel-while-queued, a failed create).
func (s *Service) refundAllResources(ctx context.Context, exp *domain.Experiment, observedFraction, gpuCost float64) {
	for _, c := range resourceCosts(exp) {
		amount := observedFraction * c.amount
		if c.resourceType == domain.ResourceGPUHours {
			amount = gpuCost
		}
		if amount <= 0 && c.amount <= 0 {
			continue
		}
		_ = s.quota.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, c.resourceType, exp.CapacityTier, amount)
	}
}

// observedFraction returns exp's confirmed-alive time (see metricsdb.ObservedElapsedHours) as a
// fraction of its estimate, and its true GPU cost billed per accelerator type actually used (see
// metricsdb.ObservedGPUCost) — the two figures refundAllResources needs. This is the same
// GreptimeDB query Controller uses for every automatic eviction path — a user cancelling a job
// and the controller evicting it agree on what "how long did this run" and "what did it cost"
// mean, because both ask the same source of truth the same way. No fallback: GreptimeDB is a
// required dependency of this deployment, not an optional one, so a query error is returned to
// the caller rather than papered over.
func (s *Service) observedFraction(ctx context.Context, exp *domain.Experiment) (fraction, gpuCost float64, err error) {
	if exp.EstimatedDurationHours <= 0 {
		return 0, 0, nil
	}
	hours, err := metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, s.observedGapCap, s.observedStep)
	if err != nil {
		return 0, 0, fmt.Errorf("scheduler: observed elapsed hours: %w", err)
	}
	gpuCost, err = metricsdb.ObservedGPUCost(ctx, s.metricsDBURL, exp.ID, exp.GPUCount, time.Now().UTC(), ObservedMaxLookback, s.observedGapCap, s.observedStep)
	if err != nil {
		return 0, 0, fmt.Errorf("scheduler: observed GPU cost: %w", err)
	}
	return hours / exp.EstimatedDurationHours, gpuCost, nil
}

// computePriority calculates the weighted priority score for an experiment.
//
// Priority = w1*novelty + w3*costEfficiency
//
// Components:
//   - novelty:        provided by the caller (already computed against active experiments)
//   - costEfficiency: 1 / (1 + estimatedCost), favours cheaper experiments
//
// Note: SchedulingWeights has no W2/abuse-penalty field today — abuse is handled entirely by
// the controller's eviction guards (crash-loop, silence), not by suppressing admission
// priority. (This doc comment previously described a 3-term w1/w2/w3 formula that no longer
// matches the code — corrected to the actual 2-term one.)
func (s *Service) computePriority(_ context.Context, exp *domain.Experiment, novelty float64) (float64, error) {
	w := s.weights

	// Cost efficiency: cheap experiments get higher scores.
	cost := exp.EstimatedCostT4H
	if cost < 0 {
		cost = 0
	}
	costEfficiency := 1.0 / (1.0 + cost)

	score := w.W1Novelty*novelty +
		w.W3CostEfficiency*costEfficiency

	return score, nil
}
