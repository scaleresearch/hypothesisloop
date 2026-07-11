package scheduler

// JobWatcher watches backend-executed workloads for experiments in SUBMITTED or ADMITTED
// state and drives the DB transitions SUBMITTED/ADMITTED → RUNNING → COMPLETED/FAILED.
// It talks only to the workload.Backend interface, so it has no idea whether jobs are
// actually running as Kubernetes Jobs, Slurm allocations, or anything else.
//
// Responsibility split:
//   - JobWatcher: backend workload status → DB status sync and quota accounting on completion.
//   - Controller (services/controller): policy-driven eviction (silence, overrun,
//     quota exhaustion, metric decline). Terminates jobs that are running but should stop.
//
// JobWatcher is only concerned with jobs that finish naturally or are already gone;
// it never initiates termination itself.

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// JobStatusStore is the persistence interface needed by JobWatcher.
type JobStatusStore interface {
	ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error)
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	// MarkStarted transitions SUBMITTED/ADMITTED -> RUNNING. Returns false (no error) if the
	// experiment already left that state (e.g. cancelled/evicted concurrently) — the caller
	// must skip every other onRunning side effect in that case.
	MarkStarted(ctx context.Context, id string) (bool, error)
	// TransitionStatus atomically updates status only when current status matches from.
	// Returns false (no error) if the row was already in a different state — used to
	// prevent double-refunds when natural completion and controller eviction race.
	TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error)
	// TransitionStatusFromNonTerminal atomically updates status unless the row is already
	// COMPLETED/FAILED/EVICTED — used where the caller can't name the exact prior status (see
	// onFinished's ADMITTED-terminal branch) but still must not resurrect an already-terminal job.
	TransitionStatusFromNonTerminal(ctx context.Context, id string, to domain.ExperimentStatus) (bool, error)
	// UpdateAdmittedFlavor persists the flavor the job actually ran on and the
	// estimated cost recomputed at that flavor's rate, so downstream accounting is consistent.
	UpdateAdmittedFlavor(ctx context.Context, id string, gpuType domain.GPUType, estimatedCostT4H float64) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
}

// JobQuotaRefunder writes each resource dimension's final observed cost (an absolute set, not a
// delta — see metricsdb.UsageTracker.SetObserved) when a job completes or fails: elapsed × rate
// for GPU-hours, and the equivalent proportional-elapsed-fraction amount for
// CPU-core-hours/RAM-GB-hours/storage-GB-hours.
type JobQuotaRefunder interface {
	RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
}

// JobQuotaAdjuster debits extra cost if a job ever ran on a more expensive
// flavor than requested (e.g. H100 request admitted on H200). GPU-hours only — flavor
// substitution is a GPU-hardware-tier concept, not one that applies to CPU/RAM/storage.
type JobQuotaAdjuster interface {
	DebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, costT4H float64) error
}

// JobBackendClient is the narrow slice of workload.Backend that JobWatcher needs. It is
// deliberately backend-agnostic: JobWatcher never assumes Kubernetes (or any other
// execution engine) underneath. Implemented by workload.Backend (in production,
// *workload.ClusterSet, but any Backend implementation satisfies it unchanged).
type JobBackendClient interface {
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (workload.JobPhase, error)
	GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType
}

// JobWatcher starts a per-experiment goroutine for every SUBMITTED or ADMITTED experiment
// and drives its lifecycle to RUNNING and then COMPLETED/FAILED as the backend workload progresses.
type JobWatcher struct {
	store         JobStatusStore
	backend       JobBackendClient
	logger        *zap.Logger
	quotaRefunder JobQuotaRefunder // optional; refunds unused T4h on job end
	quotaAdjuster JobQuotaAdjuster // optional; debits extra cost on flavor substitution
	mu            sync.Mutex
	watched       map[string]context.CancelFunc
	pollInterval  time.Duration
	scanInterval  time.Duration
	// stuckPendingTimeout bounds how long a job may stay SUBMITTED/ADMITTED without reporting
	// RUNNING before it is evicted with reason stuck_pending and fully refunded. See #1 in
	// competetors/SYNTHESIS_GAPS_AND_PLAN.md.
	stuckPendingTimeout time.Duration

	// metricsDBURL, observedGapCap, observedStep configure onFinished's observed-elapsed query
	// (metricsdb.ObservedElapsedHours) — the same source of truth every other termination path
	// uses. GreptimeDB is a required dependency: no fallback if unset or unreachable.
	metricsDBURL   string
	observedGapCap time.Duration
	observedStep   time.Duration
}

// NewJobWatcher constructs a JobWatcher.
func NewJobWatcher(store JobStatusStore, backend JobBackendClient, logger *zap.Logger) *JobWatcher {
	return &JobWatcher{
		store:        store,
		backend:      backend,
		logger:       logger,
		watched:      make(map[string]context.CancelFunc),
		pollInterval: 0,
		scanInterval: 0,
	}
}

func (w *JobWatcher) WithPollInterval(d time.Duration) *JobWatcher {
	w.pollInterval = d
	return w
}

func (w *JobWatcher) WithScanInterval(d time.Duration) *JobWatcher {
	w.scanInterval = d
	return w
}

// WithStuckPendingTimeout sets how long a job may stay SUBMITTED/ADMITTED without reporting
// RUNNING before job_watcher evicts it as stuck_pending.
func (w *JobWatcher) WithStuckPendingTimeout(d time.Duration) *JobWatcher {
	w.stuckPendingTimeout = d
	return w
}

// WithQuotaRefunder attaches a refunder that issues unused-T4h credits on job completion.
func (w *JobWatcher) WithQuotaRefunder(r JobQuotaRefunder) *JobWatcher {
	w.quotaRefunder = r
	return w
}

// WithQuotaAdjuster attaches an adjuster used to charge the cost difference if a job
// substitutes a more expensive flavor (e.g. H100 → H200).
func (w *JobWatcher) WithQuotaAdjuster(a JobQuotaAdjuster) *JobWatcher {
	w.quotaAdjuster = a
	return w
}

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step parameters onFinished uses to
// compute observed-elapsed time — see metricsdb.ObservedElapsedHours. Pass the same values the
// Controller in this deployment uses, so every termination path (automatic eviction, natural
// completion, user cancel) agrees on what "how long did this run" means.
func (w *JobWatcher) WithObservedTimeConfig(metricsDBURL string, gapCap, step time.Duration) *JobWatcher {
	w.metricsDBURL = metricsDBURL
	w.observedGapCap = gapCap
	w.observedStep = step
	return w
}

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

// scanAndWatch finds experiments in SUBMITTED or ADMITTED state without an active
// watch goroutine and starts one for each.
func (w *JobWatcher) scanAndWatch(ctx context.Context) error {
	submitted, err := w.store.ListExperimentsWithStatus(ctx, domain.StatusSubmitted)
	if err != nil {
		return err
	}
	admitted, err := w.store.ListExperimentsWithStatus(ctx, domain.StatusAdmitted)
	if err != nil {
		return err
	}
	for _, exp := range append(submitted, admitted...) {
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
	var admittedType domain.GPUType
	transitionedToRunning := false

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
					//      no fresh admission event): re-querying GetAdmittedGPUType here, not
					//      just re-stamping the cached value from admission, picks up a live type
					//      change. This is cheap for the real backend (queuebackend.Backend reads
					//      a stored JobReport, not a k8s API call) and a harmless no-op for the
					//      raw k8s client, which always reports exp.GPUType.
					if t := w.backend.GetAdmittedGPUType(ctx, exp); t != "" {
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
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionStuckPending)).Inc()

	if w.quotaRefunder == nil || exp.PlatformExperimentID == "" {
		return
	}
	for _, d := range []struct {
		resourceType domain.ResourceType
		amount       float64
	}{
		{domain.ResourceGPUHours, exp.EstimatedCostT4H},
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours},
	} {
		if d.amount <= 0 {
			continue
		}
		// Never started running: 0 observed cost, regardless of what was reserved.
		if err := w.quotaRefunder.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, d.resourceType, exp.CapacityTier, 0); err != nil {
			w.logger.Error("job_watcher: stuck_pending refund", zap.String("id", exp.ID), zap.String("resource", string(d.resourceType)), zap.Error(err))
		}
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

// onFinished handles the RUNNING → COMPLETED/FAILED transition and issues quota refunds.
// observedElapsedHours returns exp's confirmed-alive time via metricsdb.ObservedElapsedHours —
// the same GreptimeDB-backed query every other termination path uses, with no start time
// supplied: the start boundary is derived from the metrics themselves (see
// metricsdb.FirstObserved), not from this goroutine's own poll-observed startedAt. GreptimeDB is
// a required dependency of this deployment with no fallback: a query error is returned to the
// caller, which skips the refund write for this pass rather than substituting a wall-clock guess.
func (w *JobWatcher) observedElapsedHours(ctx context.Context, exp *domain.Experiment) (float64, error) {
	return metricsdb.ObservedElapsedHours(ctx, w.metricsDBURL, exp.ID, time.Now().UTC(), observedMaxLookback, w.observedGapCap, w.observedStep)
}

func (w *JobWatcher) onFinished(ctx context.Context, exp *domain.Experiment, succeeded bool, startedAt time.Time) {
	status := domain.StatusCompleted
	if !succeeded {
		status = domain.StatusFailed
	}
	w.logger.Info("job_watcher: experiment finished",
		zap.String("id", exp.ID),
		zap.String("status", string(status)),
	)

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

	// Write each resource dimension's observed cost: elapsed × gpu_count × rate for GPU-hours;
	// the equivalent proportional-elapsed-fraction amount for CPU/RAM/storage. Both COMPLETED
	// and FAILED jobs get this write — only quota-exhausted evictions don't, because the budget
	// is genuinely consumed. 0 (never started) and >estimate (overrun) are both meaningful and
	// always written, never skipped.
	if w.quotaRefunder == nil || exp.PlatformExperimentID == "" {
		return
	}
	elapsedHours, err := w.observedElapsedHours(ctx, exp)
	if err != nil {
		w.logger.Error("job_watcher: observed elapsed hours", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	// Billed per accelerator type actually used (see metricsdb.ObservedGPUCost), not one flat
	// rate over elapsedHours — correct even if this job was rescheduled onto a different
	// accelerator type mid-run.
	gpuObserved, err := metricsdb.ObservedGPUCost(ctx, w.metricsDBURL, exp.ID, exp.GPUCount, time.Now().UTC(), observedMaxLookback, w.observedGapCap, w.observedStep)
	if err != nil {
		w.logger.Error("job_watcher: observed GPU cost", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	if err := w.quotaRefunder.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, domain.ResourceGPUHours, exp.CapacityTier, gpuObserved); err != nil {
		w.logger.Error("job_watcher: refund quota", zap.String("id", exp.ID), zap.Error(err))
	}

	if exp.EstimatedDurationHours <= 0 {
		return
	}
	observedFraction := elapsedHours / exp.EstimatedDurationHours
	for _, d := range []struct {
		resourceType domain.ResourceType
		estimated    float64
	}{
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours},
	} {
		if d.estimated <= 0 {
			continue
		}
		if err := w.quotaRefunder.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, d.resourceType, exp.CapacityTier, observedFraction*d.estimated); err != nil {
			w.logger.Error("job_watcher: refund quota", zap.String("id", exp.ID), zap.String("resource", string(d.resourceType)), zap.Error(err))
		}
	}
}
