package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// Reconcile runs one full reconciliation pass over all running experiments.
func (c *Controller) Reconcile(ctx context.Context) error {
	exps, err := c.store.ListRunningExperiments(ctx)
	if err != nil {
		return fmt.Errorf("controller.Reconcile: list: %w", err)
	}

	c.logger.Info("reconcile tick", zap.Int("running_experiments", len(exps)))

	// One instant for the whole pass. Every consumer below judges the same job by the same
	// numbers, instead of each taking its own clock reading and reaching a different verdict.
	now := time.Now().UTC()

	// One observed-elapsed read per job, shared by the stage-progress calculation and the
	// job-length cap. See tickObservations for why this is not a cache.
	obs := c.observeElapsed(ctx, exps, now)

	// One bad row must not stop the pass: every other experiment still needs its stage
	// advance, silence check, job-length cap and quota check this tick. Failures are logged
	// as they happen and returned together at the end.
	var errs []error

	// Build the PE report-interval and stage job-length maps and advance stage ladders in one pass.
	reportIntervalByPE := map[string]time.Duration{}
	maxJobHoursByPE := map[string]float64{}
	declaredMetricKeysByPE := map[string][]string{}
	// Only the stage ladder needs these maps. Silence and quota-exhaustion checks below do
	// not, so an unreadable platform-experiment list must not stop them from running — that
	// turned one failing query into a pass where nothing at all was reconciled.
	pes, err := c.stagesStore.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpRunning)
	if err != nil {
		c.logger.Error("stages: list platform experiments; continuing without stage maps", zap.Error(err))
		errs = append(errs, fmt.Errorf("stages: list platform experiments: %w", err))
		pes = nil
	}
	for _, pe := range pes {
		if pe.ReportIntervalSeconds > 0 {
			reportIntervalByPE[pe.ID] = time.Duration(pe.ReportIntervalSeconds) * time.Second
		}
		var rankingKeys []string
		for _, m := range pe.Metrics {
			// Only ranking metrics prove training progress — a constraint or attribute
			// metric changing (or staying put) says nothing about whether the run is stuck.
			if m.EffectiveRole() == domain.MetricRoleRanking {
				rankingKeys = append(rankingKeys, m.Key)
			}
		}
		if len(rankingKeys) > 0 {
			declaredMetricKeysByPE[pe.ID] = rankingKeys
		}
		// Read before advanceStages: a boundary crossed on this tick takes effect from the
		// next one, so no job is evicted under a cap it was never running under.
		if maxHours := pe.CurrentMaxJobHours(); maxHours > 0 {
			maxJobHoursByPE[pe.ID] = maxHours
		}
		// Stage ranking reads this contract per boundary; fail fast on a bad one rather
		// than at the cut, where it would silently skew a cut. Create/Update already
		// reject a bad contract before an experiment can ever reach here — this is
		// defense against a row written before that validation existed.
		if err := domain.ValidateMetricDefinitions(pe.Metrics); err != nil {
			c.logger.Error("stages: invalid metric contract", zap.String("pe", pe.ID), zap.Error(err))
			errs = append(errs, fmt.Errorf("stages: platform experiment %s has invalid metric contract: %w", pe.ID, err))
			continue
		}
		if err := c.advanceStages(ctx, pe, exps, obs, now); err != nil {
			c.logger.Error("stages: advance", zap.String("pe", pe.ID), zap.Error(err))
			errs = append(errs, fmt.Errorf("stages: advance %s: %w", pe.ID, err))
		}
	}

	// Evict active jobs belonging to closed platform experiments (spec op 7c).
	// This runs on every tick, so it self-heals if the Close() call only updated DB
	// status but pod termination or refunds were not yet completed.
	if err := c.reconcileClosedExperiments(ctx); err != nil {
		c.logger.Error("reconcile closed experiments", zap.Error(err))
		errs = append(errs, fmt.Errorf("reconcile closed experiments: %w", err))
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
			errs = append(errs, fmt.Errorf("quota exhaustion check for %s: %w", exp.AgentID, err))
		}
	}

	for _, exp := range exps {
		if err := c.reconcileOne(ctx, exp, now, obs, reportIntervalByPE, maxJobHoursByPE, declaredMetricKeysByPE); err != nil {
			c.logger.Error("reconcile experiment", zap.String("exp", exp.ID), zap.Error(err))
			errs = append(errs, fmt.Errorf("reconcile experiment %s: %w", exp.ID, err))
		}
	}
	return errors.Join(errs...)
}

// checkQuotaExhaustion evicts all running jobs for an agent when actual AccH consumed
// reaches their tier quota. No refund — budget genuinely exhausted.
func (c *Controller) checkQuotaExhaustion(ctx context.Context, agentID, platformExpID string, now time.Time) error {
	// Observed consumption only. A reservation-inclusive figure would trip this check on work
	// that has merely been queued, and evict — irreversibly, unrefunded — running jobs for budget
	// nobody has spent. RAM/storage are not enforced here because nothing observes them.
	aq, err := c.quota.GetObservedAgentQuota(ctx, agentID, platformExpID)
	if err != nil || aq == nil {
		return err
	}

	running, err := c.store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}
	// ADMITTED jobs already hold their reservation and are about to have a workload created for
	// them — without stopping these too, a cut/exhausted agent's job can still start after this
	// sweep believes it has stopped everything (GetAgentQueuedExperiments also returns QUEUED and
	// SUBMITTED, which never spent observed hours and are excluded by tierExhausted's usage check
	// below being about observed consumption, not queue membership).
	admitted, err := c.store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}

	// 0.99 not 1.0: floating-point accumulation across many debit/refund calls means "exactly
	// exhausted" may never compare equal/greater than the raw budget — a 1% margin avoids a
	// budget that's genuinely spent sitting just under the threshold forever.
	// Guard each dimension on a positive budget: a zero budget (e.g. a CPU-only platform
	// experiment has GuaranteedAcceleratorHours == 0) is not an exhaustion condition — it means
	// "no quota of that kind to spend", not "quota spent". Without the guard 0 >= 0*0.99 is true on
	// the first tick and every job is evicted for quota_exhaustion.
	spent := func(budget, used float64) bool {
		return budget > 0 && used >= budget*0.99
	}
	guaranteedExhausted := spent(aq.GuaranteedAcceleratorHours, aq.UsedGuaranteedAccH) ||
		spent(aq.GuaranteedCPUCoreHours, aq.UsedGuaranteedCPUCoreH)
	burstExhausted := spent(aq.BurstAcceleratorHours, aq.UsedBurstAccH) ||
		spent(aq.BurstCPUCoreHours, aq.UsedBurstCPUCoreH)

	if !guaranteedExhausted && !burstExhausted {
		return nil
	}

	for _, exp := range running {
		tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
			(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
		if !tierExhausted {
			continue
		}
		outcome, err := c.store.ResolveTermination(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionQuotaExhaustion))
		if err != nil {
			c.logger.Error("quota exhaustion evict", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if outcome != domain.TerminationWritten {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
		// Persist the same observed terminal usage as every other terminal path.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
	}

	for _, exp := range admitted {
		if exp.Status != domain.StatusAdmitted {
			continue // QUEUED/SUBMITTED haven't claimed a reservation the way ADMITTED has
		}
		tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
			(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
		if !tierExhausted {
			continue
		}
		outcome, err := c.store.ResolveTermination(ctx, exp.ID, domain.StatusAdmitted, domain.StatusEvicted,
			string(domain.EvictionQuotaExhaustion))
		if err != nil {
			c.logger.Error("quota exhaustion evict admitted", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if outcome != domain.TerminationWritten {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
		// Never reached RUNNING; settlement derives zero usage from absent metrics. EVICTED (not
		// REJECTED) to match CancelExperiment's convention: ADMITTED already has a workload being
		// created for it, so moving it out of the SUBMITTED/ADMITTED/RUNNING desired-state set is
		// what makes the cluster-agent tear that workload down.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
		// Status is already EVICTED above — that's what removes it from the cluster-agent's
		// desired-running set; the Job disappears on the agent's next reconcile pass.
		c.logger.Info("quota exhaustion eviction",
			zap.String("agent", agentID),
			zap.String("exp", exp.ID),
			zap.Float64("actual_guaranteed_acch", aq.UsedGuaranteedAccH),
			zap.Float64("actual_burst_acch", aq.UsedBurstAccH),
			zap.Float64("quota_guaranteed_acch", aq.GuaranteedAcceleratorHours),
			zap.Float64("quota_burst_acch", aq.BurstAcceleratorHours),
			zap.Float64("actual_guaranteed_cpuh", aq.UsedGuaranteedCPUCoreH),
			zap.Float64("actual_burst_cpuh", aq.UsedBurstCPUCoreH),
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
			outcome, err := c.store.ResolveTermination(ctx, exp.ID, prevStatus, finalStatus,
				string(domain.EvictionQuotaExhaustion))
			if err != nil {
				c.logger.Error("quota exhaustion cancel pre-run", zap.String("id", exp.ID), zap.Error(err))
				continue
			}
			if outcome != domain.TerminationWritten {
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

	return nil
}

// reconcileOne evicts exp when it has gone silent or has outrun the current stage's job-length
// cap. Quality is the agent's own call: it reads its metrics and cancels a bad run itself, and
// quota exhaustion bounds the damage if it doesn't.
func (c *Controller) reconcileOne(ctx context.Context, exp *domain.Experiment, now time.Time, obs *tickObservations, reportIntervalByPE map[string]time.Duration, maxJobHoursByPE map[string]float64, declaredMetricKeysByPE map[string][]string) error {
	if maxHours, ok := maxJobHoursByPE[exp.PlatformExperimentID]; ok {
		// The pass's own reading. A job this pass could not measure is left alone rather than
		// evicted on a number nobody has.
		hours, measured := obs.hours(exp.ID)
		if measured && hours > maxHours {
			c.logger.Info("stage job-length cap exceeded",
				zap.String("exp", exp.ID),
				zap.Float64("observed_hours", hours),
				zap.Float64("max_job_hours", maxHours),
			)
			return c.evict(ctx, exp, domain.EvictionJobTooLong, now)
		}
	}

	evict, reason, err := c.checkSilence(ctx, exp, now, reportIntervalByPE, declaredMetricKeysByPE[exp.PlatformExperimentID])
	if err != nil {
		return err
	}
	if evict {
		return c.evict(ctx, exp, reason, now)
	}
	return nil
}
