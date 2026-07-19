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
	UpdateAdmittedFlavor(ctx context.Context, id string, acceleratorType domain.AcceleratorType, estimatedCostAccH float64) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// MarkQuotaSettled records that a terminal experiment's final observed usage has been
	// durably written — see services/settlement. Only called after that write succeeds.
	MarkQuotaSettled(ctx context.Context, id string) error
	// DeletePendingReservation removes id's durable pending-capacity claim (see
	// pending_capacity_reservations' schema comment) once its fate is resolved: its pod is
	// confirmed running (live capacity now covers it directly) or it left SUBMITTED/ADMITTED
	// without ever needing one counted (stuck-pending eviction, or a terminal report arriving
	// before RUNNING was ever observed). No-op if none exists.
	DeletePendingReservation(ctx context.Context, id string) error
}

// QuotaSettler durably writes a terminal experiment's final observed usage across every
// resource dimension — see services/settlement.Settler. Idempotent and safe to retry.
type QuotaSettler interface {
	Settle(ctx context.Context, exp *domain.Experiment) error
}

// JobQuotaAdjuster debits extra cost if a job ever ran on a more expensive
// flavor than requested (e.g. H100 request admitted on H200). Accelerator-hours only — flavor
// substitution is an accelerator-hardware-tier concept, not one that applies to CPU/RAM/storage.
type JobQuotaAdjuster interface {
	DebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, costAccH float64) error
}

// JobBackendClient is the narrow slice of workload.Backend that JobWatcher needs. It is
// deliberately backend-agnostic: JobWatcher never assumes Kubernetes (or any other
// execution engine) underneath. Implemented by workload.Backend (in production,
// *workload.ClusterSet, but any Backend implementation satisfies it unchanged).
type JobBackendClient interface {
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (workload.JobPhase, error)
	GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) domain.AcceleratorType
}

// JobWatcher starts a per-experiment goroutine for every SUBMITTED or ADMITTED experiment
// and drives its lifecycle to RUNNING and then COMPLETED/FAILED as the backend workload progresses.
type JobWatcher struct {
	store         JobStatusStore
	backend       JobBackendClient
	logger        *zap.Logger
	settler       QuotaSettler     // optional; durably settles final usage on job end
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

// WithQuotaSettler attaches the durable settler used to write final observed usage on job
// completion or stuck-pending eviction.
func (w *JobWatcher) WithQuotaSettler(s QuotaSettler) *JobWatcher {
	w.settler = s
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
