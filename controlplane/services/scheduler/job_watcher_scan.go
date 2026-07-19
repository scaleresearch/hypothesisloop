package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// Start runs the scan loop until ctx is cancelled.
func (w *JobWatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(w.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.scanAndWatch(ctx); err != nil {
				w.logger.Error("job_watcher: scan", zap.Error(err))
			}
		}
	}
}

// scanAndWatch finds experiments in SUBMITTED, ADMITTED, or RUNNING state without an active
// watch goroutine and starts one for each. RUNNING must be included here — otherwise a
// control-service restart permanently loses the watch for any job already running at the
// time of restart, and its eventual SUCCEEDED/FAILED report is never converted into terminal
// experiment state or quota settlement.
func (w *JobWatcher) scanAndWatch(ctx context.Context) error {
	submitted, err := w.store.ListExperimentsWithStatus(ctx, domain.StatusSubmitted)
	if err != nil {
		return err
	}
	admitted, err := w.store.ListExperimentsWithStatus(ctx, domain.StatusAdmitted)
	if err != nil {
		return err
	}
	running, err := w.store.ListExperimentsWithStatus(ctx, domain.StatusRunning)
	if err != nil {
		return err
	}
	for _, exp := range append(append(submitted, admitted...), running...) {
		w.mu.Lock()
		_, already := w.watched[exp.ID]
		w.mu.Unlock()
		if already {
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		w.watched[exp.ID] = cancel
		w.mu.Unlock()
		go w.watchOne(watchCtx, exp)
	}
	return nil
}

// watchOne polls the backend workload for a single experiment until it reaches a terminal phase
// or the context is cancelled (e.g. the experiment was evicted by the controller).
func (w *JobWatcher) watchOne(ctx context.Context, exp *domain.Experiment) {
	defer func() {
		w.mu.Lock()
		delete(w.watched, exp.ID)
		w.mu.Unlock()
	}()

	var startedAt time.Time
	var admittedType domain.AcceleratorType
	transitionedToRunning := false

	// A restart-recovered watch for an already-RUNNING experiment must not repeat onRunning's
	// side effects (accelerator-type recording, flavor-substitution billing) — seed the local
	// state from the durable started_at column instead of assuming this is a fresh admission.
	if exp.Status == domain.StatusRunning && exp.StartedAt != nil {
		transitionedToRunning = true
		startedAt = *exp.StartedAt
		admittedType = exp.AcceleratorType
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !transitionedToRunning && w.stuckPendingTimeout > 0 && exp.SubmittedAt != nil &&
				time.Since(*exp.SubmittedAt) > w.stuckPendingTimeout {
				w.onStuckPending(ctx, exp)
				return
			}

			phase, err := w.backend.PollJobPhase(ctx, exp)
			if err != nil {
				w.logger.Warn("job_watcher: poll", zap.String("id", exp.ID), zap.Error(err))
				continue
			}

			switch phase {
			case workload.JobPhaseGone:
				// Job was deleted externally — controller already updated DB status.
				return

			case workload.JobPhaseRunning:
				if !transitionedToRunning {
					var started bool
					admittedType, started = w.onRunning(ctx, exp)
					if !started {
						// Experiment already left SUBMITTED/ADMITTED (cancelled/evicted
						// concurrently) — nothing more for this watcher to do.
						return
					}
					transitionedToRunning = true
					startedAt = time.Now().UTC()
				} else if w.metricsDBURL != "" {
					// Re-query and re-emit the accelerator-type marker on every poll tick, not
					// just once at admission. Two independent reasons:
					//   1. A once-per-admission write is a single-sample series, and observed
					//      behavior against GreptimeDB is that sparse single-sample series can
					//      sit invisible to range queries for much longer than dense,
					//      continuously-appended ones (the heartbeat series never shows this
					//      lag) — the same eventual-consistency risk RecordObservation avoids by
					//      construction. A periodic re-emit makes the type series behave like the
					//      heartbeat series: multiple recent samples, so onFinished's query only
					//      needs any one of them to have landed.
					//   2. It also closes the gap where a job is rescheduled onto different
					//      hardware within the same watch goroutine (no fresh onRunning call, so
					//      no fresh admission event): re-querying GetAdmittedAcceleratorType here, not
					//      just re-stamping the cached value from admission, picks up a live type
					//      change. This is cheap for the real backend (queuebackend.Backend reads
					//      a stored JobReport, not a k8s API call) and a harmless no-op for the
					//      raw k8s client, which always reports exp.AcceleratorType.
					if t := w.backend.GetAdmittedAcceleratorType(ctx, exp); t != "" {
						admittedType = t
					}
					if admittedType != "" {
						if err := metricsdb.RecordAcceleratorType(ctx, w.metricsDBURL, exp.ID, string(admittedType), time.Now().UTC()); err != nil {
							w.logger.Warn("job_watcher: re-record accelerator type", zap.String("id", exp.ID), zap.Error(err))
						}
					}
				}

			case workload.JobPhaseSucceeded:
				w.onFinished(ctx, exp, true, startedAt)
				return

			case workload.JobPhaseFailed:
				w.onFinished(ctx, exp, false, startedAt)
				return
			}
		}
	}
}
