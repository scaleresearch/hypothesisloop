package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// checkMetricDecline returns true when every metric this experiment has reported shows no
// improvement for a continuous duration >= metricDeclineFraction × estimated_duration_hours.
// Recomputed from GreptimeDB's own stored samples every call — no in-memory window, so there is
// nothing here that a controller restart could lose or two replicas could disagree about.
func (c *Controller) checkMetricDecline(ctx context.Context, exp *domain.Experiment, now time.Time, metricDirectionByName map[string]string) (bool, domain.EvictionReason, error) {
	if exp.EstimatedDurationHours <= 0 || c.metricDeclineFraction <= 0 {
		return false, "", nil
	}
	startedAt, observed, err := metricsdb.FirstObserved(ctx, c.metricsDBURL, exp.ID, now, c.observedMaxLookback(), c.observedStep())
	if err != nil {
		return false, "", fmt.Errorf("metric decline first observation: %w", err)
	}
	if !observed {
		return false, "", nil
	}
	declineWindow := time.Duration(c.metricDeclineFraction * exp.EstimatedDurationHours * float64(time.Hour))

	// ~50 grid points across the job's whole lifetime so far, regardless of how long that is —
	// coarse enough to keep the query cheap over a multi-hour run, fine enough to resolve a
	// genuine trend shift within declineWindow.
	step := now.Sub(startedAt) / 50
	if step < c.observedStep() {
		step = c.observedStep()
	}
	// The query-cost floor above is sized for a multi-hour job, where declineWindow is many
	// multiples of it. A short job's declineWindow can be only ~2x that floor, leaving too few
	// grid points to tell "genuinely declining" apart from one ordinary reporting-cadence gap
	// landing on the wrong side of a grid line — exactly the kind of timing jitter real load
	// introduces. Also cap the step so at least 4 grid points always span declineWindow.
	if maxStep := declineWindow / 4; maxStep > 0 && step > maxStep {
		step = maxStep
	}

	promQL := fmt.Sprintf(`experiment_metric_value{job_id=%q}`, exp.ID)
	series, err := metricsdb.QueryRange(ctx, c.metricsDBURL, promQL, startedAt, now, step)
	if err != nil {
		return false, "", fmt.Errorf("metric decline range: %w", err)
	}

	foundAny := false
	for _, s := range series {
		if len(s.Points) < 2 {
			continue
		}
		direction, declared := metricDirectionByName[s.Labels["metric_name"]]
		if !declared {
			// Workloads may report dashboard-only secondary series. They have no declared
			// optimization direction and must not participate in an eviction decision.
			continue
		}
		minimize := direction == "minimize"

		lastImproved := startedAt
		for i := 1; i < len(s.Points); i++ {
			improved := s.Points[i].Value > s.Points[i-1].Value
			if minimize {
				improved = s.Points[i].Value < s.Points[i-1].Value
			}
			if improved && s.Points[i].Time.After(lastImproved) {
				lastImproved = s.Points[i].Time
			}
		}

		// Time the job was not running at all must not count against it. A job requeued after
		// preemption sits QUEUED reporting nothing, and is then evicted the instant it resumes:
		// lastImproved is still its pre-preemption value while now has moved on, so the streak
		// below spans the whole wait for capacity. That contradicts the requeue semantics
		// preemption promises (loop_preempt.go: "the job returns to QUEUED and will run again").
		// Note this stretch is usually trailing — the job has produced no new samples yet, so the
		// point loop above never sees it as a gap between samples.
		//
		// This deliberately keys on liveness, not on the absence of metrics: a job that stayed up
		// and simply stopped reporting the metrics it owes has heartbeats throughout, so its
		// streak is left intact and it stays evictable. Silence must not buy immunity.
		if notAlive, found, err := metricsdb.LastNotAlive(ctx, c.metricsDBURL, exp.ID, lastImproved, now, c.observedGapCap(), c.observedStep()); err != nil {
			return false, "", fmt.Errorf("metric decline liveness: %w", err)
		} else if found && notAlive.After(lastImproved) {
			lastImproved = notAlive
		}

		// Non-improving streak itself must span >= declineWindow to evict.
		if now.Sub(lastImproved) < declineWindow {
			return false, "", nil
		}
		foundAny = true
	}

	if !foundAny {
		return false, "", nil
	}
	return true, domain.EvictionMetricDecline, nil
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
// reconcile pass (or the stale desired-state metric sweep, for a job never reported) decide.
func (c *Controller) checkSilence(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration) (bool, domain.EvictionReason, error) {
	startedAt, observed, err := metricsdb.FirstObserved(ctx, c.metricsDBURL, exp.ID, now, c.observedMaxLookback(), c.observedStep())
	if err != nil {
		return false, "", fmt.Errorf("silence first observation: %w", err)
	}
	if !observed {
		return false, "", nil
	}
	reportInterval := c.defaultReportInterval
	if d, ok := reportIntervalByPE[exp.PlatformExperimentID]; ok && d > 0 {
		reportInterval = d
	}
	window := time.Duration(c.silenceMultiplier * float64(reportInterval))
	if window < c.minSilenceWindow {
		window = c.minSilenceWindow
	}
	if now.Sub(startedAt) < window {
		return false, "", nil // hasn't even had one full window to report in yet
	}
	alive, err := c.isAlive(ctx, exp.ID, window)
	if err != nil {
		return false, "", fmt.Errorf("silence liveness: %w", err)
	}
	if !alive {
		phase, found, err := metricsdb.LatestJobPhase(ctx, c.metricsDBURL, exp.ID, exp.ClusterName, window)
		if err != nil {
			return false, "", fmt.Errorf("silence job phase: %w", err)
		}
		if found && phase != workload.JobPhaseRunning {
			// Pod isn't up right now (Pending/recreating/gone) — quiet is expected here,
			// not evidence of a hung process. Let the reschedule finish; next tick re-checks.
			return false, "", nil
		}
		return true, domain.EvictionSilent, nil
	}
	return false, "", nil
}

// researcherHasCapacity returns true when the researcher still has unconsumed quota in the
// experiment's capacity tier — used to suppress overrun evictions when budget remains.
// It accounts for the overrunning experiment's actual elapsed cost beyond its estimate so
// that an already-exhausted agent is not given a false reprieve.
func (c *Controller) researcherHasCapacity(ctx context.Context, exp *domain.Experiment, now time.Time) (bool, error) {
	if exp.PlatformExperimentID == "" {
		return false, fmt.Errorf("experiment has no platform experiment")
	}
	aq, err := c.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return false, err
	}
	if aq == nil {
		return false, fmt.Errorf("agent quota not found")
	}
	// Aggregate overrun deltas across all running jobs in the same tier, mirroring
	// checkQuotaExhaustion (7b). Evaluating only the current job's overrun understates
	// true consumption when multiple jobs are simultaneously overrunning.
	running, err := c.store.GetAgentRunningExperiments(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return false, err
	}
	var accDeltaG, accDeltaB, cpuDeltaG, cpuDeltaB float64
	for _, r := range running {
		actual, err := c.observedAcceleratorCost(ctx, r, now)
		if err != nil {
			return false, fmt.Errorf("observed accelerator cost for %s: %w", r.ID, err)
		}
		d := actual - r.EstimatedCostAccH
		if d < 0 {
			d = 0
		}
		var cpuD float64
		if r.EstimatedCPUCoreHours > 0 {
			hours, err := c.observedElapsedHours(ctx, r.ID, now)
			if err != nil {
				return false, fmt.Errorf("observed elapsed hours for %s: %w", r.ID, err)
			}
			if cpuD = hours*r.RequestedCPUCores() - r.EstimatedCPUCoreHours; cpuD < 0 {
				cpuD = 0
			}
		}
		if r.CapacityTier == domain.CapacityGuaranteed {
			accDeltaG += d
			cpuDeltaG += cpuD
		} else {
			accDeltaB += d
			cpuDeltaB += cpuD
		}
	}
	// A zero budget in a dimension is not "no capacity" — a CPU-only experiment has accelerator
	// budget 0 yet real capacity to run. Mirror checkQuotaExhaustion: the researcher has capacity
	// only while *every* tracked, positively-budgeted dimension still has headroom.
	hasHeadroom := func(budget, used, delta float64) bool {
		return budget <= 0 || (used+delta) < budget*0.99
	}
	switch exp.CapacityTier {
	case domain.CapacityGuaranteed:
		return hasHeadroom(aq.GuaranteedAcceleratorHours, aq.UsedGuaranteedAccH, accDeltaG) &&
			hasHeadroom(aq.GuaranteedCPUCoreHours, aq.UsedGuaranteedCPUCoreH, cpuDeltaG), nil
	case domain.CapacityBurst:
		return hasHeadroom(aq.BurstAcceleratorHours, aq.UsedBurstAccH, accDeltaB) &&
			hasHeadroom(aq.BurstCPUCoreHours, aq.UsedBurstCPUCoreH, cpuDeltaB), nil
	}
	return false, fmt.Errorf("unknown capacity tier %q", exp.CapacityTier)
}

// checkOverrun returns true when the experiment has been confirmed running (via
// observedElapsedHours, not wall-clock StartedAt) longer than overrunMultiplier× estimated — a
// job stuck in a reschedule/node-death gap for most of its wall-clock life isn't actually
// overrunning its budget. A query error is logged and treated as "not overrunning" for this
// pass: the next reconcile tick tries again, same as every other observed-time check in this
// file.
func (c *Controller) checkOverrun(ctx context.Context, exp *domain.Experiment, now time.Time) (bool, domain.EvictionReason, error) {
	if exp.EstimatedDurationHours <= 0 {
		return false, "", nil
	}
	hours, err := c.observedElapsedHours(ctx, exp.ID, now)
	if err != nil {
		return false, "", fmt.Errorf("overrun observed elapsed: %w", err)
	}
	if hours > exp.EstimatedDurationHours*c.overrunMultiplier {
		return true, domain.EvictionOverrun, nil
	}
	return false, "", nil
}
