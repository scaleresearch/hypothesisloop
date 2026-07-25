package controller

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// applyPhase2Hold stops held agents' jobs, returns their reservations, and redistributes
// their remaining quota equally across active agents.
func (c *Controller) applyPhase2Hold(ctx context.Context, pe *domain.PlatformExperiment, heldAgentIDs, activeAgentIDs []string, runningExps []*domain.Experiment) error {
	// 1. Collect held agents' remaining quota to redistribute — one query for all.
	allQuotas, err := c.phase2Store.ListAgentQuotas(ctx, pe.ID)
	if err != nil {
		c.logger.Error("phase2: list agent quotas", zap.String("pe", pe.ID), zap.Error(err))
		allQuotas = nil
	}
	if err := metricsdb.PopulateUsage(ctx, c.metricsDBURL, pe.ID, allQuotas); err != nil {
		c.logger.Error("phase2: populate usage", zap.String("pe", pe.ID), zap.Error(err))
	}
	if err := c.phase2Store.AddDesiredQuotaUsage(ctx, pe.ID, allQuotas); err != nil {
		c.logger.Error("phase2: populate desired usage", zap.String("pe", pe.ID), zap.Error(err))
	}
	quotaByAgent := make(map[string]*domain.AgentQuota, len(allQuotas))
	for _, q := range allQuotas {
		q := q
		quotaByAgent[q.AgentID] = q
	}

	// The 60% of the budget withheld at experiment start is released to active agents here.
	// Only real (guaranteed) hours are redistributed — burst is a virtual overcommit limit,
	// not physical compute. Adding it would inflate active agents' allocations beyond the
	// actual remaining budget. Accelerator-hours drives the phase-2 trigger itself and always
	// redistributes; CPU/RAM/storage redistribute too, for any platform experiment that
	// also tracks them (0 budget = skipped, same "not tracked" convention as elsewhere).
	boundary := c.phase2BoundaryFrac
	if boundary <= 0 {
		boundary = phase2BoundaryFraction
	}

	// Collect every dimension's ops and apply them in one transaction (RedistributePhase2Quota)
	// instead of four independently-committing loops.
	var zeros []db.Phase2ZeroOp
	var adds []db.Phase2AddOp
	c.collectRedistribution(&zeros, &adds, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceAcceleratorHours, pe.BudgetAcceleratorHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedAcceleratorHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedAccH },
	)
	c.collectRedistribution(&zeros, &adds, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceCPUCoreHours, pe.BudgetCPUCoreHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedCPUCoreHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedCPUCoreH },
	)
	c.collectRedistribution(&zeros, &adds, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceRAMGBHours, pe.BudgetRAMGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedRAMGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedRAMGBH },
	)
	c.collectRedistribution(&zeros, &adds, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceStorageGBHours, pe.BudgetStorageGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedStorageGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedStorageGBH },
	)

	if len(zeros) > 0 || len(adds) > 0 {
		applied, err := c.phase2Store.RedistributePhase2Quota(ctx, pe.ID, zeros, adds)
		if err != nil {
			c.logger.Error("phase2: redistribute quota", zap.String("pe", pe.ID), zap.Error(err))
		} else if applied {
			c.logger.Info("phase2: quota redistributed",
				zap.String("pe", pe.ID),
				zap.Int("zero_ops", len(zeros)),
				zap.Int("add_ops", len(adds)),
			)
		}
		// !applied (no error) means an earlier call already committed this platform experiment's
		// redistribution — expected on every retry via reconcilePhase2Hold, not an error.
	}

	// 2. Stop running jobs for held agents (once, not per-dimension).
	for _, agentID := range heldAgentIDs {
		if err := c.stopHeldAgentJobs(ctx, agentID, pe.ID, runningExps); err != nil {
			c.logger.Error("phase2: stop jobs", zap.String("agent", agentID), zap.Error(err))
		}
	}

	return nil
}

// stopHeldAgentJobs terminates all non-terminal jobs for a held agent.
func (c *Controller) stopHeldAgentJobs(ctx context.Context, agentID, platformExpID string, runningExps []*domain.Experiment) error {
	// Stop running jobs (refund unused AccH).
	running, err := c.phase2Store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get running: %w", err)
	}
	for _, exp := range running {
		updated, err := c.phase2Store.TransitionTerminal(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionPhase2Hold))
		if err != nil {
			c.logger.Error("phase2 hold: evict running", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionPhase2Hold)).Inc()
		// Status is already EVICTED above — the cluster-agent's next reconcile pass removes
		// the Job on its own. A phase2 hold still owes the researcher whatever they genuinely
		// ran, same as every other eviction path — settleAndMark computes that from observed
		// metrics, not the unused remainder.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionPhase2Hold)
		c.settleAndMark(ctx, exp)
		c.logger.Info("phase2 hold: stopped running job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	// Cancel queued/submitted jobs (return reservations).
	preRun, err := c.phase2Store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get queued: %w", err)
	}
	for _, exp := range preRun {
		prevStatus := exp.Status
		updated, err := c.phase2Store.TransitionTerminal(ctx, exp.ID, prevStatus, domain.StatusRejected,
			string(domain.EvictionPhase2Hold))
		if err != nil {
			c.logger.Error("phase2 hold: reject queued", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionPhase2Hold)).Inc()
		// Status is already updated above (away from QUEUED/SUBMITTED) — if a Job existed
		// for this experiment, it disappears on the cluster-agent's next reconcile pass.
		// Never reached RUNNING; settlement derives zero usage from absent metrics.
		exp.Status = domain.StatusRejected
		exp.EvictionReason = string(domain.EvictionPhase2Hold)
		c.settleAndMark(ctx, exp)
		c.logger.Info("phase2 hold: cancelled pre-run job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

// collectRedistribution computes one resource dimension's held-agent zero ops and active-agent
// add ops and appends them to zeros/adds — it performs no writes itself, so every dimension's ops
// across the whole platform experiment can be applied together in one transaction (see
// RedistributePhase2Quota). No-op if budget is 0 (platform experiment doesn't track this
// dimension).
func (c *Controller) collectRedistribution(
	zeros *[]db.Phase2ZeroOp,
	adds *[]db.Phase2AddOp,
	pe *domain.PlatformExperiment,
	heldAgentIDs, activeAgentIDs []string,
	quotaByAgent map[string]*domain.AgentQuota,
	boundary float64,
	resourceType domain.ResourceType,
	budget float64,
	guaranteedOf, usedOf func(*domain.AgentQuota) float64,
) {
	if budget <= 0 {
		return
	}
	totalRemaining := budget * (1.0 - boundary)
	for _, agentID := range heldAgentIDs {
		if q, ok := quotaByAgent[agentID]; ok {
			if rem := guaranteedOf(q) - usedOf(q); rem > 0 {
				totalRemaining += rem
			}
		}
		*zeros = append(*zeros, db.Phase2ZeroOp{AgentID: agentID, ResourceType: resourceType})
	}

	if len(activeAgentIDs) == 0 || totalRemaining <= 0 {
		return
	}
	perAgent := totalRemaining / float64(len(activeAgentIDs))
	sort.Strings(activeAgentIDs) // deterministic order
	for _, agentID := range activeAgentIDs {
		*adds = append(*adds, db.Phase2AddOp{AgentID: agentID, ResourceType: resourceType, Delta: perAgent})
	}
}
