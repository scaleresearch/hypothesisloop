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
	// What returns to the survivors is what a cut agent did NOT spend, and only observed
	// consumption can answer that (#18). The two calls above leave a cut agent's in-flight jobs
	// counted at their full admission estimate, so an agent cut while holding a job estimated at
	// 100 AccH that has really burned 10 looks 90 AccH poorer than it is — and those 90 AccH
	// reach nobody: this agent's row is zeroed moments later, and the eviction that follows
	// settles the job at its real 10. Re-read each cut agent against observed usage alone.
	for _, agentID := range cut {
		observed, err := c.quota.GetObservedAgentQuota(ctx, agentID, pe.ID)
		if err != nil {
			return fmt.Errorf("stages: observed quota for cut agent %s: %w", agentID, err)
		}
		quotaByAgent[agentID] = observed
	}

	// The share of the budget withheld for the stage now starting is released here. Only real
	// (guaranteed) hours move — burst is a virtual overcommit limit, not physical compute, so
	// redistributing it would inflate survivors' allocations beyond the actual remaining budget.
	releaseFrac := pe.Stages[stageIndex].LengthPct / 100.0

	// Accelerator-hours drives progress itself and always moves; CPU/RAM/storage move too for any
	// platform experiment that tracks them (0 budget = skipped, the same "not tracked" convention
	// as elsewhere). Only observed usage travels into the transaction — the allocation each
	// unspent figure is measured against is read there, under lock.
	dims := []db.StageRedistribution{
		{ResourceType: domain.ResourceAcceleratorHours, Budget: pe.BudgetAcceleratorHours, ReleaseFrac: releaseFrac,
			UsedByAgent: usedByCutAgent(cut, quotaByAgent, func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedAccH })},
		{ResourceType: domain.ResourceCPUCoreHours, Budget: pe.BudgetCPUCoreHours, ReleaseFrac: releaseFrac,
			UsedByAgent: usedByCutAgent(cut, quotaByAgent, func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedCPUCoreH })},
		{ResourceType: domain.ResourceRAMGBHours, Budget: pe.BudgetRAMGBHours, ReleaseFrac: releaseFrac,
			UsedByAgent: usedByCutAgent(cut, quotaByAgent, func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedRAMGBH })},
		{ResourceType: domain.ResourceStorageGBHours, Budget: pe.BudgetStorageGBHours, ReleaseFrac: releaseFrac,
			UsedByAgent: usedByCutAgent(cut, quotaByAgent, func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedStorageGBH })},
	}

	advanced, err := c.stagesStore.AdvanceStage(ctx, pe.ID, stageIndex, cut, survivors, dims)
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
		// ADMITTED already has a workload being created for it, so it must go to EVICTED (the
		// desired-state pull's deletion signal) — REJECTED is only correct for QUEUED/SUBMITTED,
		// which never had a workload to tear down. See CancelExperiment for the same split.
		to := domain.StatusRejected
		if exp.Status == domain.StatusAdmitted {
			to = domain.StatusEvicted
		}
		updated, err := c.stagesStore.TransitionTerminal(ctx, exp.ID, exp.Status, to,
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
		exp.Status = to
		exp.EvictionReason = string(domain.EvictionStageCut)
		c.settleAndMark(ctx, exp)
		c.logger.Info("stage cut: cancelled pre-run job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

func usedByCutAgent(cut []string, quotaByAgent map[string]*domain.AgentQuota, usedOf func(*domain.AgentQuota) float64) map[string]float64 {
	used := make(map[string]float64, len(cut))
	for _, agentID := range cut {
		if q, ok := quotaByAgent[agentID]; ok {
			used[agentID] = usedOf(q)
		}
	}
	return used
}
