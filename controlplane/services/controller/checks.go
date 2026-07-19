package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// checkMetricDecline returns true when every metric this experiment has reported shows no
// improvement for a continuous duration >= metricDeclineFraction × estimated_duration_hours.
// Recomputed from GreptimeDB's own stored samples every call — no in-memory window, so there is
// nothing here that a controller restart could lose or two replicas could disagree about.
func (c *Controller) checkMetricDecline(ctx context.Context, exp *domain.Experiment, now time.Time, metricDirectionByName map[string]string) (bool, domain.EvictionReason) {
	if exp.StartedAt == nil || exp.EstimatedDurationHours <= 0 || c.metricDeclineFraction <= 0 {
		return false, ""
	}
	declineWindow := time.Duration(c.metricDeclineFraction * exp.EstimatedDurationHours * float64(time.Hour))

	// ~50 grid points across the job's whole lifetime so far, regardless of how long that is —
	// coarse enough to keep the query cheap over a multi-hour run, fine enough to resolve a
	// genuine trend shift within declineWindow.
	step := now.Sub(*exp.StartedAt) / 50
	if step < c.observedStep() {
		step = c.observedStep()
	}

	promQL := fmt.Sprintf(`experiment_metric_value{job_id=%q}`, exp.ID)
	series, err := metricsdb.QueryRange(ctx, c.metricsDBURL, promQL, *exp.StartedAt, now, step)
	if err != nil {
		c.logger.Error("metric decline query failed", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}

	foundAny := false
	for _, s := range series {
		if len(s.Points) < 2 {
			continue
		}
		minimize := metricDirectionByName[s.Labels["metric_name"]] == "minimize"

		lastImproved := *exp.StartedAt
		for i := 1; i < len(s.Points); i++ {
			improved := s.Points[i].Value > s.Points[i-1].Value
			if minimize {
				improved = s.Points[i].Value < s.Points[i-1].Value
			}
			if improved && s.Points[i].Time.After(lastImproved) {
				lastImproved = s.Points[i].Time
			}
		}

		// Non-improving streak itself must span >= declineWindow to evict.
		if now.Sub(lastImproved) < declineWindow {
			return false, ""
		}
		foundAny = true
	}

	if !foundAny {
		return false, ""
	}
	return true, domain.EvictionMetricDecline
}

// checkSilence returns true when no real observation (node-agent heartbeat or job-reported
// metric) exists within max(minSilenceWindow, 3× the PE's report interval) — a stateless
// GreptimeDB query every time, so a controller restart or a second replica never has a
// "haven't seen anything yet" warm-up gap that looks like silence. Falls back to
// defaultReportInterval when the PE interval is unknown.
//
// Before evicting, this also checks the cluster-agent's latest job report: a job that's
// between pods (a node-death reschedule in progress, same self-heal path scenario 1 exercises)
// is expected to be quiet — that's not the "training process silently died" case this reason is
// for. Only a report showing the pod actually Running, with no metrics despite that, is real
// silence. No report yet, or a query error, means "can't tell" — skip and let the next
// reconcile pass (or ListStaleDesiredState, for a job that never got a report at all) decide.
func (c *Controller) checkSilence(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration) (bool, domain.EvictionReason) {
	if exp.StartedAt == nil {
		return false, ""
	}
	reportInterval := c.defaultReportInterval
	if d, ok := reportIntervalByPE[exp.PlatformExperimentID]; ok && d > 0 {
		reportInterval = d
	}
	window := time.Duration(c.silenceMultiplier * float64(reportInterval))
	if window < c.minSilenceWindow {
		window = c.minSilenceWindow
	}
	if now.Sub(*exp.StartedAt) < window {
		return false, "" // hasn't even had one full window to report in yet
	}
	alive, err := c.isAlive(ctx, exp.ID, window)
	if err != nil {
		// GreptimeDB unreachable: don't evict on a query failure indistinguishable from
		// genuine silence — wait for the next reconcile pass to try again.
		c.logger.Error("silence check query failed", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}
	if !alive {
		report, err := c.store.GetJobReport(ctx, exp.ID)
		if err != nil {
			c.logger.Error("silence check: job report query failed", zap.String("experiment", exp.ID), zap.Error(err))
			return false, ""
		}
		if report != nil && workload.ParseJobPhase(report.Phase) != workload.JobPhaseRunning {
			// Pod isn't up right now (Pending/recreating/gone) — quiet is expected here,
			// not evidence of a hung process. Let the reschedule finish; next tick re-checks.
			return false, ""
		}
		return true, domain.EvictionSilent
	}
	return false, ""
}

// researcherHasCapacity returns true when the researcher still has unconsumed quota in the
// experiment's capacity tier — used to suppress overrun evictions when budget remains.
// It accounts for the overrunning experiment's actual elapsed cost beyond its estimate so
// that an already-exhausted agent is not given a false reprieve.
func (c *Controller) researcherHasCapacity(ctx context.Context, exp *domain.Experiment, now time.Time) bool {
	if exp.PlatformExperimentID == "" {
		return false
	}
	aq, err := c.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil || aq == nil {
		return false
	}
	// Aggregate overrun deltas across all running jobs in the same tier, mirroring
	// checkQuotaExhaustion (7b). Evaluating only the current job's overrun understates
	// true consumption when multiple jobs are simultaneously overrunning.
	running, err := c.store.GetAgentRunningExperiments(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return false
	}
	var deltaGuaranteed, deltaBurst float64
	for _, r := range running {
		actual, err := c.observedAcceleratorCost(ctx, r.ID, r.AcceleratorCount, now)
		if err != nil {
			c.logger.Error("researcherHasCapacity: observed accelerator cost", zap.String("experiment", r.ID), zap.Error(err))
			continue
		}
		d := actual - r.EstimatedCostAccH
		if d < 0 {
			d = 0
		}
		if r.CapacityTier == domain.CapacityGuaranteed {
			deltaGuaranteed += d
		} else {
			deltaBurst += d
		}
	}
	switch exp.CapacityTier {
	case domain.CapacityGuaranteed:
		return (aq.UsedGuaranteedAccH + deltaGuaranteed) < aq.GuaranteedAcceleratorHours*0.99
	case domain.CapacityBurst:
		return (aq.UsedBurstAccH + deltaBurst) < aq.BurstAcceleratorHours*0.99
	}
	return false
}

// checkOverrun returns true when the experiment has been confirmed running (via
// observedElapsedHours, not wall-clock StartedAt) longer than overrunMultiplier× estimated — a
// job stuck in a reschedule/node-death gap for most of its wall-clock life isn't actually
// overrunning its budget. A query error is logged and treated as "not overrunning" for this
// pass: the next reconcile tick tries again, same as every other observed-time check in this
// file.
func (c *Controller) checkOverrun(ctx context.Context, exp *domain.Experiment, now time.Time) (bool, domain.EvictionReason) {
	if exp.EstimatedDurationHours <= 0 {
		return false, ""
	}
	hours, err := c.observedElapsedHours(ctx, exp.ID, now)
	if err != nil {
		c.logger.Error("checkOverrun: observed elapsed hours", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}
	if hours > exp.EstimatedDurationHours*c.overrunMultiplier {
		return true, domain.EvictionOverrun
	}
	return false, ""
}
