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
	startedAt, observed, err := c.observed.FirstObserved(ctx, exp.ID, exp.CreatedAt, now, c.observedGapCap())
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
	phase, found, err := c.observed.LatestJobPhase(ctx, exp.ID, exp.ClusterName, window)
	if err != nil {
		return false, "", fmt.Errorf("silence job phase: %w", err)
	}
	// Absence has two completely different causes, and they must be told apart before either can
	// be acted on: the cluster is not reporting (nothing can be concluded about this job), or the
	// cluster is reporting and this job is not in what it reports. Both branches need the same
	// snapshot facts, and only these branches do — the common case above never pays for the query.
	if !found || phase == workload.JobPhaseGone {
		presence, err := c.observed.ClusterSnapshotPresence(ctx, exp.ID, exp.ClusterName, exp.CreatedAt, now)
		if err != nil {
			return false, "", fmt.Errorf("silence snapshot presence: %w", err)
		}
		if !presence.Reported || presence.SnapshotAge > c.clusterSilenceCeiling {
			// The cluster itself has gone quiet. Whether this job is alive is genuinely unknown
			// and stays unknown until reporting resumes, so waiting produces no better evidence —
			// only an ever-growing pile of reservations held against a cluster nobody can see.
			return true, domain.EvictionClusterUnreachable, nil
		}
		if !found {
			// The cluster is reporting, just not freshly enough for this job's silence window.
			// Not a verdict on the job; the next pass re-reads.
			return false, "", nil
		}
		if presence.AbsentSnapshots < metricsdb.GoneConfirmingSnapshots {
			// Missing from the newest snapshot but present in the one before it. That is exactly
			// what a routine drift-delete-then-recreate looks like from here, and it resolves
			// itself within a snapshot or two. Wait for the absence to be confirmed.
			return false, "", nil
		}
		// Consecutive complete snapshots from a live cluster, none of which mention this job —
		// the runtime's own confirmation that no pod/container exists for it. Unlike Pending this
		// cannot resolve itself by waiting: nothing is converging. Most commonly a host reboot
		// that wiped a bare-metal executor's container state out from under a RUNNING job,
		// permanently stranding its quota until a human notices. Refund and terminate through the
		// same path every other reason uses. Takes priority over any stale liveness/metric
		// samples still inside the window below.
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
		reported, changed, err := c.observed.AnyDeclaredMetricChanged(ctx, exp.ID, declaredMetricKeys, 2*window)
		if err != nil {
			return false, "", fmt.Errorf("silence declared-metric progress: %w", err)
		}
		if changed {
			return false, "", nil // moving, which is the whole point
		}
		if reported {
			// Reporting, but every declared metric held one constant value across the window.
			// reported=true means at least two samples (see declaredMetricSpread — a single point
			// reads as not-reported precisely because it cannot show movement), so this is a job
			// re-emitting a cached value with its training loop hung, not one still warming up.
			return true, domain.EvictionSilent, nil
		}
		// Nothing in the window. That is two different jobs: one that reported earlier and went
		// quiet, and one whose reporting path never worked at all. Only the second is decided
		// here — ask over the job's whole life rather than this window, or a job that reported
		// fine an hour ago gets condemned as if it never reported. Asked with the "even one
		// sample" reader, not the progress one: a single point is not enough to judge movement,
		// but it is a complete answer to "did this job ever report".
		everReported, err := c.observed.AnyDeclaredMetricReported(ctx, exp.ID, declaredMetricKeys, now.Sub(startedAt))
		if err != nil {
			return false, "", fmt.Errorf("silence declared-metric ever-reported: %w", err)
		}
		if everReported {
			return false, "", nil
		}
		// Alive, past its grace period, and has never once emitted a metric its own platform
		// experiment declared. It cannot be ranked, cut, or compared — there is nothing to judge
		// it by — while it holds an accelerator and bills for it. The reporting path is broken
		// (wrong URL, a stale helper baked into the image, a swallowed exception), and no amount
		// of further running fixes that. The grace above is 2x the silence window, i.e. six times
		// the platform experiment's own declared reporting cadence: a job that has not reported
		// in six of its own intervals is not slow, it is not reporting.
		return true, domain.EvictionNeverReportedMetrics, nil
	}
	// Same silence, two very different causes. A job that reported and then stopped has a
	// hung or dead training process; one that never reported at all has a reporting path
	// that never worked — and because workloads typically swallow the post failure and only
	// warn to stderr, that failure is otherwise completely silent, reaching an operator as
	// "stuck job" with no hint the metrics path is at fault. Tell them apart here, where the
	// evidence is, so the eviction reason names the actual problem.
	reported, err := c.observed.HasEverReportedMetric(ctx, exp.ID, exp.CreatedAt, now)
	if err != nil {
		return false, "", fmt.Errorf("silence ever-reported: %w", err)
	}
	if !reported {
		return true, domain.EvictionNeverReportedMetrics, nil
	}
	return true, domain.EvictionSilent, nil
}
