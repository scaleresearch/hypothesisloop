package quota

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// GetQuota returns an agent's allocation and current usage for a platform experiment, merging
// the allocation (Postgres) with consumption (metrics DB) into one struct.
func (s *PlatformExperimentsService) GetQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: populate usage: %w", err)
	}
	return aq, nil
}

// GetAgentQuota is an alias for GetQuota satisfying the controller.QuotaService interface.
func (s *PlatformExperimentsService) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	return s.GetQuota(ctx, agentID, platformExpID)
}

// ListQuotas returns every agent's allocation and current usage for a platform experiment,
// merging the allocation (Postgres) with consumption (metrics DB) in one batched usage query.
func (s *PlatformExperimentsService) ListQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	quotas, err := s.store.ListAgentQuotas(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: %w", err)
	}
	if err := metricsdb.PopulateUsage(ctx, s.usage.URL(), platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: populate usage: %w", err)
	}
	return quotas, nil
}

// allocationFor returns the (guaranteed, burst) allocation limit for resourceType on aq — the
// Postgres-side half of the check; usage (the other half) lives in the metrics DB and is checked
// by UsageTracker.CheckAndDebit itself.
func allocationFor(aq *domain.AgentQuota, resourceType domain.ResourceType, tier domain.CapacityTier) float64 {
	guaranteed := tier == domain.CapacityGuaranteed
	switch resourceType {
	case domain.ResourceCPUCoreHours:
		if guaranteed {
			return aq.GuaranteedCPUCoreHours
		}
		return aq.BurstCPUCoreHours
	case domain.ResourceRAMGBHours:
		if guaranteed {
			return aq.GuaranteedRAMGBHours
		}
		return aq.BurstRAMGBHours
	case domain.ResourceStorageGBHours:
		if guaranteed {
			return aq.GuaranteedStorageGBHours
		}
		return aq.BurstStorageGBHours
	default: // domain.ResourceGPUHours
		if guaranteed {
			return aq.GuaranteedT4Hours
		}
		return aq.BurstT4Hours
	}
}

// CheckAndDebitQuota validates that the agent has sufficient quota in resourceType's pool and
// debits it. amount is 0 for any dimension the platform experiment doesn't track — a no-op
// debit that only ever succeeds (0 <= 0 remaining), so untracked dimensions never block
// admission.
//
// The read-then-reserve step (read current used, compare to limit, write the reservation) runs
// inside WithAdmissionLock so two control-service replicas admitting against the same agent's
// quota at once can't both observe the same remaining balance and jointly overspend it —
// metricsdb.UsageTracker's own mutex only ever serialized within one process
func (s *PlatformExperimentsService) CheckAndDebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	if amount <= 0 {
		return nil
	}

	return s.store.WithAdmissionLock(ctx, agentID, platformExpID, func(ctx context.Context) error {
		// Block held agents from submitting new jobs (Domain 10). Checked inside the same
		// advisory lock as the debit below — checking it before acquiring the lock left a window
		// where a hold inserted concurrently (between the check and the lock) would not be seen,
		// letting a job slip in for an agent that should already be held.
		held, err := s.store.IsAgentHeld(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("phase2 hold check: %w", err)
		}
		if held {
			return fmt.Errorf("agent_phase2_held: agent %s is on hold for experiment %s", agentID, platformExpID)
		}

		aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
		if err != nil {
			return err
		}
		if aq == nil {
			return fmt.Errorf("insufficient_quota: no quota found (agent not signed up?)")
		}

		limit := allocationFor(aq, resourceType, tier)
		return s.usage.CheckAndDebit(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount, limit)
	})
}

// RefundQuota overwrites experimentID's own resourceType usage with its observed cost (an
// absolute set, not a delta — see UsageTracker.SetObserved). Despite the name (kept for callers
// that still think of this as "the refund step"), amount here means the job's final observed
// cost for this dimension, computed by the caller from confirmed-alive time.
//
// Runs under the same advisory lock as CheckAndDebitQuota: CheckAndDebitQuota's balance check
// sums every job's series for (agent, PE, resource, tier) and only the lock's own critical
// section stops that sum from racing another write to a sibling job's series — the per-series
// writes here are otherwise independent of each other but not of that sum.
func (s *PlatformExperimentsService) RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return s.store.WithAdmissionLock(ctx, agentID, platformExpID, func(ctx context.Context) error {
		return s.usage.SetObserved(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount)
	})
}

// CorrectReservation overwrites experimentID's own resourceType reservation with a new absolute
// amount (see metricsdb.UsageTracker.SetReservation) — no balance check, since this corrects an
// existing reservation to match a revised (e.g. shortened, after preemption) estimate rather
// than reserving new capacity. Locked for the same reason as RefundQuota above — called from
// preempt() while other agents/experiments may be concurrently admitting against the same
// agent+PE's aggregate.
func (s *PlatformExperimentsService) CorrectReservation(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return s.store.WithAdmissionLock(ctx, agentID, platformExpID, func(ctx context.Context) error {
		return s.usage.SetReservation(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount)
	})
}

// DebitQuota adds amount to experimentID's own resourceType usage without a balance check.
// Used for system-level adjustments such as flavor substitution cost corrections
// (e.g. H100 job admitted on H200 — agent owes the rate difference). Locked for the same reason
// as RefundQuota/CorrectReservation above.
func (s *PlatformExperimentsService) DebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return s.store.WithAdmissionLock(ctx, agentID, platformExpID, func(ctx context.Context) error {
		return s.usage.Debit(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount)
	})
}
