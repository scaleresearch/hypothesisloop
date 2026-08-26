package scheduler

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Start repeatedly derives lifecycle work from PostgreSQL desired state and the latest backend
// observation. No per-experiment goroutine or transition history survives a pass.
func (w *JobWatcher) Start(ctx context.Context) {
	if w.pollInterval <= 0 {
		panic("job_watcher: poll interval must be positive")
	}
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		if err := w.scanAndWatch(ctx); err != nil {
			w.logger.Error("job_watcher: reconcile", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// scanAndWatch is retained as the package's reconciliation entry point. Each call is a complete
// stateless pass over every desired-running experiment. A single experiment's reconcile error
// (e.g. a stuck job whose backend poll fails) must not stop the pass from reaching the rest.
func (w *JobWatcher) scanAndWatch(ctx context.Context) error {
	statuses := []domain.ExperimentStatus{
		domain.StatusSubmitted,
		domain.StatusAdmitted,
		domain.StatusRunning,
	}
	// Speculative-submit inputs (autoscaler.md), read only when a deployment has opted in via
	// WithScaleUpTimeout — mirrors Loop.tick's own opt-in gate for the same reason: a backend
	// that has not implemented these two methods must not be asked to. A fetch failure degrades
	// this pass to today's stuckPendingTimeout-only behaviour rather than aborting the whole
	// scan — an eviction pass for every other experiment must survive one infrastructure hiccup.
	var autoscalerEnabled map[string]bool
	var clusterIDs map[string]string
	if w.scaleUpTimeout > 0 {
		var err error
		autoscalerEnabled, err = w.backend.GetAutoscalerCapability(ctx)
		if err != nil {
			w.logger.Warn("job_watcher: autoscaler capability unavailable, deferring to stuck_pending timeout this pass", zap.Error(err))
			autoscalerEnabled = nil
		}
		clusterIDs, err = w.backend.GetClusterIDs(ctx)
		if err != nil {
			w.logger.Warn("job_watcher: cluster ids unavailable, deferring to stuck_pending timeout this pass", zap.Error(err))
			clusterIDs = nil
		}
	}
	for _, status := range statuses {
		exps, err := w.store.ListExperimentsWithStatus(ctx, status)
		if err != nil {
			return err
		}
		for _, exp := range exps {
			if err := w.reconcileOne(ctx, exp, autoscalerEnabled, clusterIDs); err != nil {
				w.logger.Error("job_watcher: reconcile experiment",
					zap.String("experiment_id", exp.ID), zap.Error(err))
			}
		}
	}
	return nil
}

func (w *JobWatcher) reconcileOne(ctx context.Context, exp *domain.Experiment, autoscalerEnabled map[string]bool, clusterIDs map[string]string) error {
	// Checked regardless of exp.Status, including RUNNING: a Kubernetes Job's Active count
	// (what PollJobPhase treats as Running, see k8sexec.PollJobPhaseAndUID) includes a pod
	// stuck in ImagePullBackOff — the desired-state Job "exists and isn't finished" long
	// before its container ever actually starts. A never-self-heals reason means it never
	// will, whatever exp.Status currently says.
	reason, message, _, scheduledNodes, schedulingReason, found, err := w.backend.PollPhaseDetail(ctx, exp)
	if err != nil {
		return fmt.Errorf("poll phase detail: %w", err)
	}
	if found && domain.NeverSelfHealsPhaseReasons[reason] {
		w.onUnschedulable(ctx, exp, reason, message)
		return nil
	}

	// Placement-deadline check, run before the phase switch and independent of it: a gang whose
	// pods haven't all bound is the same "still can't tell / still waiting on capacity" state
	// whether the aggregate phase currently reads Pending, Gone, or (one landed rank) Running —
	// see autoscaler.md's "Gang scheduling" section. Skipped once every rank has bound
	// (scheduledNodes >= Nodes()), which is also true for every non-gang job whose one pod has
	// bound — the general case degenerating, not a special one.
	// Gated on found: no phase-detail row yet (a transient metrics-store gap, or the very first
	// scan before the agent's first push lands) must never read as "0 ranks placed" and start
	// counting toward eviction for a job that may already be fully bound and running — codex
	// review caught this as a real hazard (a missing/stale row could evict a healthy job). Absent
	// data means "we don't know yet", not "nothing is placed"; the next scan tries again.
	if found && scheduledNodes < int32(exp.Job.Nodes()) {
		evicted, err := w.checkScaleUpDeadline(ctx, exp, schedulingReason, autoscalerEnabled, clusterIDs)
		if err != nil {
			return fmt.Errorf("check scale-up deadline: %w", err)
		}
		if evicted {
			return nil
		}
	}

	// The phase is polled before any further deadline is applied, so every decision below is made
	// against what the runtime reports right now rather than against how long a Postgres row has
	// sat in a pre-RUNNING status. Keying "stuck pending" on status alone evicted pods that were
	// plainly Running — they were merely blocked from being *marked* RUNNING, which is a different
	// fault with a different remedy, and refunding them as never-started billed nobody for real
	// hardware.
	phase, err := w.backend.PollJobPhase(ctx, exp)
	if err != nil {
		return fmt.Errorf("poll actual phase: %w", err)
	}

	switch phase {
	case workload.JobPhaseSucceeded:
		return w.onFinished(ctx, exp, true)
	case workload.JobPhaseFailed:
		return w.onFinished(ctx, exp, false)
	case workload.JobPhaseRunning:
		if exp.Status == domain.StatusRunning {
			// The cluster agent continuously writes actual accelerator type into the metrics store.
			return nil
		}
		switch _, outcome := w.onRunning(ctx, exp); outcome {
		case runningMarked, runningLeftState:
			return nil
		case runningTypeMismatch:
			w.evictNotYetRunningWithFailover(ctx, exp, domain.EvictionFlavorMismatch, clusterIDs[exp.ClusterName])
			return nil
		case runningTypeUnobservable:
			return w.evictIfPastAdmissionDeadline(ctx, exp, domain.EvictionAcceleratorTypeUnobservable)
		default:
			return fmt.Errorf("unknown running outcome %d", outcome)
		}
	case workload.JobPhaseGone:
		if exp.Status == domain.StatusRunning {
			// Desired state remains authoritative and the cluster agent independently retries
			// creation. A RUNNING job whose workload has genuinely vanished is the controller's
			// call, not this loop's: it is the side that can tell an absent workload apart from
			// an absent cluster (see controller.checkSilence).
			return nil
		}
		// Gone keeps its own existing handling, unchanged: the placement-deadline check above
		// only fires while scheduledNodes < Nodes(), so a fully-bound job that then vanishes
		// (scheduledNodes already caught up before Gone was observed) still needs this bound.
		return w.evictIfPastAdmissionDeadline(ctx, exp, domain.EvictionStuckPending)
	case workload.JobPhasePending:
		// Superseded by the placement-deadline check above, which now runs unconditionally
		// before this switch and covers exactly this case (plus the ones that check used to
		// miss — Running with a partial gang, and Gone before scheduledNodes catches up).
		return nil
	default:
		return fmt.Errorf("unknown actual phase %q", phase)
	}
}
