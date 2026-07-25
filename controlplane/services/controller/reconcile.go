package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// sweepStaleDesiredState flags (log + gauge, never mutates) SUBMITTED/ADMITTED/RUNNING
// experiments with no recent cluster job report — orphaned desired-state that survived an
// extended cluster-agent outage combined with a control-plane bug/crash. cluster-agent's own
// ~2-3s reconcile is already GC-like on the cluster side; this is the rarer control-plane-side
// case that reactive cleanup alone doesn't catch. Alert-only by design — an operator decides
// what to do with a flagged experiment, this never auto-evicts or auto-corrects it.
func (c *Controller) sweepStaleDesiredState(ctx context.Context) error {
	var stale []*domain.Experiment
	for _, status := range []domain.ExperimentStatus{domain.StatusSubmitted, domain.StatusAdmitted, domain.StatusRunning} {
		exps, err := c.store.ListExperimentsWithStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("sweepStaleDesiredState: list %s: %w", status, err)
		}
		for _, exp := range exps {
			_, found, err := metricsdb.LatestJobPhase(ctx, c.metricsDBURL, exp.ID, exp.ClusterName, c.staleDesiredStateThreshold)
			if err != nil {
				return fmt.Errorf("sweepStaleDesiredState: phase %s: %w", exp.ID, err)
			}
			if !found {
				stale = append(stale, exp)
			}
		}
	}
	obsmetrics.StaleDesiredStateExperiments.Set(float64(len(stale)))
	for _, exp := range stale {
		c.logger.Warn("stale desired-state: no recent cluster job report",
			zap.String("id", exp.ID),
			zap.String("status", string(exp.Status)),
			zap.String("cluster", exp.ClusterName),
		)
	}
	return nil
}

// Reconcile runs one full reconciliation pass over all running experiments.
func (c *Controller) Reconcile(ctx context.Context) error {
	exps, err := c.store.ListRunningExperiments(ctx)
	if err != nil {
		return fmt.Errorf("controller.Reconcile: list: %w", err)
	}

	c.logger.Info("reconcile tick", zap.Int("running_experiments", len(exps)))

	now := time.Now().UTC()

	// Build PE report-interval map and metric-direction map; run phase-2 checks in one pass.
	reportIntervalByPE := map[string]time.Duration{}
	metricDirectionsByPE := map[string]map[string]string{}
	if c.phase2Store != nil {
		pes, err := c.phase2Store.ListPlatformExperiments(ctx, "running")
		if err != nil {
			return fmt.Errorf("phase2: list platform experiments: %w", err)
		}
		for _, pe := range pes {
			if pe.ReportIntervalSeconds > 0 {
				reportIntervalByPE[pe.ID] = time.Duration(pe.ReportIntervalSeconds) * time.Second
			}
			metricDirectionsByPE[pe.ID] = make(map[string]string, len(pe.Metrics))
			for _, md := range pe.Metrics {
				if md.Key == "" || (md.Direction != "maximize" && md.Direction != "minimize") {
					return fmt.Errorf("phase2: platform experiment %s has invalid metric contract", pe.ID)
				}
				if _, duplicate := metricDirectionsByPE[pe.ID][md.Key]; duplicate {
					return fmt.Errorf("phase2: platform experiment %s has duplicate metric %q", pe.ID, md.Key)
				}
				metricDirectionsByPE[pe.ID][md.Key] = md.Direction
			}
			if err := c.checkPhase2Transition(ctx, pe, exps); err != nil {
				return fmt.Errorf("phase2: check transition for %s: %w", pe.ID, err)
			}
		}
	}

	// Evict active jobs belonging to closed platform experiments (spec op 7c).
	// This runs on every tick, so it self-heals if the Close() call only updated DB
	// status but pod termination or refunds were not yet completed.
	if c.phase2Store != nil {
		if err := c.reconcileClosedExperiments(ctx); err != nil {
			return fmt.Errorf("reconcile closed experiments: %w", err)
		}
	}

	// Quota exhaustion check: per unique (agentID, platformExpID) pair.
	checked := map[string]bool{}
	for _, exp := range exps {
		if exp.PlatformExperimentID == "" {
			continue
		}
		key := exp.AgentID + "/" + exp.PlatformExperimentID
		if checked[key] {
			continue
		}
		checked[key] = true
		if err := c.checkQuotaExhaustion(ctx, exp.AgentID, exp.PlatformExperimentID, now); err != nil {
			return fmt.Errorf("quota exhaustion check for %s: %w", exp.AgentID, err)
		}
	}

	for _, exp := range exps {
		if err := c.reconcileOne(ctx, exp, now, reportIntervalByPE, metricDirectionsByPE[exp.PlatformExperimentID]); err != nil {
			return fmt.Errorf("reconcile experiment %s: %w", exp.ID, err)
		}
	}
	return nil
}

// checkQuotaExhaustion evicts all running jobs for an agent when actual AccH consumed
// reaches their tier quota. No refund — budget genuinely exhausted.
func (c *Controller) checkQuotaExhaustion(ctx context.Context, agentID, platformExpID string, now time.Time) error {
	aq, err := c.quota.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil || aq == nil {
		return err
	}

	running, err := c.store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}

	// UsedGuaranteedAccH = Σ(estimated_cost of running jobs) + Σ(actual_cost of completed jobs).
	// We want Σ(actual_running) + Σ(actual_completed) per the spec.
	// Replace running-job estimates with actual elapsed cost by adding the per-job delta.
	// The old code added Σ(actual_running) on top of UsedGuaranteedAccH which double-counted
	// running jobs (their estimate was already in UsedGuaranteedAccH).
	// Same delta correction as accelerator, applied to every tracked budget dimension: the Used*
	// fields carry running jobs' *estimates*, so we swap each running job's estimate for its
	// observed-so-far cost. Accelerator is billed per-type (observedAcceleratorCost); CPU is
	// linear, so its observed cost is observedElapsedHours × RequestedCPUCores(). RAM/storage are
	// not enforced here because nothing debits an observed RAM/storage figure to true up against.
	var accDeltaG, accDeltaB, cpuDeltaG, cpuDeltaB float64
	for _, exp := range running {
		accActual, err := c.observedAcceleratorCost(ctx, exp.ID, exp.AcceleratorCount, now)
		if err != nil {
			return fmt.Errorf("observed accelerator cost for %s: %w", exp.ID, err)
		}
		accDelta := accActual - exp.EstimatedCostAccH // negative when under-budget, positive on overrun
		var cpuDelta float64
		if exp.EstimatedCPUCoreHours > 0 {
			hours, err := c.observedElapsedHours(ctx, exp.ID, now)
			if err != nil {
				return fmt.Errorf("observed elapsed hours for %s: %w", exp.ID, err)
			}
			cpuDelta = hours*exp.RequestedCPUCores() - exp.EstimatedCPUCoreHours
		}
		if exp.CapacityTier == domain.CapacityGuaranteed {
			accDeltaG += accDelta
			cpuDeltaG += cpuDelta
		} else {
			accDeltaB += accDelta
			cpuDeltaB += cpuDelta
		}
	}
	// 0.99 not 1.0: floating-point accumulation across many debit/refund calls means "exactly
	// exhausted" may never compare equal/greater than the raw budget — a 1% margin avoids a
	// budget that's genuinely spent sitting just under the threshold forever.
	// Guard each dimension on a positive budget: a zero budget (e.g. a CPU-only platform
	// experiment has GuaranteedAcceleratorHours == 0) is not an exhaustion condition — it means
	// "no quota of that kind to spend", not "quota spent". Without the guard 0 >= 0*0.99 is true on
	// the first tick and every job is evicted for quota_exhaustion.
	spent := func(budget, used, delta float64) bool {
		return budget > 0 && (used+delta) >= budget*0.99
	}
	guaranteedExhausted := spent(aq.GuaranteedAcceleratorHours, aq.UsedGuaranteedAccH, accDeltaG) ||
		spent(aq.GuaranteedCPUCoreHours, aq.UsedGuaranteedCPUCoreH, cpuDeltaG)
	burstExhausted := spent(aq.BurstAcceleratorHours, aq.UsedBurstAccH, accDeltaB) ||
		spent(aq.BurstCPUCoreHours, aq.UsedBurstCPUCoreH, cpuDeltaB)

	if !guaranteedExhausted && !burstExhausted {
		return nil
	}

	for _, exp := range running {
		tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
			(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
		if !tierExhausted {
			continue
		}
		updated, err := c.store.TransitionTerminal(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionQuotaExhaustion))
		if err != nil {
			c.logger.Error("quota exhaustion evict", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
		// Persist the same observed terminal usage as every other terminal path.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
		// Status is already EVICTED above — that's what removes it from the cluster-agent's
		// desired-running set; the Job disappears on the agent's next reconcile pass.
		c.logger.Info("quota exhaustion eviction",
			zap.String("agent", agentID),
			zap.String("exp", exp.ID),
			zap.Float64("actual_guaranteed_acch", aq.UsedGuaranteedAccH+accDeltaG),
			zap.Float64("actual_burst_acch", aq.UsedBurstAccH+accDeltaB),
			zap.Float64("quota_guaranteed_acch", aq.GuaranteedAcceleratorHours),
			zap.Float64("quota_burst_acch", aq.BurstAcceleratorHours),
			zap.Float64("actual_guaranteed_cpuh", aq.UsedGuaranteedCPUCoreH+cpuDeltaG),
			zap.Float64("actual_burst_cpuh", aq.UsedBurstCPUCoreH+cpuDeltaB),
			zap.Float64("quota_guaranteed_cpuh", aq.GuaranteedCPUCoreHours),
			zap.Float64("quota_burst_cpuh", aq.BurstCPUCoreHours),
		)
	}

	// Cancel QUEUED and SUBMITTED jobs and return their reservations — spec 7b.
	// No refund for running jobs (budget genuinely exhausted), but pre-run reservations are returned.
	cancelPreRun := func(exps []*domain.Experiment, finalStatus domain.ExperimentStatus) {
		for _, exp := range exps {
			tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
				(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
			if !tierExhausted {
				continue
			}
			prevStatus := exp.Status
			updated, err := c.store.TransitionTerminal(ctx, exp.ID, prevStatus, finalStatus,
				string(domain.EvictionQuotaExhaustion))
			if err != nil {
				c.logger.Error("quota exhaustion cancel pre-run", zap.String("id", exp.ID), zap.Error(err))
				continue
			}
			if !updated {
				continue
			}
			obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
			// exp never reached RUNNING, so Settle derives zero usage from metrics
			// regardless of the quota_exhaustion reason string — it never consumed anything.
			exp.Status = finalStatus
			exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
			c.settleAndMark(ctx, exp)
			// Status is already updated above (away from SUBMITTED) — if a Job was created
			// for this experiment, it disappears on the cluster-agent's next reconcile pass.
			c.logger.Info("quota exhaustion: cancelled pre-run job",
				zap.String("agent", agentID),
				zap.String("exp", exp.ID),
				zap.String("status", string(exp.Status)),
			)
		}
	}

	// GetAgentQueuedExperiments returns both QUEUED and SUBMITTED experiments.
	// SUBMITTED jobs have a backend workload created but not yet admitted — handled by cancelPreRun.
	preRun, err := c.store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		c.logger.Error("quota exhaustion: list pre-run jobs", zap.String("agent", agentID), zap.Error(err))
	} else {
		cancelPreRun(preRun, domain.StatusRejected)
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

func (c *Controller) reconcileOne(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration, metricDirectionByName map[string]string) error {
	// 1. Silence check.
	evict, reason, err := c.checkSilence(ctx, exp, now, reportIntervalByPE)
	if err != nil {
		return err
	}
	if evict {
		return c.evict(ctx, exp, reason, now)
	}

	// 2. Overrun — only evict if the researcher has no remaining capacity.
	// If they still have quota headroom, let the job continue past the 1.5× estimate.
	evict, reason, err = c.checkOverrun(ctx, exp, now)
	if err != nil {
		return err
	}
	if evict {
		hasCapacity, err := c.researcherHasCapacity(ctx, exp, now)
		if err != nil {
			return err
		}
		if !hasCapacity {
			return c.evict(ctx, exp, reason, now)
		}
	}

	// 3. Metric decline: evict if all observed metrics have been monotonically declining
	// for longer than metricDeclineFraction × estimated_duration_hours.
	evict, reason, err = c.checkMetricDecline(ctx, exp, now, metricDirectionByName)
	if err != nil {
		return err
	}
	if evict {
		return c.evict(ctx, exp, reason, now)
	}

	return nil
}
