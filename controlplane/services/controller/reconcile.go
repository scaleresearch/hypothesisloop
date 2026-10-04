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
	// The closed-experiment and quota-exhaustion checks below need none of these maps and run
	// whether or not the list was read. Silence and the job-length cap judge a job by its
	// platform experiment's own interval, cap and metrics, so a pass that could not read them
	// judges no job: on the defaults a job with a longer interval would be evicted as silent.
	pes, listErr := c.stagesStore.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpRunning)
	if listErr != nil {
		c.logger.Error("stages: list platform experiments; no job is judged for silence or length this pass", zap.Error(listErr))
		errs = append(errs, fmt.Errorf("stages: list platform experiments: %w", listErr))
	}
	for _, pe := range pes {
		if pe.ReportIntervalSeconds > 0 {
			reportIntervalByPE[pe.ID] = time.Duration(pe.ReportIntervalSeconds) * time.Second
		}
		// Every declared metric counts as a sign of progress, whatever its role. A ranking
		// metric may be posted once, at completion, and a job judged by those alone was evicted
		// as never_reported_metrics while its other declared metrics moved.
		for _, m := range pe.Metrics {
			declaredMetricKeysByPE[pe.ID] = append(declaredMetricKeysByPE[pe.ID], m.Key)
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
		if err := c.checkQuotaExhaustion(ctx, exp.AgentID, exp.PlatformExperimentID); err != nil {
			c.logger.Error("quota exhaustion check", zap.String("agent", exp.AgentID), zap.Error(err))
			errs = append(errs, fmt.Errorf("quota exhaustion check for %s: %w", exp.AgentID, err))
		}
	}

	if listErr != nil {
		return errors.Join(errs...)
	}
	for _, exp := range exps {
		if err := c.reconcileOne(ctx, exp, now, obs, reportIntervalByPE, maxJobHoursByPE, declaredMetricKeysByPE); err != nil {
			c.logger.Error("reconcile experiment", zap.String("exp", exp.ID), zap.Error(err))
			errs = append(errs, fmt.Errorf("reconcile experiment %s: %w", exp.ID, err))
		}
	}
	return errors.Join(errs...)
}

// exhaustionTolerance is how far under a budget, in accelerator-hours, observed usage counts as
// having reached it: usage is a sum of floats and can land a rounding error short of a budget it
// equals.
const exhaustionTolerance = 1e-6

// checkQuotaExhaustion evicts all running jobs for an agent when actual AccH consumed
// reaches their tier quota. No refund: the budget is genuinely exhausted. One job's failed write does
// not stop the others, and the failures are returned together.
func (c *Controller) checkQuotaExhaustion(ctx context.Context, agentID, platformExpID string) error {
	// Observed consumption only. A reservation-inclusive figure would trip this check on work
	// that has merely been queued, and evict — irreversibly, unrefunded — running jobs for budget
	// nobody has spent. RAM/storage are not enforced here because nothing observes them.
	aq, err := c.quota.GetObservedAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}
	if aq == nil {
		return fmt.Errorf("agent %s runs jobs in %s and has no quota there", agentID, platformExpID)
	}

	running, err := c.store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}
	// A zero budget means there is no quota of that kind to spend. Without the guard 0 >= 0 is
	// true on the first tick and every job is evicted for quota_exhaustion.
	spent := func(budget, used float64) bool {
		return budget > 0 && used >= budget-exhaustionTolerance
	}
	// Burst hours are inside the agent's share of the budget: a spent share ends work at either
	// priority.
	shareExhausted := spent(aq.Share(), aq.Used())
	guaranteedExhausted := shareExhausted || spent(aq.GuaranteedAcceleratorHours, aq.UsedGuaranteedAccH)
	burstExhausted := shareExhausted || spent(aq.BurstAcceleratorHours, aq.UsedBurstAccH)

	if !guaranteedExhausted && !burstExhausted {
		return nil
	}

	tierExhausted := func(exp *domain.Experiment) bool {
		return (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
			(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
	}

	var errs []error
	for _, exp := range running {
		if !tierExhausted(exp) {
			continue
		}
		outcome, err := c.store.ResolveTermination(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionQuotaExhaustion))
		if err != nil {
			errs = append(errs, fmt.Errorf("evict %s: %w", exp.ID, err))
			continue
		}
		if outcome != domain.TerminationWritten {
			continue
		}
		obsmetrics.CountEviction(domain.EvictionQuotaExhaustion)
		// Persist the same observed terminal usage as every other terminal path.
		exp.Status = domain.StatusEvicted
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
	}

	// Cancel pre-run jobs and return their reservations — spec 7b. No refund for running jobs
	// (budget genuinely exhausted), but pre-run reservations are returned.
	preRun, err := c.store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list pre-run jobs: %w", err))...)
	}
	for _, exp := range preRun {
		if !tierExhausted(exp) {
			continue
		}
		outcome, err := c.store.ResolveTermination(ctx, exp.ID, exp.Status, domain.StatusRejected,
			string(domain.EvictionQuotaExhaustion))
		if err != nil {
			errs = append(errs, fmt.Errorf("cancel pre-run %s: %w", exp.ID, err))
			continue
		}
		if outcome != domain.TerminationWritten {
			continue
		}
		obsmetrics.CountEviction(domain.EvictionQuotaExhaustion)
		// exp never reached RUNNING, so Settle derives zero usage from metrics. If a Job was
		// created for it, it disappears on the cluster-agent's next reconcile pass.
		exp.Status = domain.StatusRejected
		exp.EvictionReason = string(domain.EvictionQuotaExhaustion)
		c.settleAndMark(ctx, exp)
		c.logger.Info("quota exhaustion: cancelled pre-run job",
			zap.String("agent", agentID),
			zap.String("exp", exp.ID),
		)
	}
	return errors.Join(errs...)
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
			return c.evict(ctx, exp, domain.EvictionJobTooLong)
		}
	}

	evict, reason, err := c.checkSilence(ctx, exp, now, reportIntervalByPE, declaredMetricKeysByPE[exp.PlatformExperimentID])
	if err != nil {
		return err
	}
	if evict {
		return c.evict(ctx, exp, reason)
	}
	return nil
}
