package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/obsmetrics"
)

// startOverdue is how long exp has been admitted beyond its own checkpoint window. A requeued job
// can be re-admitted while its previous generation is still inside that window, and the runtime
// creates the new workload only once the old one is gone, so the window does not count against
// the time a job has to start.
func startOverdue(exp *domain.Experiment) time.Duration {
	window := int64(0)
	if grace := exp.EffectiveJob().CheckpointGraceSeconds; grace != nil {
		window = *grace
	}
	return time.Since(*exp.SubmittedAt) - time.Duration(window)*time.Second
}

// stuckPendingOverdue is how far past timeout the wait of a job that has not landed has run.
// waiting is what its runtime reports it waits for, and the wait counts from the runtime's
// first report of that in this attempt: a runtime that rents machines says first that it waits
// for one and then that the one it rented is starting, each a wait of its own, and a deadline
// counted from admission through both ends a machine that was found late while it starts. The
// first report of a reason never moves, so a job gets one timeout for each thing it waits for,
// however it goes between them. A runtime that reports no reason is judged from admission.
//
// The first report is read only for a job already past the deadline counted from admission.
func (w *JobWatcher) stuckPendingOverdue(ctx context.Context, exp *domain.Experiment, waiting domain.PhaseDetail, timeout time.Duration) (time.Duration, error) {
	overdue := startOverdue(exp) - timeout
	if overdue <= 0 || waiting.Reason == "" {
		return overdue, nil
	}
	since, found, err := w.reports.PhaseReportedSince(ctx, exp, waiting.Reason)
	if err != nil {
		return 0, fmt.Errorf("first report of %s: %w", waiting.Reason, err)
	}
	if !found {
		return overdue, nil
	}
	return min(overdue, time.Since(since)-timeout), nil
}

// evictIfPastDeadline evicts a job that has stayed pre-RUNNING for longer than timeout, with the
// reason naming what actually failed to resolve. Called only for experiments still in SUBMITTED,
// from the one place that has just observed why the lifecycle could not advance, so none of those
// states can hold a reservation indefinitely.
func (w *JobWatcher) evictIfPastDeadline(ctx context.Context, exp *domain.Experiment, waiting domain.PhaseDetail, timeout time.Duration, reason domain.EvictionReason) error {
	if timeout <= 0 {
		return nil
	}
	if exp.SubmittedAt == nil {
		// Every SUBMITTED row is written with submitted_at (ClaimSubmitted sets it in
		// the same UPDATE). Without it there is no deadline to measure against, and silently
		// skipping is what makes a job immortal — surface it instead.
		return fmt.Errorf("experiment %s is %s with no submission time", exp.ID, exp.Status)
	}
	overdue, err := w.stuckPendingOverdue(ctx, exp, waiting, timeout)
	if err != nil {
		return err
	}
	if overdue <= 0 {
		return nil
	}
	w.logger.Warn("job_watcher: admission deadline passed, evicting",
		zap.String("id", exp.ID),
		zap.String("reason", string(reason)),
		zap.Duration("timeout", timeout),
	)
	w.evictNotYetRunning(ctx, exp, reason.WithDetail(notRunningInTime(timeout, waiting)))
	return nil
}

// notRunningInTime is the sentence of an eviction at the admission deadline: how long the job was
// given, and what its runtime last said it waited for.
func notRunningInTime(timeout time.Duration, waiting domain.PhaseDetail) string {
	said := waiting.Said()
	if said == "" {
		return fmt.Sprintf("admitted and not running within %s", timeout)
	}
	return fmt.Sprintf("admitted and not running within %s, its runtime reporting %s", timeout, said)
}

// errClusterNotReporting is why a stuck-pending decision is not made this pass: the job's cluster
// has no id among the ones the pass read, so its timeout cannot be known.
var errClusterNotReporting = errors.New("the job's cluster reports no id this pass")

// stuckPendingTimeoutFor is the stuck-pending deadline of the cluster exp was admitted to: the
// value its operator set in the cluster's settings, and the global one where none is set. The settings
// row is read at the moment a stuck-pending decision is made and nowhere else, so a job with no
// such decision pending is never held up by a read that fails.
//
// clusterIDs is the pass's one read of which connected cluster reports which id. Settings are
// keyed by that id, so for a cluster without one, because it has no fresh heartbeat or because
// the read failed, the timeout is unknown and the answer is errClusterNotReporting. The global
// value is not a substitute: it would evict a job its cluster gives longer to. A cluster that
// stays unreachable is the controller's to act on.
func (w *JobWatcher) stuckPendingTimeoutFor(ctx context.Context, exp *domain.Experiment, clusterIDs map[string]string) (time.Duration, error) {
	clusterID := clusterIDs[exp.ClusterName]
	if clusterID == "" {
		return 0, errClusterNotReporting
	}
	cs, err := w.store.GetClusterSettings(ctx, clusterID)
	if err != nil {
		return 0, fmt.Errorf("cluster settings: %w", err)
	}
	if cs != nil && cs.StuckPendingTimeoutSeconds != nil {
		return time.Duration(*cs.StuckPendingTimeoutSeconds) * time.Second, nil
	}
	return w.stuckPendingTimeout, nil
}

// evictIfStuckPending evicts exp once it has stayed pre-RUNNING past its cluster's stuck-pending
// timeout. A timeout that cannot be read is an error and no eviction: the global value might
// evict a job its cluster gives longer to. A cluster that reports no id this pass is no error,
// the decision waits for a pass in which it does.
func (w *JobWatcher) evictIfStuckPending(ctx context.Context, exp *domain.Experiment, waiting domain.PhaseDetail, clusterIDs map[string]string, reason domain.EvictionReason) error {
	timeout, err := w.stuckPendingTimeoutFor(ctx, exp, clusterIDs)
	if errors.Is(err, errClusterNotReporting) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stuck-pending timeout: %w", err)
	}
	return w.evictIfPastDeadline(ctx, exp, waiting, timeout, reason)
}

// checkScaleUpDeadline evicts exp if its gang has not fully bound (called only while
// scheduledNodes < Nodes()) past its placement deadline, or if the runtime's latest
// schedulingReason names a terminal scale-up refusal. See autoscaler.md's "Watching" section.
//
// On a cluster reporting autoscaler_enabled, the deadline is scale_up_timeout (per-cluster
// override from the cluster's settings, else the global default) and the eviction reason is
// scale_up_timeout — an infrastructure fault like any other: the job is requeued on its
// infrastructure allowance and the cluster goes into its tried list, so it is placed elsewhere
// next and cannot go round slow clusters for ever. On every other cluster (or when this deployment never called WithScaleUpTimeout), the
// deadline is that cluster's stuck-pending timeout (stuckPendingTimeoutFor), counted from when
// the wait the runtime reports began (stuckPendingOverdue), and the reason is
// EvictionStuckPending.
func (w *JobWatcher) checkScaleUpDeadline(ctx context.Context, exp *domain.Experiment, waiting domain.PhaseDetail, schedulingReason string, autoscalerEnabled map[string]bool, clusterIDs map[string]string) (evicted bool, err error) {
	if exp.SubmittedAt == nil {
		// Every SUBMITTED row is written with submitted_at (ClaimSubmitted sets it in
		// the same UPDATE); a RUNNING row keeps whatever the current attempt's submitted_at was.
		return false, fmt.Errorf("experiment %s is %s with no submission time", exp.ID, exp.Status)
	}

	var timeout time.Duration
	evictionReason := domain.EvictionStuckPending
	triedClusterID := ""
	// clusterIDs[exp.ClusterName] can be legitimately empty either because this cluster isn't
	// autoscaler-enabled, or because GetClusterIDs failed this pass while GetAutoscalerCapability
	// still succeeded (the two are fetched independently in scanAndWatch). The two failure modes
	// must not be conflated: treating the latter as "not an autoscaler cluster" would judge a
	// scale-up by the stuck-pending deadline and name the wrong reason.
	onAutoscalerCluster := autoscalerEnabled[exp.ClusterName] && clusterIDs[exp.ClusterName] != ""
	if onAutoscalerCluster {
		timeout = w.scaleUpTimeout
		evictionReason = domain.EvictionScaleUpTimeout
		triedClusterID = clusterIDs[exp.ClusterName]
		cs, err := w.store.GetClusterSettings(ctx, triedClusterID)
		if err != nil {
			return false, err
		}
		if cs != nil && cs.ScaleUpTimeoutSeconds != nil {
			timeout = time.Duration(*cs.ScaleUpTimeoutSeconds) * time.Second
		}

		// Early exit: the autoscaler itself already said no, so waiting out the rest of the
		// deadline only spends time — "the deadline is the guarantee, the event is the
		// accelerator" (autoscaler.md). Best-effort string match; a schedulingReason that doesn't
		// match anything just falls through to the deadline, same as no reason at all.
		if isTerminalSchedulingRefusal(schedulingReason) {
			w.logger.Warn("job_watcher: autoscaler refused to scale up, evicting early",
				zap.String("id", exp.ID), zap.String("cluster_id", triedClusterID),
				zap.String("scheduling_reason", schedulingReason))
			w.evictNotYetRunningWithFailover(ctx, exp, evictionReason.WithDetail("the autoscaler refused to scale up: "+schedulingReason), triedClusterID)
			return true, nil
		}
	} else {
		var err error
		timeout, err = w.stuckPendingTimeoutFor(ctx, exp, clusterIDs)
		if errors.Is(err, errClusterNotReporting) {
			return false, nil
		}
		if err != nil {
			// This decision is skipped and the rest of the job's reconcile goes on: a job that
			// has in fact finished must still be read as finished.
			w.logger.Warn("job_watcher: stuck-pending timeout unreadable, placement deadline not judged this pass",
				zap.String("id", exp.ID), zap.Error(err))
			return false, nil
		}
	}
	if timeout <= 0 {
		return false, nil
	}
	overdue := startOverdue(exp) - timeout
	if !onAutoscalerCluster {
		var err error
		if overdue, err = w.stuckPendingOverdue(ctx, exp, waiting, timeout); err != nil {
			w.logger.Warn("job_watcher: start of the reported wait unreadable, placement deadline not judged this pass",
				zap.String("id", exp.ID), zap.Error(err))
			return false, nil
		}
	}
	if overdue <= 0 {
		return false, nil
	}
	w.logger.Warn("job_watcher: placement deadline passed, evicting",
		zap.String("id", exp.ID),
		zap.String("reason", string(evictionReason)),
		zap.Duration("timeout", timeout),
	)
	w.evictNotYetRunningWithFailover(ctx, exp, evictionReason.WithDetail(notRunningInTime(timeout, waiting)), triedClusterID)
	return true, nil
}

// isTerminalSchedulingRefusal reports whether schedulingReason names a scale-up attempt the
// autoscaler has already given up on, rather than one still in flight. Deliberately a small,
// best-effort set of substrings covering cluster-autoscaler and Karpenter's own refusal
// vocabulary — an unrecognised reason (including "TriggeredScaleUp", "still waiting", or empty)
// falls through to the deadline, never the other way around: missing a refusal costs one bounded
// wait, misreading "still trying" as "gave up" would evict work the autoscaler was about to land.
func isTerminalSchedulingRefusal(schedulingReason string) bool {
	if schedulingReason == "" {
		return false
	}
	reason := strings.ToLower(schedulingReason)
	for _, marker := range []string{
		"nottriggerscaleup",
		"max node group size reached",
		"incompatible with nodepool",
		"nodeclaim failure",
	} {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	return false
}

// onUnschedulable evicts a job whose runtime reported a phase-detail reason in
// domain.NeverSelfHealsPhaseReasons — a bad image or a malformed container config that will
// never resolve itself — well before the generic stuckPendingTimeout would otherwise catch it.
// Checked regardless of exp.Status (see reconcileOne): a Kubernetes Job counts as "Active",
// and so as RUNNING here, from the moment its pod exists, even while that pod is stuck in
// ImagePullBackOff and its container has never actually started.
func (w *JobWatcher) onUnschedulable(ctx context.Context, exp *domain.Experiment, reason, message string) {
	w.logger.Warn("job_watcher: never-self-heals phase detail, evicting",
		zap.String("id", exp.ID),
		zap.String("phase_reason", reason),
		zap.String("phase_message", message),
	)
	w.evictNotYetRunning(ctx, exp, domain.PhaseDetail{Reason: reason, Message: message}.EvictionOfNeverStarting())
}

// evictNotYetRunning ends a job the lifecycle never advanced to RUNNING, whatever prevented it.
// Settle computes observed usage from real metrics, so this zeroes out to a full refund when
// nothing genuinely ran and still bills a job whose pod was up (unreadable accelerator type) for
// exactly what it burned — except when the environment is what failed it, which the store
// resolves into a free requeue while the allowance lasts and, once it runs out, into an EVICTED
// row that Settle refunds in full. The decision is not repeated here: this function reports
// whichever outcome ResolveTermination produced.
func (w *JobWatcher) evictNotYetRunning(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason) {
	w.evictNotYetRunningWithFailover(ctx, exp, reason, "")
}

// evictNotYetRunningWithFailover is evictNotYetRunning counted as a failover when triedClusterID
// names the cluster that just failed to deliver capacity. Used by the scale-up-timeout and
// flavor_mismatch paths (autoscaler.md). The store records the cluster in the job's tried list
// itself, for every infrastructure fault.
func (w *JobWatcher) evictNotYetRunningWithFailover(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason, triedClusterID string) {
	if triedClusterID != "" {
		obsmetrics.FailoversTotal.WithLabelValues(string(reason.Code())).Inc()
	}
	outcome, err := w.store.ResolveTermination(ctx, exp.ID, exp.Status, domain.StatusEvicted, string(reason))
	if err != nil {
		w.logger.Error("job_watcher: evict never-started transition", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	// Settle reads the reason off the experiment, so it has to be the one just persisted rather
	// than whatever the row carried from a previous attempt.
	exp.EvictionReason = string(reason)
	if outcome != domain.TerminationSkipped {
		w.wakeAdmission()
	}
	switch outcome {
	case domain.TerminationSkipped:
		// Already moved on (e.g. finished right before this check) — nothing to do.
		return
	case domain.TerminationRequeued:
		// Bill what this attempt burned before it goes back in the queue, so a job cycling
		// through requeues is not invisible to running-cost and quota exhaustion meanwhile. Not
		// the refund: that lands only if the job ends in an infrastructure fault once its
		// requeue allowance runs out — see settlement.Settle. Deliberately not MarkQuotaSettled,
		// since the row is QUEUED again rather than terminal.
		exp.Status = domain.StatusQueued
		if err := w.settler.Settle(ctx, exp); err != nil {
			w.logger.Warn("job_watcher: settle before infrastructure requeue", zap.String("id", exp.ID), zap.Error(err))
		}
		w.logger.Info("job_watcher: infrastructure fault, requeued at no cost",
			zap.String("id", exp.ID), zap.String("reason", string(reason)))
		return
	}
	obsmetrics.CountEviction(reason)
	exp.Status = domain.StatusEvicted
	w.settleQuota(ctx, exp)
}

// settleQuota durably writes exp's final observed usage and marks it settled on success. On
// failure, exp is left unsettled for service/settlement.Reconciler to retry the same write.
func (w *JobWatcher) settleQuota(ctx context.Context, exp *domain.Experiment) {
	if err := w.settler.Settle(ctx, exp); err != nil {
		w.logger.Warn("job_watcher: settle quota", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	if err := w.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		w.logger.Error("job_watcher: mark quota settled", zap.String("id", exp.ID), zap.Error(err))
	}
}

// runningOutcome is what one attempt to advance a job to RUNNING concluded. onRunning reports
// only what it observed; reconcileOne owns what to do about it, so no caller has to infer policy
// from a bare bool or a generic error.
type runningOutcome int

const (
	// runningMarked: the experiment is now RUNNING.
	runningMarked runningOutcome = iota
	// runningLeftState: it left SUBMITTED underneath us (cancelled or evicted
	// concurrently). Nothing to do — and nothing to bound, since it is already terminal.
	runningLeftState
	// runningTypeUnobservable: the pod is up but its accelerator type is not readable. Usually a
	// sample or two behind and resolves on the next tick, so it is retried — but it is a
	// "can't tell" state and therefore needs a deadline.
	runningTypeUnobservable
	// runningTypeMismatch: the pod is up on an accelerator type other than the one reserved and
	// billed. Already-happened placement, so waiting cannot fix it.
	runningTypeMismatch
)

// onRunning attempts the SUBMITTED → RUNNING transition, returning the accelerator type
// this admission landed on (empty unless the outcome is runningMarked).
func (w *JobWatcher) onRunning(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, runningOutcome) {
	w.logger.Info("job_watcher: experiment running", zap.String("id", exp.ID))
	var admittedType domain.AcceleratorType
	if exp.AcceleratorCount > 0 {
		observedType, observed, err := w.reports.GetAdmittedAcceleratorType(ctx, exp)
		if err != nil {
			w.logger.Error("job_watcher: read observed accelerator type", zap.String("id", exp.ID), zap.Error(err))
			return "", runningTypeUnobservable
		}
		if !observed {
			return "", runningTypeUnobservable
		}
		admittedType = observedType
		if !admittedType.Is(exp.AcceleratorType) {
			w.logger.Error("job_watcher: observed accelerator type disagrees with desired accelerator type",
				zap.String("id", exp.ID), zap.String("desired", string(exp.AcceleratorType)), zap.String("observed", string(admittedType)))
			return "", runningTypeMismatch
		}
		// The same type in the runtime's casing is the type the job asked for and is billed as.
		admittedType = exp.AcceleratorType
	}
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: mark running", zap.String("id", exp.ID), zap.Error(err))
		return "", runningTypeUnobservable
	}
	if !started {
		w.logger.Info("job_watcher: experiment left SUBMITTED before Running was observed — skipping", zap.String("id", exp.ID))
		return "", runningLeftState
	}

	// A CPU-only job has no accelerator dimension — whatever node it landed on (even an
	// accelerator-labeled one) is not an "admitted accelerator type" and must not be reverse-mapped into one.
	return admittedType, runningMarked
}

// onFinished handles the RUNNING → COMPLETED/FAILED transition and settles quota. reason is what
// the runtime said of a failed job's process, and is written with the status.
func (w *JobWatcher) onFinished(ctx context.Context, exp *domain.Experiment, status domain.ExperimentStatus, reason string) error {
	w.logger.Info("job_watcher: experiment finished",
		zap.String("id", exp.ID),
		zap.String("status", string(status)),
		zap.String("reason", reason),
	)

	// A process that ended ran. The poll cadence misses the RUNNING of a job that starts and
	// ends between two reports, and a runtime that reads its machines' state from a vendor may
	// never see one, so a terminal report for a job still SUBMITTED moves it through RUNNING
	// first: no job is COMPLETED or FAILED without having been RUNNING, and the type it ran on
	// is recorded (MarkStarted).
	//
	// A job that left SUBMITTED some other way is ended only if that way made it RUNNING. One
	// that was requeued or has ended is left as it is: the report is of an attempt that is over,
	// and ending a QUEUED job on it settled quota for a job still to run.
	if exp.Status != domain.StatusRunning {
		started, err := w.store.MarkStarted(ctx, exp.ID)
		if err != nil {
			return fmt.Errorf("job_watcher: mark %s running before its end: %w", exp.ID, err)
		}
		if !started {
			current, err := w.store.GetExperiment(ctx, exp.ID)
			if err != nil {
				return fmt.Errorf("job_watcher: read %s after it left SUBMITTED: %w", exp.ID, err)
			}
			if current == nil || current.Status != domain.StatusRunning {
				w.logger.Info("job_watcher: experiment left SUBMITTED before its end was reported, the report is of an attempt that is over", zap.String("id", exp.ID))
				return nil
			}
		}
		exp.Status = domain.StatusRunning
	}

	// A gang with retry budget goes straight back to QUEUED. Writing FAILED first would announce
	// a terminal status for a job that runs again.
	if status == domain.StatusFailed && w.retryGang(ctx, exp) {
		return nil
	}

	// A conditional transition, so a concurrent controller eviction can only win once; if we lose
	// the race, skip the refund: the controller already issued it.
	updated, err := w.store.TransitionStatus(ctx, exp.ID, domain.StatusRunning, status, reason)
	if err != nil {
		w.logger.Error("job_watcher: transition status", zap.String("id", exp.ID), zap.Error(err))
		return err
	}
	if !updated {
		return nil
	}
	w.wakeAdmission()

	// Durably settle each resource dimension's observed cost. See settleQuota's doc comment for
	// the crash/outage retry story.
	w.settleQuota(ctx, exp)
	return nil
}

// retryGang returns a failed multi-node job to QUEUED for another whole-gang attempt while its
// max_retries budget lasts, reporting whether it did.
//
// Only gangs. For a single-pod job the runtime already expresses "restart the failed unit" as
// BackoffLimit, and by the time the job watcher sees JobPhaseFailed that budget is spent — so a
// control-plane retry there would be a second retry authority stacked on the first. A gang is the
// case the runtime cannot express: k8s can recreate a failed index, which strands the survivors,
// or fail the whole Job, which is terminal. Neither is "restart the gang", so the decision moves
// to the only layer that can make it, and is written as desired state like every other.
func (w *JobWatcher) retryGang(ctx context.Context, exp *domain.Experiment) bool {
	if exp.Job.Nodes() < 2 || exp.Job.MaxRetries == nil || *exp.Job.MaxRetries < 1 {
		return false
	}
	// Bill what this attempt burned before it goes back in the queue, so a job retrying for
	// minutes is not invisible to running-cost and quota exhaustion in the meantime. Deliberately
	// not MarkQuotaSettled: the row stops being terminal, and the final attempt settles for real.
	if err := w.settler.Settle(ctx, exp); err != nil {
		w.logger.Warn("job_watcher: settle before gang retry", zap.String("id", exp.ID), zap.Error(err))
	}
	requeued, err := w.store.RequeueForRetry(ctx, exp.ID, *exp.Job.MaxRetries)
	if err != nil {
		w.logger.Error("job_watcher: requeue gang for retry", zap.String("id", exp.ID), zap.Error(err))
		return false
	}
	if !requeued {
		// Budget exhausted, or the row already left SUBMITTED/RUNNING. The caller writes the
		// terminal status if it still can.
		return false
	}
	w.wakeAdmission()
	w.logger.Info("job_watcher: gang failed, requeued for a fresh attempt",
		zap.String("id", exp.ID),
		zap.Int("attempts_used", exp.RetriesUsed()+1),
		zap.Int("max_retries", *exp.Job.MaxRetries),
	)
	return true
}
