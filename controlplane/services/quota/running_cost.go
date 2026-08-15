package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// AddDesiredQuotaUsage (see db.PlatformExperimentsStore) debits every non-terminal experiment's
// static, admission-time EstimatedCostAccH into "used". That's the right number for a QUEUED or
// ADMITTED job — nothing has run yet, so the estimate is the only number there is. But once a job
// is RUNNING, the estimate stops tracking reality: a job that overruns its estimate (this
// platform experiment's own stage 2 is uncapped — max_job_hours absent — precisely so a
// confirmation run can run long) keeps debiting only its original guess while it silently
// accrues real, unbilled accelerator-hours, so "Budget Used"/"Utilization" reads far below actual
// live consumption for as long as the job keeps running. correctRunningCosts replaces each
// running job's estimate with its actual observed-elapsed cost so far, the same computation
// controller.checkQuotaExhaustion already trusts to decide when a quota is genuinely spent.

// defaultReportIntervalSeconds/defaultSilenceMultiplier mirror this platform's own defaults
// (settings/hypothesisloop.yaml: scheduler.default_report_interval_seconds,
// scheduler.silence_multiplier) — used only as the fallback gap-detection cadence when a platform
// experiment doesn't set its own report_interval_seconds, exactly as the controller does.
const (
	defaultReportIntervalSeconds = 30
	defaultSilenceMultiplier     = 3.0
)

// correctRunningCosts adjusts quotas in place: for every currently-RUNNING experiment owned by
// one of the agents in quotas, it swaps that job's static EstimatedCostAccH (already included via
// AddDesiredQuotaUsage) for its actual observed-elapsed accelerator cost so far.
func (s *PlatformExperimentsService) correctRunningCosts(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}

	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("quota.correctRunningCosts: %w", err)
	}
	if pe == nil {
		return fmt.Errorf("quota.correctRunningCosts: platform experiment %s not found", platformExpID)
	}
	reportInterval := time.Duration(defaultReportIntervalSeconds) * time.Second
	if pe.ReportIntervalSeconds > 0 {
		reportInterval = time.Duration(pe.ReportIntervalSeconds) * time.Second
	}
	gapCap := time.Duration(defaultSilenceMultiplier * float64(reportInterval))
	now := time.Now().UTC()

	for _, q := range quotas {
		running, err := s.store.GetAgentRunningExperiments(ctx, q.AgentID, platformExpID)
		if err != nil {
			return fmt.Errorf("quota.correctRunningCosts: %w", err)
		}
		for _, exp := range running {
			if exp.AcceleratorCount <= 0 || exp.AcceleratorType == "" {
				continue
			}
			rate, ok := exp.AcceleratorType.LookupCost()
			if !ok {
				return fmt.Errorf("quota.correctRunningCosts: no registered rate for accelerator type %q (experiment %s)", exp.AcceleratorType, exp.ID)
			}
			// The lookback is the job's own real lifetime (its CreatedAt), never a fixed
			// constant — the same reasoning as registry.GetTimeseries using exp.CreatedAt
			// instead of a hardcoded window: a fixed bound would make an old-but-legitimately-
			// running job's real observations invisible to this query.
			lookback := now.Sub(exp.CreatedAt)
			if lookback <= 0 {
				continue
			}
			_, found, err := metricsdb.FirstObserved(ctx, s.metricsDBURL, exp.ID, now, lookback, reportInterval)
			if err != nil {
				return fmt.Errorf("quota.correctRunningCosts: first observed for %s: %w", exp.ID, err)
			}
			if !found {
				// No observation has ever been posted for this job yet (e.g. it just started
				// and hasn't reported its first heartbeat). Leave its admission-time estimate
				// in place, exactly as a QUEUED/ADMITTED job's usage is computed — reporting
				// zero here would silently undercount actual consumption, which is worse than
				// the original over-estimate this fix exists to correct.
				continue
			}
			hours, err := metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, now, lookback, gapCap, reportInterval)
			if err != nil {
				return fmt.Errorf("quota.correctRunningCosts: observed elapsed hours for %s: %w", exp.ID, err)
			}
			actual := hours * float64(exp.AcceleratorCount) * rate
			delta := actual - exp.EstimatedCostAccH // negative when the job is under its estimate so far
			if exp.CapacityTier == domain.CapacityGuaranteed {
				q.UsedGuaranteedAccH += delta
			} else {
				q.UsedBurstAccH += delta
			}
		}
	}
	return nil
}
