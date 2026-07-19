package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// sweepStaleDesiredState flags (log + gauge, never mutates) SUBMITTED/ADMITTED/RUNNING
// experiments with no recent cluster job report — orphaned desired-state that survived an
// extended cluster-agent outage combined with a control-plane bug/crash. cluster-agent's own
// ~2-3s reconcile is already GC-like on the cluster side; this is the rarer control-plane-side
// case that reactive cleanup alone doesn't catch. Alert-only by design — an operator decides
// what to do with a flagged experiment, this never auto-evicts or auto-corrects it.
func (c *Controller) sweepStaleDesiredState(ctx context.Context) error {
	stale, err := c.store.ListStaleDesiredState(ctx, c.staleDesiredStateThreshold)
	if err != nil {
		return fmt.Errorf("sweepStaleDesiredState: %w", err)
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
	// metricDirectionByName: metricName → "maximize"|"minimize", derived from any PE's metric definitions.
	metricDirectionByName := map[string]string{}
	if c.phase2Store != nil {
		pes, err := c.phase2Store.ListPlatformExperiments(ctx, "running")
		if err != nil {
			c.logger.Error("phase2: list platform experiments", zap.Error(err))
		} else {
			for _, pe := range pes {
				if pe.ReportIntervalSeconds > 0 {
					reportIntervalByPE[pe.ID] = time.Duration(pe.ReportIntervalSeconds) * time.Second
				}
				for _, md := range pe.Metrics {
					metricDirectionByName[md.Key] = md.Direction
				}
				if err := c.checkPhase2Transition(ctx, pe, exps); err != nil {
					c.logger.Error("phase2: check transition", zap.String("pe", pe.ID), zap.Error(err))
				}
			}
		}
	}

	// Evict active jobs belonging to closed platform experiments (spec op 7c).
	// This runs on every tick, so it self-heals if the Close() call only updated DB
	// status but pod termination or refunds were not yet completed.
	if c.phase2Store != nil {
		if err := c.reconcileClosedExperiments(ctx); err != nil {
			c.logger.Error("reconcile closed experiments", zap.Error(err))
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
			c.logger.Error("quota exhaustion check", zap.String("agent", exp.AgentID), zap.Error(err))
		}
	}

	for _, exp := range exps {
		if err := c.reconcileOne(ctx, exp, now, reportIntervalByPE, metricDirectionByName); err != nil {
			c.logger.Error("reconcile experiment",
				zap.String("id", exp.ID),
				zap.Error(err),
			)
		}
	}
	return nil
}

// checkQuotaExhaustion evicts all running jobs for an agent when actual T4h consumed
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

	// UsedGuaranteedT4H = Σ(estimated_cost of running jobs) + Σ(actual_cost of completed jobs).
	// We want Σ(actual_running) + Σ(actual_completed) per the spec.
	// Replace running-job estimates with actual elapsed cost by adding the per-job delta.
	// The old code added Σ(actual_running) on top of UsedGuaranteedT4H which double-counted
	// running jobs (their estimate was already in UsedGuaranteedT4H).
	var deltaGuaranteed, deltaBurst float64
	for _, exp := range running {
		actual, err := c.observedGPUCost(ctx, exp.ID, exp.GPUCount, now)
		if err != nil {
			c.logger.Error("checkQuotaExhaustion: observed GPU cost", zap.String("experiment", exp.ID), zap.Error(err))
			continue
		}
		delta := actual - exp.EstimatedCostT4H // negative when under-budget, positive on overrun
		if exp.CapacityTier == domain.CapacityGuaranteed {
			deltaGuaranteed += delta
		} else {
			deltaBurst += delta
		}
	}
	// 0.99 not 1.0: floating-point accumulation across many debit/refund calls means "exactly
	// exhausted" may never compare equal/greater than the raw budget — a 1% margin avoids a
	// budget that's genuinely spent sitting just under the threshold forever.
	guaranteedExhausted := (aq.UsedGuaranteedT4H + deltaGuaranteed) >= aq.GuaranteedT4Hours*0.99
	burstExhausted := (aq.UsedBurstT4H + deltaBurst) >= aq.BurstT4Hours*0.99

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
		// Unlike a normal RUNNING job (whose reservation job_watcher's onRunning already
		// cleared), a job resubmitted after a preemption/reschedule cycle can still hold a live
		// reservation until its own watch cycle re-confirms RUNNING — release it here too,
		// unconditionally, matching every other terminal-transition path (evict, cancel,
		// onFinished). No-op if already gone.
		_ = c.store.DeletePendingReservation(ctx, exp.ID)
		// exp was RUNNING (StartedAt set) and the reason is EvictionQuotaExhaustion, so
		// settleAndMark's Settle call is a deliberate no-op (budget genuinely spent) — it just
		// marks the row settled immediately, nothing to write.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
		// Status is already EVICTED above — that's what removes it from the cluster-agent's
		// desired-running set; the Job disappears on the agent's next reconcile pass.
		c.logger.Info("quota exhaustion eviction",
			zap.String("agent", agentID),
			zap.String("exp", exp.ID),
			zap.Float64("actual_guaranteed", aq.UsedGuaranteedT4H+deltaGuaranteed),
			zap.Float64("actual_burst", aq.UsedBurstT4H+deltaBurst),
			zap.Float64("quota_guaranteed", aq.GuaranteedT4Hours),
			zap.Float64("quota_burst", aq.BurstT4Hours),
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
			// exp never reached RUNNING (StartedAt nil), so Settle refunds it fully to 0
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
	if evict, reason := c.checkSilence(ctx, exp, now, reportIntervalByPE); evict {
		return c.evict(ctx, exp, reason, now)
	}

	// 2. Overrun — only evict if the researcher has no remaining capacity.
	// If they still have quota headroom, let the job continue past the 1.5× estimate.
	if evict, reason := c.checkOverrun(ctx, exp, now); evict {
		if !c.researcherHasCapacity(ctx, exp, now) {
			return c.evict(ctx, exp, reason, now)
		}
	}

	// 3. Metric decline: evict if all observed metrics have been monotonically declining
	// for longer than metricDeclineFraction × estimated_duration_hours.
	if evict, reason := c.checkMetricDecline(ctx, exp, now, metricDirectionByName); evict {
		return c.evict(ctx, exp, reason, now)
	}

	return nil
}
