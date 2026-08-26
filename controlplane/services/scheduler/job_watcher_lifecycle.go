package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// evictIfPastAdmissionDeadline evicts a job that has stayed pre-RUNNING for longer than
// stuckPendingTimeout, with the reason naming what actually failed to resolve. Called only for
// experiments still in SUBMITTED/ADMITTED, from the one place that has just observed why the
// lifecycle could not advance — the deadline is the bound on every such "still can't tell" state,
// so none of them can hold a reservation indefinitely.
func (w *JobWatcher) evictIfPastAdmissionDeadline(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason) error {
	if w.stuckPendingTimeout <= 0 {
		return nil
	}
	if exp.SubmittedAt == nil {
		// Every SUBMITTED/ADMITTED row is written with submitted_at (ClaimSubmitted sets it in
		// the same UPDATE). Without it there is no deadline to measure against, and silently
		// skipping is what makes a job immortal — surface it instead.
		return fmt.Errorf("experiment %s is %s with no submission time", exp.ID, exp.Status)
	}
	if time.Since(*exp.SubmittedAt) <= w.stuckPendingTimeout {
		return nil
	}
	w.logger.Warn("job_watcher: admission deadline passed, evicting",
		zap.String("id", exp.ID),
		zap.String("reason", string(reason)),
		zap.Duration("timeout", w.stuckPendingTimeout),
	)
	w.evictNotYetRunning(ctx, exp, reason)
	return nil
}

// checkScaleUpDeadline evicts exp if its gang has not fully bound (called only while
// scheduledNodes < Nodes()) past its placement deadline, or if the runtime's latest
// schedulingReason names a terminal scale-up refusal. See autoscaler.md's "Watching" section.
//
// On a cluster reporting autoscaler_enabled, the deadline is scale_up_timeout (per-cluster
// override from cluster_settings, else the global default) and the eviction reason is
// scale_up_timeout — an infrastructure fault recorded as a tried_clusters failover rather than
// spending infra_requeue_count, so a run of slow clusters cannot exhaust a job's environment-fault
// budget. On every other cluster (or when this deployment never called WithScaleUpTimeout), the
// deadline and reason are exactly today's stuckPendingTimeout/EvictionStuckPending.
func (w *JobWatcher) checkScaleUpDeadline(ctx context.Context, exp *domain.Experiment, schedulingReason string, autoscalerEnabled map[string]bool, clusterIDs map[string]string) (evicted bool, err error) {
	if exp.SubmittedAt == nil {
		// Every SUBMITTED/ADMITTED row is written with submitted_at (ClaimSubmitted sets it in
		// the same UPDATE); a RUNNING row keeps whatever the current attempt's submitted_at was.
		return false, fmt.Errorf("experiment %s is %s with no submission time", exp.ID, exp.Status)
	}

	timeout := w.stuckPendingTimeout
	evictionReason := domain.EvictionStuckPending
	triedClusterID := ""
	onAutoscalerCluster := w.scaleUpTimeout > 0 && autoscalerEnabled[exp.ClusterName]
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
			w.evictNotYetRunningWithFailover(ctx, exp, evictionReason, triedClusterID)
			return true, nil
		}
	}
	if timeout <= 0 {
		return false, nil
	}
	if time.Since(*exp.SubmittedAt) <= timeout {
		return false, nil
	}
	w.logger.Warn("job_watcher: placement deadline passed, evicting",
		zap.String("id", exp.ID),
		zap.String("reason", string(evictionReason)),
		zap.Duration("timeout", timeout),
	)
	w.evictNotYetRunningWithFailover(ctx, exp, evictionReason, triedClusterID)
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
	w.evictNotYetRunning(ctx, exp, domain.EvictionUnschedulable)
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

// evictNotYetRunningWithFailover is evictNotYetRunning plus a cluster to record as a speculative
// scale-up failover — see ResolveTermination's triedClusterID doc. Used by the scale-up-timeout
// and flavor_mismatch paths (autoscaler.md), which name the specific cluster that just failed to
// deliver capacity rather than the environment in general.
func (w *JobWatcher) evictNotYetRunningWithFailover(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason, triedClusterID string) {
	outcome, err := w.store.ResolveTermination(ctx, exp.ID, exp.Status, domain.StatusEvicted, string(reason), triedClusterID)
	if err != nil {
		w.logger.Error("job_watcher: evict never-started transition", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	// Settle reads the reason off the experiment, so it has to be the one just persisted rather
	// than whatever the row carried from a previous attempt.
	exp.EvictionReason = string(reason)
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
// failure, exp is left unsettled for services/settlement.Reconciler to retry the same write.
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
	// runningLeftState: it left SUBMITTED/ADMITTED underneath us (cancelled or evicted
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

// onRunning attempts the SUBMITTED/ADMITTED → RUNNING transition, returning the accelerator type
// this admission landed on (empty unless the outcome is runningMarked).
func (w *JobWatcher) onRunning(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, runningOutcome) {
	w.logger.Info("job_watcher: experiment running", zap.String("id", exp.ID))
	var admittedType domain.AcceleratorType
	if exp.AcceleratorCount > 0 {
		var err error
		admittedType, err = w.backend.GetAdmittedAcceleratorType(ctx, exp)
		if err != nil {
			w.logger.Warn("job_watcher: observed accelerator type not yet available, will retry", zap.String("id", exp.ID), zap.Error(err))
			return "", runningTypeUnobservable
		}
		if admittedType != exp.AcceleratorType {
			w.logger.Error("job_watcher: observed accelerator type disagrees with desired flavor",
				zap.String("id", exp.ID), zap.String("desired", string(exp.AcceleratorType)), zap.String("observed", string(admittedType)))
			return "", runningTypeMismatch
		}
	}
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: mark running", zap.String("id", exp.ID), zap.Error(err))
		return "", runningTypeUnobservable
	}
	if !started {
		w.logger.Info("job_watcher: experiment left SUBMITTED/ADMITTED before Running was observed — skipping", zap.String("id", exp.ID))
		return "", runningLeftState
	}

	// A CPU-only job has no accelerator dimension — whatever node it landed on (even an
	// accelerator-labeled one) is not an "admitted flavor" and must not be reverse-mapped into one.
	return admittedType, runningMarked
}

// backfillStartedFromObservations handles a terminal report for an experiment this watcher never
// itself saw reach RUNNING (the poll cadence can miss a job that starts and finishes between two
// ticks — see onFinished). Before treating that as "never ran," check the metrics store for any
// observation tied to this experiment; if one exists, move state through RUNNING first.
func (w *JobWatcher) backfillStartedFromObservations(ctx context.Context, exp *domain.Experiment) (bool, error) {
	if w.metricsDBURL == "" {
		return false, fmt.Errorf("job_watcher: metrics DB URL is required to resolve terminal lifecycle")
	}
	_, observed, err := metricsdb.FirstObserved(ctx, w.metricsDBURL, exp.ID, exp.CreatedAt, time.Now().UTC(), w.observedGapCap)
	if err != nil {
		return false, fmt.Errorf("job_watcher: check observations for terminal job %s: %w", exp.ID, err)
	}
	if !observed {
		return false, nil
	}
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: backfill running state", zap.String("id", exp.ID), zap.Error(err))
		return false, fmt.Errorf("job_watcher: backfill running state for %s: %w", exp.ID, err)
	}
	if !started {
		// Already left SUBMITTED/ADMITTED by some other path — nothing to backfill.
		return false, nil
	}
	w.logger.Warn("job_watcher: backfilling lifecycle state, metrics show it ran", zap.String("id", exp.ID))
	exp.Status = domain.StatusRunning
	return true, nil
}

// onFinished handles the RUNNING → COMPLETED/FAILED transition and settles quota.
func (w *JobWatcher) onFinished(ctx context.Context, exp *domain.Experiment, succeeded bool) error {
	status := domain.StatusCompleted
	if !succeeded {
		status = domain.StatusFailed
	}
	w.logger.Info("job_watcher: experiment finished",
		zap.String("id", exp.ID),
		zap.String("status", string(status)),
	)

	if exp.Status != domain.StatusRunning {
		if _, err := w.backfillStartedFromObservations(ctx, exp); err != nil {
			return err
		}
	}

	// If seen as RUNNING, use a conditional transition so a concurrent controller eviction can
	// only win once; if we lose the race, skip the refund — the controller already issued it.
	if exp.Status == domain.StatusRunning {
		updated, err := w.store.TransitionStatus(ctx, exp.ID, domain.StatusRunning, status)
		if err != nil {
			w.logger.Error("job_watcher: transition status", zap.String("id", exp.ID), zap.Error(err))
			return err
		}
		if !updated {
			// Controller evicted it concurrently — refund already issued, nothing to do.
			return nil
		}
	} else {
		// Completed without this cycle observing a RUNNING poll — usually a true ADMITTED →
		// terminal transition, but a pod recreation can also land a genuinely-RUNNING job here.
		// Guard with the non-terminal check, or a late completion from an already-evicted job's
		// stale pod could resurrect it past a terminal state.
		updated, err := w.store.TransitionStatusFromNonTerminal(ctx, exp.ID, status)
		if err != nil {
			w.logger.Error("job_watcher: update status", zap.String("id", exp.ID), zap.Error(err))
			return err
		}
		if !updated {
			// Already terminal (e.g. controller evicted it concurrently) — nothing to do.
			return nil
		}
	}

	if !succeeded && w.retryGang(ctx, exp) {
		return nil
	}

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
		// Budget exhausted, or another watcher requeued it first. Either way this call has
		// nothing left to do; when the budget is what ran out, the row stays FAILED and the
		// caller settles it.
		return false
	}
	w.logger.Info("job_watcher: gang failed, requeued for a fresh attempt",
		zap.String("id", exp.ID),
		zap.Int("attempts_used", exp.RetriesUsed()+1),
		zap.Int("max_retries", *exp.Job.MaxRetries),
	)
	return true
}
