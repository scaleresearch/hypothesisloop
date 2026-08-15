package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

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
//
// A live pod is not, by itself, proof of progress: declaredMetricKeys (the PE's own metric
// contract) lets this also catch a job whose training loop hung but keeps re-emitting the same
// constant value — see metricsdb.AnyDeclaredMetricChanged. That check gets double the plain
// silence window, since a legitimately coarse eval cadence (metrics only every few report
// intervals) must not be mistaken for a stuck process.
func (c *Controller) checkSilence(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration, declaredMetricKeys []string) (bool, domain.EvictionReason, error) {
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
	phase, found, err := metricsdb.LatestJobPhase(ctx, c.metricsDBURL, exp.ID, exp.ClusterName, window)
	if err != nil {
		return false, "", fmt.Errorf("silence job phase: %w", err)
	}
	if !found {
		// No fresh phase report at all — indistinguishable from "cluster-agent itself
		// can't currently observe/report" (e.g. a control-plane <-> cluster connectivity
		// gap). Can't tell whether the pod is actually gone or just unreported; assuming
		// the latter and evicting would kill real, still-running work. Skip and let a
		// later reconcile decide once reporting resumes.
		return false, "", nil
	}
	if phase == workload.JobPhaseGone {
		// A fresh, complete cluster snapshot that doesn't mention this job at all — the
		// runtime's own confirmation no live pod/container exists for it (see
		// metricsdb.LatestJobPhase). Unlike Pending, this cannot resolve itself by waiting:
		// nothing is converging. Most commonly a host reboot that wiped a bare-metal executor's
		// container state out from under a RUNNING job, permanently stranding its quota until a
		// human notices. Refund and terminate through the same path every other reason uses.
		// Takes priority over any stale liveness/metric samples still inside the window below.
		return true, domain.EvictionWorkloadGone, nil
	}
	if phase != workload.JobPhaseRunning {
		// Pending/recreating — quiet is expected here, not evidence of a hung process. Let the
		// reschedule finish; next tick re-checks. (Succeeded/Failed are terminalized by the
		// job watcher's own faster poll, not here — one path per outcome.)
		return false, "", nil
	}

	alive, err := c.isAlive(ctx, exp.ID, window)
	if err != nil {
		return false, "", fmt.Errorf("silence liveness: %w", err)
	}
	if alive {
		// A live pod is not, by itself, proof of progress: a job whose training loop hung but
		// keeps re-emitting the same constant value would otherwise look perpetually alive to
		// presence-only detection. Gated on the confirmed-Running phase above, so this can never
		// fire mid-reschedule or against stale samples from a job that's already Gone/finished.
		if len(declaredMetricKeys) == 0 {
			return false, "", nil
		}
		if now.Sub(startedAt) < 2*window {
			return false, "", nil // give a coarse eval cadence room to post its first point
		}
		reported, changed, err := metricsdb.AnyDeclaredMetricChanged(ctx, c.metricsDBURL, exp.ID, declaredMetricKeys, 2*window)
		if err != nil {
			return false, "", fmt.Errorf("silence declared-metric progress: %w", err)
		}
		if !reported || changed {
			// Not reported yet, or too few samples to judge: still warming up, or its own
			// reporting is broken — the latter is exactly what checkQuotaExhaustion/
			// stuck-pending bounds, not this check.
			return false, "", nil
		}
		return true, domain.EvictionSilent, nil
	}
	// Same silence, two very different causes. A job that reported and then stopped has a
	// hung or dead training process; one that never reported at all has a reporting path
	// that never worked — and because workloads typically swallow the post failure and only
	// warn to stderr, that failure is otherwise completely silent, reaching an operator as
	// "stuck job" with no hint the metrics path is at fault. Tell them apart here, where the
	// evidence is, so the eviction reason names the actual problem.
	reported, err := metricsdb.HasEverReportedMetric(ctx, c.metricsDBURL, exp.ID, c.observedMaxLookback())
	if err != nil {
		return false, "", fmt.Errorf("silence ever-reported: %w", err)
	}
	if !reported {
		return true, domain.EvictionNeverReportedMetrics, nil
	}
	return true, domain.EvictionSilent, nil
}
