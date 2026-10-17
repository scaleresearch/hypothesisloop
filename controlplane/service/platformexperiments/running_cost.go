package platformexperiments

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// AddDesiredQuotaUsage (see db.PlatformExperimentsStore) debits every non-terminal experiment's
// static, admission-time EstimatedCostAccH into "used". That's the right number for a QUEUED or
// SUBMITTED job — nothing has run yet, so the estimate is the only number there is. But once a job
// is RUNNING, the estimate stops tracking reality: a job that overruns its estimate (this
// platform experiment's own stage 2 is uncapped — max_job_hours absent — precisely so a
// confirmation run can run long) keeps debiting only its original guess while it silently
// accrues real, unbilled accelerator-hours, so "Budget Used"/"Utilization" reads far below actual
// live consumption for as long as the job keeps running. correctRunningCosts replaces each
// running job's estimate with its actual observed-elapsed cost so far, the same computation
// controller.checkQuotaExhaustion already trusts to decide when a quota is genuinely spent.

// runningCost is one running job's observed-so-far consumption, derived from metrics (how long it
// has really been alive) and its real spec (how much it consumes per hour). found is false when
// the job has never posted an observation, in which case the figures are meaningless and callers
// must decide for themselves what to do with a job that has not yet been seen.
type runningCost struct {
	hours float64
	accH  float64
	found bool
}

// runningCostCalc computes runningCost for jobs in one platform experiment. Built once per pass so
// the platform experiment row is read once, not per job.
//
// The gap cap is the deployment's (config.SchedulerConfig.ObservationCadence), never the platform
// experiment's own report_interval_seconds. What ObservedElapsedHours measures is liveness —
// dominated by the node-agent heartbeat — not how often a platform experiment asks its jobs to
// report a metric, so a PE's reporting contract has no business setting the billing quantum.
// Deriving it from that made the same job cost differently here than in settlement, visibly
// jumping the moment the job completed.
type runningCostCalc struct {
	svc    *PlatformExperimentsService
	gapCap time.Duration
	now    time.Time
}

func (s *PlatformExperimentsService) newRunningCostCalc() *runningCostCalc {
	return &runningCostCalc{
		svc:    s,
		gapCap: s.observedGapCap,
		now:    time.Now().UTC(),
	}
}

func (c *runningCostCalc) costOf(ctx context.Context, exp *domain.Experiment) (runningCost, error) {
	hours, found, err := metricsdb.ObservedElapsed(ctx, c.svc.metricsDBURL, exp.ID, exp.CreatedAt, c.now, c.gapCap)
	if err != nil {
		return runningCost{}, fmt.Errorf("observed elapsed for %s: %w", exp.ID, err)
	}
	if !found {
		return runningCost{}, nil
	}
	// accH uses the same rate source as settlement (domain.Experiment.RatedCost — the
	// admission-time estimate's implied per-hour rate), not a live catalog lookup
	// (AcceleratorType.LookupCost). That used to make live accounting and settlement price the
	// same job differently: LookupCost reflects today's catalog, which can change (or lose the
	// entry entirely, e.g. #19) after the job was admitted, while settlement always bills at the
	// rate implied by the estimate the job was actually admitted with. RatedCost needs no catalog
	// lookup and so can't be knocked out by a retired flavor either.
	// A CPU job is billed at that rate too: settlement charges every job that reserved an estimate.
	return runningCost{found: true, hours: hours, accH: exp.RatedCost(exp.EstimatedCostAccH, hours)}, nil
}

// correctRunningCosts adjusts quotas in place: for every currently-RUNNING experiment owned by
// one of the agents in quotas, it swaps that job's static EstimatedCostAccH (already included via
// AddDesiredQuotaUsage) for its actual observed-elapsed accelerator cost so far.
func (s *PlatformExperimentsService) correctRunningCosts(ctx context.Context, pe *domain.PlatformExperiment, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	calc := s.newRunningCostCalc()
	settled, err := metricsdb.SettledAccHByJob(ctx, s.metricsDBURL, pe.CreatedAt, pe.ID)
	if err != nil {
		return fmt.Errorf("quota.correctRunningCosts: %w", err)
	}

	for _, q := range quotas {
		running, err := s.store.GetAgentRunningExperiments(ctx, q.AgentID, pe.ID)
		if err != nil {
			return fmt.Errorf("quota.correctRunningCosts: %w", err)
		}
		for _, exp := range running {
			rc, err := calc.costOf(ctx, exp)
			if err != nil {
				return fmt.Errorf("quota.correctRunningCosts: %w", err)
			}
			if !rc.found {
				// No observation has ever been posted for this job yet (e.g. it just started
				// and hasn't reported its first heartbeat). Leave its admission-time estimate
				// in place, exactly as a QUEUED/SUBMITTED job's usage is computed — reporting
				// zero here would silently undercount actual consumption, which is worse than
				// the original over-estimate this fix exists to correct.
				continue
			}
			// rc.accH is cumulative over the job's whole life, and a requeued job's earlier
			// stints are already in q as settled usage.
			delta := rc.accH - settled[exp.ID] - exp.EstimatedCostAccH
			if exp.CapacityTier == domain.CapacityGuaranteed {
				q.UsedGuaranteedAccH += delta
			} else {
				q.UsedBurstAccH += delta
			}
		}
	}
	return nil
}

// addRunningActualCosts adds each running job's observed-so-far cost to q, with no estimate term
// anywhere: q must arrive carrying settled observed usage only. A job that has never reported adds
// nothing — it is not yet visible in the actual state, and this figure exists to answer "has this
// budget been spent?", which nothing unobserved can answer.
func (s *PlatformExperimentsService) addRunningActualCosts(ctx context.Context, pe *domain.PlatformExperiment, q *domain.AgentQuota) error {
	calc := s.newRunningCostCalc()
	settled, err := metricsdb.SettledAccHByJob(ctx, s.metricsDBURL, pe.CreatedAt, pe.ID)
	if err != nil {
		return fmt.Errorf("quota.addRunningActualCosts: %w", err)
	}
	running, err := s.store.GetAgentRunningExperiments(ctx, q.AgentID, pe.ID)
	if err != nil {
		return fmt.Errorf("quota.addRunningActualCosts: %w", err)
	}
	for _, exp := range running {
		rc, err := calc.costOf(ctx, exp)
		if err != nil {
			return fmt.Errorf("quota.addRunningActualCosts: %w", err)
		}
		if !rc.found {
			continue
		}
		// rc.accH is cumulative over the job's whole life, and a requeued job's earlier stints
		// are already in q as settled usage.
		accH := rc.accH - settled[exp.ID]
		if exp.CapacityTier == domain.CapacityGuaranteed {
			q.UsedGuaranteedAccH += accH
		} else {
			q.UsedBurstAccH += accH
		}
	}
	return nil
}
