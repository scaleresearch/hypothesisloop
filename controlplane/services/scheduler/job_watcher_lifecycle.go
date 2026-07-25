package scheduler

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// onStuckPending evicts a job admitted but never reported RUNNING within stuckPendingTimeout
// (e.g. unschedulable, bad image, accelerator stockout). Transitions SUBMITTED/ADMITTED ->
// EVICTED; cluster-agent's own reconcile loop deletes the Job on its next pull. Refunds the
// full reservation since no runtime was consumed.
func (w *JobWatcher) onStuckPending(ctx context.Context, exp *domain.Experiment) {
	w.logger.Warn("job_watcher: stuck pending, evicting",
		zap.String("id", exp.ID),
		zap.Duration("timeout", w.stuckPendingTimeout),
	)

	updated, err := w.store.TransitionStatus(ctx, exp.ID, exp.Status, domain.StatusEvicted)
	if err != nil {
		w.logger.Error("job_watcher: stuck_pending transition", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	if !updated {
		// Already moved on (e.g. started running right before this check) — nothing to do.
		return
	}
	if err := w.store.UpdateEvictionReason(ctx, exp.ID, string(domain.EvictionStuckPending)); err != nil {
		w.logger.Warn("job_watcher: set eviction reason", zap.String("id", exp.ID), zap.Error(err))
	}
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionStuckPending)).Inc()

	// exp never reported RUNNING, so Settle naturally computes 0 elapsed hours, zeroing its
	// reservation. On failure, services/settlement.Reconciler retries — nothing gets stuck.
	w.settleQuota(ctx, exp)
}

// settleQuota durably writes exp's final observed usage and marks it settled on success. Safe
// to call unconditionally (no-op settler or no platform experiment both succeed trivially). On
// failure, exp is left unsettled for services/settlement.Reconciler to retry.
func (w *JobWatcher) settleQuota(ctx context.Context, exp *domain.Experiment) {
	if w.settler == nil {
		return
	}
	if err := w.settler.Settle(ctx, exp); err != nil {
		w.logger.Warn("job_watcher: settle quota", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	if err := w.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		w.logger.Error("job_watcher: mark quota settled", zap.String("id", exp.ID), zap.Error(err))
	}
}

// onRunning handles the ADMITTED/SUBMITTED → RUNNING transition, returning the accelerator type
// this admission landed on and whether the transition actually happened. A false return means
// the experiment already left SUBMITTED/ADMITTED (cancelled/evicted concurrently) — the caller
// must stop watching and skip every side effect below.
func (w *JobWatcher) onRunning(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, bool) {
	w.logger.Info("job_watcher: experiment running", zap.String("id", exp.ID))
	var admittedType domain.AcceleratorType
	if exp.AcceleratorCount > 0 {
		var err error
		admittedType, err = w.backend.GetAdmittedAcceleratorType(ctx, exp)
		if err != nil {
			w.logger.Error("job_watcher: observed accelerator type", zap.String("id", exp.ID), zap.Error(err))
			return "", false
		}
		if admittedType != exp.AcceleratorType {
			w.logger.Error("job_watcher: observed accelerator type disagrees with desired flavor",
				zap.String("id", exp.ID), zap.String("desired", string(exp.AcceleratorType)), zap.String("observed", string(admittedType)))
			return "", false
		}
	}
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: mark running", zap.String("id", exp.ID), zap.Error(err))
	}
	if !started {
		w.logger.Info("job_watcher: experiment left SUBMITTED/ADMITTED before Running was observed — skipping", zap.String("id", exp.ID))
		return "", false
	}

	// A CPU-only job has no accelerator dimension — whatever node it landed on (even an
	// accelerator-labeled one) is not an "admitted flavor" and must not be reverse-mapped into one.
	return admittedType, true
}

// backfillStartedFromObservations handles a terminal report for an experiment this watcher never
// itself saw reach RUNNING (the poll cadence can miss a job that starts and finishes between two
// ticks — see onFinished). Before treating that as "never ran," check the metrics store for any
// observation tied to this experiment; if one exists, move state through RUNNING first.
func (w *JobWatcher) backfillStartedFromObservations(ctx context.Context, exp *domain.Experiment) (bool, error) {
	if w.metricsDBURL == "" {
		return false, fmt.Errorf("job_watcher: metrics DB URL is required to resolve terminal lifecycle")
	}
	_, observed, err := metricsdb.FirstObserved(ctx, w.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, w.observedStep)
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

	// Durably settle each resource dimension's observed cost. See settleQuota's doc comment for
	// the crash/outage retry story.
	w.settleQuota(ctx, exp)
	return nil
}
