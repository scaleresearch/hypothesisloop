package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// onStuckPending evicts a job that was admitted but never reported RUNNING within
// stuckPendingTimeout (e.g. unschedulable due to fragmentation, a bad image, or a GPU-flavor
// stockout). Transitions SUBMITTED/ADMITTED -> EVICTED — this alone removes the job from the
// cluster-agent's desired-state set, so no separate delete call is needed; the cluster-agent's
// own reconcile loop deletes the underlying Job on its next pull, same as any other eviction.
// Refunds the full reservation since zero elapsed runtime was ever consumed.
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
		// Already moved on (e.g. it started running right before this check fired) — nothing to do.
		return
	}
	if err := w.store.UpdateEvictionReason(ctx, exp.ID, string(domain.EvictionStuckPending)); err != nil {
		w.logger.Warn("job_watcher: set eviction reason", zap.String("id", exp.ID), zap.Error(err))
	}
	// Evicted without ever reaching RUNNING — release the durable pending-capacity claim (see
	// pending_capacity_reservations' schema comment) so it stops being subtracted from live
	// capacity on the next tick.
	if err := w.store.DeletePendingReservation(ctx, exp.ID); err != nil {
		w.logger.Warn("job_watcher: delete pending reservation on stuck_pending eviction", zap.String("id", exp.ID), zap.Error(err))
	}
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionStuckPending)).Inc()

	// exp never reported RUNNING, so it has no observations in the metrics DB — Settle will
	// naturally compute 0 elapsed hours for it (see metricsdb.ObservedElapsedHours), correctly
	// zeroing out its reservation. If this attempt fails (e.g. metrics DB unreachable), the
	// experiment is left with quota_settled_at unset and services/settlement.Reconciler retries
	// it — no reservation is ever permanently stuck.
	w.settleQuota(ctx, exp)
}

// settleQuota durably writes exp's final observed usage and marks it settled on success. Safe
// to call unconditionally: a no-op settler or an experiment with no platform experiment both
// just succeed trivially. On failure, exp is left unsettled for services/settlement.Reconciler
// to retry — this call is a best-effort fast path, not the only chance to settle.
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

// onRunning handles the ADMITTED/SUBMITTED → RUNNING transition, and returns the accelerator
// type this admission landed on (so watchOne can keep re-stamping it on subsequent ticks — see
// the JobPhaseRunning case in watchOne for why a one-shot write isn't enough on its own) and
// whether the transition actually happened. A false return means the experiment already left
// SUBMITTED/ADMITTED (cancelled/evicted concurrently) — the caller must stop watching and skip
// every accelerator/billing side effect below, since none of it applies to a job that's already
// terminal.
func (w *JobWatcher) onRunning(ctx context.Context, exp *domain.Experiment) (domain.GPUType, bool) {
	w.logger.Info("job_watcher: experiment running", zap.String("id", exp.ID))
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: mark running", zap.String("id", exp.ID), zap.Error(err))
	}
	if !started {
		w.logger.Info("job_watcher: experiment left SUBMITTED/ADMITTED before Running was observed — skipping", zap.String("id", exp.ID))
		return "", false
	}

	// The pod is now confirmed to exist — live capacity already reflects its request (see
	// loop_tick.go's step-2 comment), so the durable pending-capacity claim from submitJob is
	// no longer needed. Best-effort: a failure here is a transient over-reservation, not a
	// correctness break (tick() would just see slightly less capacity than actually free until
	// this succeeds on a retry or the job eventually terminates).
	if err := w.store.DeletePendingReservation(ctx, exp.ID); err != nil {
		w.logger.Warn("job_watcher: delete pending reservation on running", zap.String("id", exp.ID), zap.Error(err))
	}

	admittedType := w.backend.GetAdmittedGPUType(ctx, exp)

	// Record which accelerator type this admission actually landed on — the sole write path for
	// metricsdb.ObservedGPUCost's per-type billing (see metricsdb/observed.go). Written
	// unconditionally, every time this experiment reaches RUNNING (initial admission and every
	// re-admission after a reschedule), not just when it differs from what was requested: a job
	// re-admitted onto the *same* type it had before still needs a fresh marker, since the
	// previous pod's marker only carries forward as long as nothing overrides it, and a real gap
	// (the time it spent QUEUED between eviction and re-admission) shouldn't be misread as still
	// running on the old type.
	if w.metricsDBURL != "" {
		if err := metricsdb.RecordAcceleratorType(ctx, w.metricsDBURL, exp.ID, string(admittedType), time.Now().UTC()); err != nil {
			w.logger.Warn("job_watcher: record accelerator type", zap.String("id", exp.ID), zap.Error(err))
		}
	}

	// If the job ran on a more expensive flavor (H100 → H200), debit the
	// extra cost for the full estimated duration so accounting reflects actual hardware.
	if w.quotaAdjuster == nil || exp.PlatformExperimentID == "" {
		return admittedType, true
	}
	if admittedType == exp.GPUType {
		return admittedType, true
	}
	extraPerHour := admittedType.Cost() - exp.GPUType.Cost()
	if extraPerHour <= 0 {
		return admittedType, true
	}
	extra := extraPerHour * float64(exp.GPUCount) * exp.EstimatedDurationHours
	w.logger.Info("job_watcher: flavor substitution — debiting extra cost",
		zap.String("id", exp.ID),
		zap.String("requested", string(exp.GPUType)),
		zap.String("admitted", string(admittedType)),
		zap.Float64("extra_t4h", extra),
	)
	if err := w.quotaAdjuster.DebitQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, domain.ResourceGPUHours, exp.CapacityTier, extra); err != nil {
		w.logger.Warn("job_watcher: flavor substitution debit", zap.String("id", exp.ID), zap.Error(err))
	}

	// Persist the admitted flavor and its recomputed estimated cost. Without this, the
	// completion refund, quota-exhaustion check, and phase-2 consumption tally all keep
	// using the cheaper requested rate (exp.GPUType.Cost()), under-refunding the agent and
	// under-counting real consumption for the unused/elapsed hours charged at the higher rate.
	newEstCost := admittedType.Cost() * float64(exp.GPUCount) * exp.EstimatedDurationHours
	if err := w.store.UpdateAdmittedFlavor(ctx, exp.ID, admittedType, newEstCost); err != nil {
		w.logger.Warn("job_watcher: persist admitted flavor", zap.String("id", exp.ID), zap.Error(err))
	}
	return admittedType, true
}

// backfillStartedFromObservations handles a terminal report arriving for an experiment this
// watcher never itself saw reach RUNNING (the 3s poll cadence can miss a job that starts and
// finishes between two ticks — see onFinished). Before treating that as "never ran" and
// refunding its full reservation, check the metrics store — the durable source of truth for
// execution, independent of whether any intermediate poll happened to land on it — for any
// heartbeat or training-metric observation tied to this experiment. If one exists, the job did
// run: backfill started_at (best-effort — MarkStarted always stamps "now", but Settle only uses
// StartedAt as a nil check and recomputes real elapsed time from the metrics store itself, so the
// stamped value need not be exact) and update exp in place so the caller settles from observed
// usage instead of zeroing it out. Returns whether the backfill happened.
func (w *JobWatcher) backfillStartedFromObservations(ctx context.Context, exp *domain.Experiment) bool {
	if w.metricsDBURL == "" {
		return false
	}
	_, observed, err := metricsdb.FirstObserved(ctx, w.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, w.observedStep)
	if err != nil {
		w.logger.Warn("job_watcher: check observations for never-started terminal job", zap.String("id", exp.ID), zap.Error(err))
		return false
	}
	if !observed {
		return false
	}
	started, err := w.store.MarkStarted(ctx, exp.ID)
	if err != nil {
		w.logger.Error("job_watcher: backfill started_at", zap.String("id", exp.ID), zap.Error(err))
		return false
	}
	if !started {
		// Already left SUBMITTED/ADMITTED by some other path — nothing to backfill.
		return false
	}
	w.logger.Warn("job_watcher: terminal report skipped RUNNING sample but metrics store shows it ran — backfilling started_at",
		zap.String("id", exp.ID))
	now := time.Now().UTC()
	exp.StartedAt = &now
	return true
}

// onFinished handles the RUNNING → COMPLETED/FAILED transition and settles quota.
func (w *JobWatcher) onFinished(ctx context.Context, exp *domain.Experiment, succeeded bool, startedAt time.Time) {
	status := domain.StatusCompleted
	if !succeeded {
		status = domain.StatusFailed
	}
	w.logger.Info("job_watcher: experiment finished",
		zap.String("id", exp.ID),
		zap.String("status", string(status)),
	)

	if startedAt.IsZero() && w.backfillStartedFromObservations(ctx, exp) {
		startedAt = *exp.StartedAt
	}

	// If the job was seen as RUNNING, use a conditional transition so that a concurrent
	// controller eviction (which also transitions from RUNNING) can only win once.
	// If we lose the race, skip the refund — the controller already issued it.
	if !startedAt.IsZero() {
		updated, err := w.store.TransitionStatus(ctx, exp.ID, domain.StatusRunning, status)
		if err != nil {
			w.logger.Error("job_watcher: transition status", zap.String("id", exp.ID), zap.Error(err))
			return
		}
		if !updated {
			// Controller evicted it concurrently — refund already issued, nothing to do.
			return
		}
	} else {
		// Job completed without this watch cycle observing a RUNNING poll — typically a true
		// ADMITTED → terminal transition, but a pod recreation (e.g. node-death reschedule)
		// resets this goroutine's local startedAt tracking too, so a job that IS actually
		// RUNNING (and racing a concurrent controller eviction) can also land here. Guard with
		// the same non-terminal check rather than an unconditional write, or a late-arriving
		// completion from an already-evicted job's stale pod resurrects it past a terminal state.
		updated, err := w.store.TransitionStatusFromNonTerminal(ctx, exp.ID, status)
		if err != nil {
			w.logger.Error("job_watcher: update status", zap.String("id", exp.ID), zap.Error(err))
			return
		}
		if !updated {
			// Already terminal (e.g. controller evicted it concurrently) — nothing to do.
			return
		}
	}

	// Release the durable pending-capacity claim, if one is still outstanding — normally
	// already deleted by onRunning, but a job that skipped straight from SUBMITTED/ADMITTED to
	// terminal (the startedAt.IsZero() branch above) never went through onRunning, so it must
	// be cleared here too. No-op (and safe to call unconditionally) if already gone.
	if err := w.store.DeletePendingReservation(ctx, exp.ID); err != nil {
		w.logger.Warn("job_watcher: delete pending reservation on finish", zap.String("id", exp.ID), zap.Error(err))
	}

	// Durably settle each resource dimension's observed cost: elapsed × gpu_count × rate for
	// GPU-hours, and the equivalent proportional-elapsed-fraction amount for CPU/RAM/storage.
	// See settleQuota's doc comment for the crash/outage retry story.
	w.settleQuota(ctx, exp)
}
