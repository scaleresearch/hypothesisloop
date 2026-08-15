package controller

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// applyCut commits one boundary: release the incoming stage's share of the budget across
// survivors, zero the cut agents' unspent quota into that release, stop their jobs. stageIndex
// is the 1-based index of the stage that is ending.
func (c *Controller) applyCut(ctx context.Context, pe *domain.PlatformExperiment, stageIndex int, survivors, cut []string, runningExps []*domain.Experiment) error {
	quotas, err := c.stagesStore.ListAgentQuotas(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("stages: list agent quotas: %w", err)
	}
	// Cut agents' *unspent* quota is what returns to the pool, so their usage must be current
	// before the ops are computed.
	if err := metricsdb.PopulateUsage(ctx, c.metricsDBURL, pe.CreatedAt, pe.ID, quotas); err != nil {
		return fmt.Errorf("stages: populate usage: %w", err)
	}
	if err := c.stagesStore.AddDesiredQuotaUsage(ctx, pe.ID, quotas); err != nil {
		return fmt.Errorf("stages: populate desired usage: %w", err)
	}
	quotaByAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		quotaByAgent[q.AgentID] = q
	}

	// The share of the budget withheld for the stage now starting is released here. Only real
	// (guaranteed) hours move — burst is a virtual overcommit limit, not physical compute, so
	// redistributing it would inflate survivors' allocations beyond the actual remaining budget.
	releaseFrac := pe.Stages[stageIndex].LengthPct / 100.0

	// Collect every dimension's ops and apply them in one transaction (AdvanceStage) instead of
	// four independently-committing loops. Accelerator-hours drives progress itself and always
	// moves; CPU/RAM/storage move too for any platform experiment that tracks them (0 budget =
	// skipped, the same "not tracked" convention as elsewhere).
	var zeros []db.StageZeroOp
	var adds []db.StageAddOp
	collectRedistribution(&zeros, &adds, cut, survivors, quotaByAgent, releaseFrac,
		domain.ResourceAcceleratorHours, pe.BudgetAcceleratorHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedAcceleratorHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedAccH },
	)
	collectRedistribution(&zeros, &adds, cut, survivors, quotaByAgent, releaseFrac,
		domain.ResourceCPUCoreHours, pe.BudgetCPUCoreHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedCPUCoreHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedCPUCoreH },
	)
	collectRedistribution(&zeros, &adds, cut, survivors, quotaByAgent, releaseFrac,
		domain.ResourceRAMGBHours, pe.BudgetRAMGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedRAMGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedRAMGBH },
	)
	collectRedistribution(&zeros, &adds, cut, survivors, quotaByAgent, releaseFrac,
		domain.ResourceStorageGBHours, pe.BudgetStorageGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedStorageGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedStorageGBH },
	)

	advanced, err := c.stagesStore.AdvanceStage(ctx, pe.ID, stageIndex, cut, zeros, adds)
	if err != nil {
		return fmt.Errorf("stages: advance stage %d: %w", stageIndex, err)
	}
	if !advanced {
		// Another replica committed this boundary first — its cut agents are already durable,
		// and the next reconcile tick stops their jobs. Nothing to do.
		return nil
	}
	c.logger.Info("stage advanced",
		zap.String("platform_experiment", pe.ID),
		zap.Int("stage_ended", stageIndex),
		zap.Strings("survivors", survivors),
		zap.Strings("cut", cut),
		zap.Float64("released_fraction", releaseFrac),
	)

	for _, agentID := range cut {
		if err := c.stopCutAgentJobs(ctx, agentID, pe.ID, runningExps); err != nil {
			c.logger.Error("stages: stop cut agent jobs", zap.String("agent", agentID), zap.Error(err))
		}
	}
	return nil
}

// stopCutAgentJobs terminates all non-terminal jobs for a cut agent. Idempotent — every
// transition is guarded by its from-status, so a retry after a crash is a no-op.
func (c *Controller) stopCutAgentJobs(ctx context.Context, agentID, platformExpID string, runningExps []*domain.Experiment) error {
	running, err := c.stagesStore.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get running: %w", err)
	}
	for _, exp := range running {
		updated, err := c.stagesStore.TransitionTerminal(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionStageCut))
		if err != nil {
			c.logger.Error("stage cut: evict running", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionStageCut)).Inc()
		// Status is already EVICTED above — the cluster-agent's next reconcile pass removes the
		// Job on its own. A stage cut still owes the researcher whatever they genuinely ran, same
		// as every other eviction path — settleAndMark computes that from observed metrics, not
		// the unused remainder.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionStageCut)
		c.settleAndMark(ctx, exp)
		c.logger.Info("stage cut: stopped running job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	preRun, err := c.stagesStore.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get queued: %w", err)
	}
	for _, exp := range preRun {
		updated, err := c.stagesStore.TransitionTerminal(ctx, exp.ID, exp.Status, domain.StatusRejected,
			string(domain.EvictionStageCut))
		if err != nil {
			c.logger.Error("stage cut: reject queued", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionStageCut)).Inc()
		// Never reached RUNNING; settlement derives zero usage from absent metrics.
		exp.Status = domain.StatusRejected
		exp.EvictionReason = string(domain.EvictionStageCut)
		c.settleAndMark(ctx, exp)
		c.logger.Info("stage cut: cancelled pre-run job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

// collectRedistribution computes one resource dimension's cut-agent zero ops and survivor add ops
// and appends them to zeros/adds — it performs no writes itself, so every dimension's ops across
// the whole boundary can be applied together in one transaction (see AdvanceStage). No-op if
// budget is 0 (platform experiment doesn't track this dimension).
func collectRedistribution(
	zeros *[]db.StageZeroOp,
	adds *[]db.StageAddOp,
	cut, survivors []string,
	quotaByAgent map[string]*domain.AgentQuota,
	releaseFrac float64,
	resourceType domain.ResourceType,
	budget float64,
	guaranteedOf, usedOf func(*domain.AgentQuota) float64,
) {
	if budget <= 0 {
		return
	}
	release := budget * releaseFrac
	for _, agentID := range cut {
		if q, ok := quotaByAgent[agentID]; ok {
			if rem := guaranteedOf(q) - usedOf(q); rem > 0 {
				release += rem
			}
		}
		*zeros = append(*zeros, db.StageZeroOp{AgentID: agentID, ResourceType: resourceType})
	}

	if len(survivors) == 0 || release <= 0 {
		return
	}
	perAgent := release / float64(len(survivors))
	for _, agentID := range survivors {
		*adds = append(*adds, db.StageAddOp{AgentID: agentID, ResourceType: resourceType, Delta: perAgent})
	}
}
