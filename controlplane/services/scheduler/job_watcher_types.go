package scheduler

// JobWatcher watches backend-executed workloads for experiments in SUBMITTED or ADMITTED
// state and drives the DB transitions SUBMITTED/ADMITTED → RUNNING → COMPLETED/FAILED.
// It talks only to the workload.Backend interface, so it has no idea whether jobs are
// actually running as Kubernetes Jobs, Slurm allocations, or anything else.
//
// Responsibility split:
//   - JobWatcher: backend workload status → DB status sync and quota accounting on completion.
//   - Controller (services/controller): policy-driven eviction (silence,
//     quota exhaustion, metric decline). Terminates jobs that are running but should stop.
//
// JobWatcher is only concerned with jobs that finish naturally or are already gone;
// it never initiates termination itself.

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
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
	// TransitionTerminal atomically sets status and eviction_reason together in one UPDATE,
	// guarded on from. Used by evictNeverStarted so an eviction can never persist a status
	// change with its reason lost (e.g. a crash between the two separate writes).
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// MarkQuotaSettled records that a terminal experiment's final observed usage has been
	// durably written — see services/settlement. Only called after that write succeeds.
	MarkQuotaSettled(ctx context.Context, id string) error
}

// QuotaSettler durably writes a terminal experiment's final observed usage across every
// resource dimension — see services/settlement.Settler. Idempotent and safe to retry.
type QuotaSettler interface {
	Settle(ctx context.Context, exp *domain.Experiment) error
}

// JobBackendClient is the narrow slice of workload.Backend that JobWatcher needs. It is
// deliberately backend-agnostic: JobWatcher never assumes Kubernetes (or any other
// execution engine) underneath. Implemented by the PostgreSQL/metrics desired-state backend.
type JobBackendClient interface {
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (workload.JobPhase, error)
	GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, error)
}

// PhaseDetailer is implemented by backends that can report a runtime's latest explanation for
// why a job hasn't started (see domain.PhaseDetail). Optional, checked with a type assertion
// the same way agentloop.PhaseDetailer is on the runtime side — JobBackendClient stays narrow
// for any backend that doesn't have this to report.
type PhaseDetailer interface {
	PollPhaseDetail(ctx context.Context, exp *domain.Experiment) (reason, message string, restartCount int32, found bool, err error)
}

// JobWatcher performs periodic stateless passes over durable desired state and drives lifecycle
// transitions from the latest backend observations.
type JobWatcher struct {
	store        JobStatusStore
	backend      JobBackendClient
	logger       *zap.Logger
	settler      QuotaSettler // optional; durably settles final usage on job end
	pollInterval time.Duration
	// stuckPendingTimeout bounds how long a job may stay SUBMITTED/ADMITTED without reporting
	// RUNNING before it is evicted with reason stuck_pending and fully refunded.
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
		pollInterval: 0,
	}
}

func (w *JobWatcher) WithPollInterval(d time.Duration) *JobWatcher {
	w.pollInterval = d
	return w
}

// WithStuckPendingTimeout sets how long a job may stay SUBMITTED/ADMITTED without reporting
// RUNNING before job_watcher evicts it as stuck_pending.
func (w *JobWatcher) WithStuckPendingTimeout(d time.Duration) *JobWatcher {
	w.stuckPendingTimeout = d
	return w
}

// WithQuotaSettler attaches the durable settler used to write final observed usage on job
// completion or stuck-pending eviction.
func (w *JobWatcher) WithQuotaSettler(s QuotaSettler) *JobWatcher {
	w.settler = s
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
