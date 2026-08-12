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
		if !found {
			// No fresh phase report at all — indistinguishable from "cluster-agent itself
			// can't currently observe/report" (e.g. a control-plane <-> cluster connectivity
			// gap). Can't tell whether the pod is actually gone or just unreported; assuming
			// the latter and evicting would kill real, still-running work. Skip and let a
			// later reconcile decide once reporting resumes.
			return false, "", nil
		}
		if phase != workload.JobPhaseRunning {
			// Pod isn't up right now (Pending/recreating/gone) — quiet is expected here,
			// not evidence of a hung process. Let the reschedule finish; next tick re-checks.
			return false, "", nil
		}
		return true, domain.EvictionSilent, nil
	}
	return false, "", nil
}
