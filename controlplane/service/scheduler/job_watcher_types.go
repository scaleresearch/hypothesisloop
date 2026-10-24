package scheduler

// JobWatcher reads what clusters report about experiments in SUBMITTED state and drives the DB
// transitions SUBMITTED → RUNNING → COMPLETED/FAILED. It reads only JobReports, so it has no
// idea whether jobs are actually running as Kubernetes Jobs, Slurm allocations, or anything else.
//
// Responsibility split:
//   - JobWatcher: reported job status → DB status sync and quota accounting on completion.
//   - Controller (service/controller): policy-driven eviction (silence,
//     quota exhaustion, metric decline). Terminates jobs that are running but should stop.
//
// JobWatcher is only concerned with jobs that finish naturally or are already gone;
// it never initiates termination itself.

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// JobStatusStore is the persistence interface needed by JobWatcher.
type JobStatusStore interface {
	clusterSettings
	terminations
	jobRows
	ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error)
	// MarkStarted transitions SUBMITTED -> RUNNING. Returns false (no error) if the
	// experiment already left that state (e.g. cancelled/evicted concurrently) — the caller
	// must skip every other onRunning side effect in that case.
	MarkStarted(ctx context.Context, id string) (bool, error)
	// TransitionStatus ends a job that ran, with the reason the status is announced with, only
	// when current status matches from. Returns false (no error) if the row was already in a
	// different state — used to prevent double-refunds when natural completion and controller
	// eviction race.
	TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	// RequeueForRetry returns a failed job that is still SUBMITTED or RUNNING to QUEUED for a
	// fresh attempt and spends one of its max_retries, both in one guarded UPDATE. Returns false
	// when the budget is exhausted or another writer got there first.
	RequeueForRetry(ctx context.Context, id string, maxAttemptsBefore int) (bool, error)
}

// QuotaSettler durably writes an experiment's observed usage so far across every resource
// dimension (service/settlement.Settler): an absolute, idempotent set of what the metrics store
// observed, meaningful for a job that runs again as for one that ended. Safe to retry.
type QuotaSettler interface {
	Settle(ctx context.Context, exp *domain.Experiment) error
}

// JobReports is what JobWatcher reads of what clusters report about jobs.
type JobReports interface {
	clusterIDs
	autoscalers
	// LatestJobPhase's found=false means the job's cluster has reported no status within the
	// liveness window, which every job is in between its placement and its runtime's next report.
	LatestJobPhase(ctx context.Context, exp *domain.Experiment) (phase domain.JobPhase, found bool, err error)
	// GetAdmittedAcceleratorType's found=false means no sample of this admission names a type yet.
	GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (acceleratorType domain.AcceleratorType, found bool, err error)
	// LatestPhaseDetail is the runtime's latest explanation for why a job has not started (see
	// domain.PhaseDetail), plus the gang-readiness facts checkScaleUpDeadline needs.
	// found=false means no runtime has reported one yet.
	LatestPhaseDetail(ctx context.Context, exp *domain.Experiment) (reason, message string, restartCount int32, scheduledNodes int32, found bool, err error)
	// PhaseReportedSince is when exp's current attempt first reported reason. found=false means
	// it never did.
	PhaseReportedSince(ctx context.Context, exp *domain.Experiment, reason string) (since time.Time, found bool, err error)
	// LogTail is up to the last n lines of output exp's runtime last reported, empty when it
	// reported none.
	LogTail(ctx context.Context, exp *domain.Experiment, n int) ([]string, error)
}

// JobWatcher performs periodic stateless passes over durable desired state and drives lifecycle
// transitions from the latest cluster reports.
type JobWatcher struct {
	store        JobStatusStore
	reports      JobReports
	logger       *zap.Logger
	settler      QuotaSettler // durably settles final usage on job end
	pollInterval time.Duration
	// stuckPendingTimeout bounds how long a job may stay SUBMITTED without reporting
	// RUNNING before it is evicted with reason stuck_pending and fully refunded. It is the global
	// value; a cluster with its own in the cluster's settings is judged by that, see stuckPendingTimeoutFor.
	// For a job that has not landed it counts from when the wait its runtime reports began, see
	// stuckPendingOverdue.
	stuckPendingTimeout time.Duration
	// startTimeout bounds how long a placed job the runtime reports as pending may take to start.
	startTimeout time.Duration

	// scaleUpTimeout is the global default deadline (scheduler.scale_up_timeout_seconds) for an
	// incomplete gang (scheduled_nodes < Nodes()) on an autoscaler-enabled cluster with no
	// per-cluster override in the cluster's settings.
	scaleUpTimeout time.Duration

	// freed wakes admission when a job lets go of what it held, so its room is refilled without
	// waiting for the loop's heartbeat.
	freed func()
}

// NewJobWatcher constructs a JobWatcher.
func NewJobWatcher(store JobStatusStore, reports JobReports, settler QuotaSettler, logger *zap.Logger) *JobWatcher {
	if settler == nil {
		panic("scheduler: NewJobWatcher requires a quota settler — it is the only path terminal usage is written by")
	}
	return &JobWatcher{
		store:   store,
		reports: reports,
		settler: settler,
		logger:  logger,
	}
}

// WithCapacityFreed has every job the watcher ends or requeues call wake.
func (w *JobWatcher) WithCapacityFreed(wake func()) *JobWatcher {
	w.freed = wake
	return w
}

func (w *JobWatcher) wakeAdmission() {
	if w.freed != nil {
		w.freed()
	}
}

func (w *JobWatcher) WithPollInterval(d time.Duration) *JobWatcher {
	w.pollInterval = d
	return w
}

// WithStuckPendingTimeout sets how long a job may stay SUBMITTED without reporting
// RUNNING before job_watcher evicts it as stuck_pending.
func (w *JobWatcher) WithStuckPendingTimeout(d time.Duration) *JobWatcher {
	w.stuckPendingTimeout = d
	return w
}

// WithStartTimeout sets how long a placed job may stay pending, pulling its image or creating
// its container, before job_watcher evicts it as stuck_pending.
func (w *JobWatcher) WithStartTimeout(d time.Duration) *JobWatcher {
	w.startTimeout = d
	return w
}

// WithScaleUpTimeout sets the autoscaler-aware placement deadline's (autoscaler.md) global
// default. d must be under domain.MaxScaleUpTimeoutSeconds (the rendezvous-timeout
// invariant) — config load-time validation already enforces this on scheduler.scale_up_timeout_seconds.
func (w *JobWatcher) WithScaleUpTimeout(d time.Duration) *JobWatcher {
	w.scaleUpTimeout = d
	return w
}
