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
		domain.StatusRunning,
	}
	// Which connected cluster reports which id, read once for the pass: every stuck-pending
	// decision in it needs the id to find its cluster's timeout, and reading it per job cost one
	// cluster-wide metrics query for every job still waiting to start.
	clusterIDs, idsErr := w.backend.GetClusterIDs(ctx)
	// Speculative-submit inputs (autoscaler.md), read only when a deployment has opted in via
	// WithScaleUpTimeout — mirrors Loop.tick's own opt-in gate for the same reason: a backend
	// that has not implemented the capability read must not be asked to. With the opt-in a fetch
	// failure ends the pass: judging a placement deadline without them would apply the wrong
	// timeout.
	var autoscalerEnabled map[string]bool
	if w.scaleUpTimeout > 0 {
		var err error
		autoscalerEnabled, err = w.backend.GetAutoscalerCapability(ctx)
		if err != nil {
			return fmt.Errorf("autoscaler capability: %w", err)
		}
		if idsErr != nil {
			return fmt.Errorf("cluster ids: %w", idsErr)
		}
	}
	// Without it the pass goes on with no ids: no stuck-pending decision is made in it, and
	// everything else a job's reconcile does, reading a finished job as finished above all,
	// still is.
	if idsErr != nil {
		w.logger.Warn("job_watcher: cluster ids unreadable, no stuck-pending deadline is judged this pass", zap.Error(idsErr))
		clusterIDs = nil
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
	// (what k8sexec.PollJobStatus treats as Running) includes a pod
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
	// found=false now means "no phase-detail row for this experiment's current cluster+attempt",
	// which after fencing GetLatestPhaseDetailFull to (cluster_name, attempt) is exactly "not
	// placed yet" — a permanent metrics-store gap and a genuinely never-started pod are the same
	// state from here: neither can be told apart from "we don't know", and both must still be
	// bounded by the deadline below, or a job that never gets a row written (quota-blocked pod,
	// or an agent whose PollPhaseDetail errors every poll) holds its reservation forever. This
	// used to be gated on found to dodge a *different* hazard — a stale row from a previous
	// attempt misread as "0 ranks placed" for the new one — which the attempt fence above now
	// closes at the source, so treating not-found as unplaced no longer risks evicting a healthy
	// job on a one-off metrics-store hiccup: it still only evicts once the deadline, measured from
	// submitted_at, has actually passed.
	//
	// Not for a RUNNING job: its placement is over, and the deadline counts from submitted_at, so
	// a pod being recreated hours in would evict it at once.
	bound := found && scheduledNodes >= int32(exp.Job.Nodes())
	if exp.Status != domain.StatusRunning && !bound {
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
		// A bound job is past the placement deadline above, and a cluster that stops reporting
		// never yields a phase to act on, so the admission deadline is its only bound.
		if exp.Status != domain.StatusRunning && bound {
			if deadlineErr := w.evictIfStuckPending(ctx, exp, clusterIDs, domain.EvictionStuckPending); deadlineErr != nil {
				return deadlineErr
			}
		}
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
		if found && scheduledNodes < int32(exp.Job.Nodes()) {
			// One landed rank reads as Running. The gang stays SUBMITTED, under the placement
			// deadline above, until every rank has bound.
			return nil
		}
		switch _, outcome := w.onRunning(ctx, exp); outcome {
		case runningMarked, runningLeftState:
			return nil
		case runningTypeMismatch:
			// Only feed this into cross-job tried_clusters backoff on a cluster the autoscaler
			// path actually cares about — an ordinary live-fit cluster reporting the wrong
			// flavor is a ClaimSubmitted resolution bug, not evidence the cluster's autoscaler
			// failed to deliver capacity, and must not bar other jobs from speculating there.
			triedClusterID := ""
			if autoscalerEnabled[exp.ClusterName] {
				triedClusterID = clusterIDs[exp.ClusterName]
			}
			w.evictNotYetRunningWithFailover(ctx, exp, domain.EvictionFlavorMismatch, triedClusterID)
			return nil
		case runningTypeUnobservable:
			return w.evictIfStuckPending(ctx, exp, clusterIDs, domain.EvictionAcceleratorTypeUnobservable)
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
		return w.evictIfStuckPending(ctx, exp, clusterIDs, domain.EvictionStuckPending)
	case workload.JobPhasePending:
		// An unbound job is under the placement deadline above. A bound one the runtime reports
		// as pending is starting, which includes pulling its image, and gets the start timeout.
		if exp.Status != domain.StatusRunning && bound {
			return w.evictIfPastDeadline(ctx, exp, w.startTimeout, domain.EvictionStuckPending)
		}
		return nil
	default:
		return fmt.Errorf("unknown actual phase %q", phase)
	}
}
