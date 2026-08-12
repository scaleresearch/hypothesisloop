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
	for _, status := range statuses {
		exps, err := w.store.ListExperimentsWithStatus(ctx, status)
		if err != nil {
			return err
		}
		for _, exp := range exps {
			if err := w.reconcileOne(ctx, exp); err != nil {
				w.logger.Error("job_watcher: reconcile experiment",
					zap.String("experiment_id", exp.ID), zap.Error(err))
			}
		}
	}
	return nil
}

func (w *JobWatcher) reconcileOne(ctx context.Context, exp *domain.Experiment) error {
	// Checked regardless of exp.Status, including RUNNING: a Kubernetes Job's Active count
	// (what PollJobPhase treats as Running, see k8sexec.PollJobPhaseAndUID) includes a pod
	// stuck in ImagePullBackOff — the desired-state Job "exists and isn't finished" long
	// before its container ever actually starts. A never-self-heals reason means it never
	// will, whatever exp.Status currently says.
	if detailer, ok := w.backend.(PhaseDetailer); ok {
		reason, message, _, found, err := detailer.PollPhaseDetail(ctx, exp)
		if err != nil {
			return fmt.Errorf("poll phase detail: %w", err)
		}
		if found && domain.NeverSelfHealsPhaseReasons[reason] {
			w.onUnschedulable(ctx, exp, reason, message)
			return nil
		}
	}

	if exp.Status != domain.StatusRunning && w.stuckPendingTimeout > 0 && exp.SubmittedAt != nil &&
		time.Since(*exp.SubmittedAt) > w.stuckPendingTimeout {
		w.onStuckPending(ctx, exp)
		return nil
	}

	phase, err := w.backend.PollJobPhase(ctx, exp)
	if err != nil {
		return fmt.Errorf("poll actual phase: %w", err)
	}

	switch phase {
	case workload.JobPhaseGone, workload.JobPhasePending:
		// Desired state remains authoritative. The cluster agent independently retries creation;
		// an admission that never becomes Running is bounded by stuckPendingTimeout above.
		return nil
	case workload.JobPhaseRunning:
		if exp.Status != domain.StatusRunning {
			w.onRunning(ctx, exp)
			return nil
		}
		// The cluster agent continuously writes actual accelerator type into the metrics store.
	case workload.JobPhaseSucceeded:
		return w.onFinished(ctx, exp, true)
	case workload.JobPhaseFailed:
		return w.onFinished(ctx, exp, false)
	default:
		return fmt.Errorf("unknown actual phase %q", phase)
	}
	return nil
}
