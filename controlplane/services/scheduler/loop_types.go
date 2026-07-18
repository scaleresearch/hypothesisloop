package scheduler

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// exp's admission footprint is domain.Experiment.Footprint() — CPU (millicores) and its
// accelerator (if any), jointly. One footprint shared by the SUBMITTED-footprint subtraction,
// both admission passes, and preemption victim accounting, so all three always agree on what a
// job actually costs across every dimension it requests, not just one flavor.
//
// For a distributed job (JobSpec.NumNodes > 1), exp.GPUCount is already the job's TOTAL
// footprint — JobSpec.TotalGPUs(), i.e. per-node GPUCount x NumNodes, set once at submission
// (see handler.go) — so tick()'s check (domain.Fits(avail[cluster], fp)) already requires the
// *whole* job's demand to fit before admitting anything, and submitJob creates the entire k8s
// Job (every rank/index) in a single CreateWorkload call. Admission is therefore atomic:
// either the whole job's demand fits and every rank is created together, or none of it is
// created and the job stays QUEUED — there is no code path that admits/creates a subset of a
// distributed job's ranks.
//
// KNOWN GAP: "fits" here means the cluster's *aggregate* free capacity per dimension is
// enough, not that NumNodes *distinct* hosts each have a full rank's footprint free. Distributed
// jobs get a hard per-host anti-affinity rule at Job-build time (see job_affinity.go's
// spreadAcrossHosts), so aggregate headroom spread across fewer hosts than NumNodes can still
// leave some ranks permanently Pending after admission — see job_affinity.go's KNOWN GAP comment
// for the failure mode. A real fix needs per-node (not just per-cluster) capacity reporting from
// GetFlavorCapacity, which does not exist yet; that's future work, out of scope here.

// LoopStore is the persistence interface required by the scheduler loop.
type LoopStore interface {
	ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListAdmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
	MarkQueued(ctx context.Context, id string) error
	// MarkSubmitted transitions id to SUBMITTED and persists clusterName atomically with that
	// transition — the cluster choice and the capacity accounting for it must land together,
	// so a later tick never sees an experiment claiming capacity on a cluster it isn't
	// actually recorded against.
	MarkSubmitted(ctx context.Context, id, clusterName string) error
	// RequeuePreempted returns id to QUEUED and overwrites its duration plus every resource
	// estimate (GPU/CPU/RAM/storage) with the caller's proportionally rescaled remaining amounts
	// — see the store implementation's doc comment for why all four must move together.
	RequeuePreempted(ctx context.Context, id string, remainingHours, newCostT4H, newCPUCoreHours, newRAMGBHours, newStorageGBHours float64) error
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// UpdateNotAdmittedReason explains why a QUEUED job was skipped on its most recent tick
	// (capacity_unavailable | outranked | summary_gate); pass "" to clear it on admission.
	UpdateNotAdmittedReason(ctx context.Context, id, reason string) error
	// HasUnsummarizedCompleted returns true when the agent has a COMPLETED experiment
	// without a summary in the given platform experiment. Used to enforce the summary gate
	// during admission so that batch-submitted jobs cannot bypass it.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	// UpsertPendingReservation durably claims exp's admission footprint against clusterName
	// (see pending_capacity_reservations' schema comment) — written alongside MarkSubmitted so
	// a second tick before the pod actually exists can't double-admit into the same capacity.
	UpsertPendingReservation(ctx context.Context, experimentID, clusterName string, fp domain.Footprint) error
	// DeletePendingReservation removes id's reservation once its fate is resolved (rollback to
	// QUEUED after a failed CreateWorkload — see submitJob). No-op if none exists.
	DeletePendingReservation(ctx context.Context, id string) error
	// ListPendingReservationsByCluster sums every outstanding reservation per cluster — see
	// pending_capacity_reservations' schema comment. tick() subtracts this from live capacity on
	// top of the existing SUBMITTED/ADMITTED/RUNNING accounting.
	ListPendingReservationsByCluster(ctx context.Context) (map[string]domain.Footprint, error)
}

// LoopQuotaStore handles quota bookkeeping for the loop. Preemption requeues the victim without
// refunding (see RequeuePreempted) — its estimate stays reserved until the job's eventual
// completion or eviction writes its real observed cost, same as every other termination path.
type LoopQuotaStore interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	// CorrectReservation overwrites experimentID's own not-yet-final reservation for
	// resourceType/tier with a new absolute amount — used on preemption requeue to replace a
	// stale original-estimate reservation with the revised, shortened one (see
	// metricsdb.UsageTracker.SetReservation).
	CorrectReservation(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
}

// LoopWorkloadClient is the backend-agnostic interface needed by the loop. No DeleteWorkload:
// preemption requeues the victim (status -> QUEUED) *before* calling WaitForJobDeletion —
// that status change is itself what tells the cluster-agent the Job should go away.
type LoopWorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error
	// GetFlavorCapacity returns available capacity as a canonical domain.Footprint per cluster
	// — guaranteed[cluster] and burst[cluster] — every configured cluster has an entry, even at
	// zero, so the loop can enumerate clusters from these maps alone.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error)
}

// Reprioritizer recomputes priority scores for all queued experiments.
// Implemented by *Service. Called after every admission pass so that new information
// (novelty changes, elapsed time, etc.) is always reflected in the queue ordering.
type Reprioritizer interface {
	RePrioritize(ctx context.Context) error
}

// Loop is the single-threaded scheduler loop. One goroutine, no concurrent ticks.
type Loop struct {
	store          LoopStore
	quota          LoopQuotaStore
	workload       LoopWorkloadClient
	reprioritizer  Reprioritizer // optional; called after each admission pass
	trigger        chan struct{}
	logger         *zap.Logger
	heartbeat      time.Duration
	preemptTimeout time.Duration
	// guaranteedFairnessWindow quantizes queued_at for sortGuaranteed's age-bucket fairness
	// tiebreak — see sortGuaranteed's doc comment.
	guaranteedFairnessWindow time.Duration

	// ticking enforces the single-threaded invariant tick() and preempt() capacity accounting
	// depend on for correctness (see competetors/SYNTHESIS_GAPS_AND_PLAN.md item #2). Unlike
	// cluster-agent's reconcile loop — which is safe to run concurrently because it's purely
	// declarative (diff desired vs. actual, converge) — tick() is a read-then-decide-then-write
	// capacity accounting operation: two concurrent ticks could each read the same "1 GPU free"
	// and both admit a job against it, a real double-booking, not an idempotent retry. This
	// turns that invariant into an enforced guarantee instead of a comment: it panics loudly on
	// reentrancy rather than silently double-admitting/double-preempting.
	ticking atomic.Bool

	// metricsDBURL, observedGapCap, observedStep let preempt() rank victims by real observed
	// runtime (metricsdb.ObservedElapsedHours) instead of wall-clock ElapsedHours() — a job stuck
	// in a reschedule/node-death gap for most of its wall-clock life isn't "the one that's made
	// the most progress" just because it was admitted a while ago.
	metricsDBURL   string
	observedGapCap time.Duration
	observedStep   time.Duration
}

// NewLoop constructs a Loop.
func NewLoop(store LoopStore, quota LoopQuotaStore, workloadClient LoopWorkloadClient, logger *zap.Logger) *Loop {
	return &Loop{
		store:                    store,
		quota:                    quota,
		workload:                 workloadClient,
		trigger:                  make(chan struct{}, 1),
		logger:                   logger,
		heartbeat:                10 * time.Second,
		preemptTimeout:           30 * time.Second,
		guaranteedFairnessWindow: 60 * time.Second,
	}
}

func (l *Loop) WithHeartbeat(d time.Duration) *Loop {
	l.heartbeat = d
	return l
}

func (l *Loop) WithPreemptTimeout(d time.Duration) *Loop {
	l.preemptTimeout = d
	return l
}

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step preempt() uses to rank
// victims by real observed runtime — see metricsdb.ObservedElapsedHours. Pass the same values
// every other observed-time consumer in this deployment uses.
func (l *Loop) WithObservedTimeConfig(metricsDBURL string, gapCap, step time.Duration) *Loop {
	l.metricsDBURL = metricsDBURL
	l.observedGapCap = gapCap
	l.observedStep = step
	return l
}

func (l *Loop) WithGuaranteedFairnessWindow(d time.Duration) *Loop {
	l.guaranteedFairnessWindow = d
	return l
}

// WithReprioritizer attaches a Reprioritizer that runs after every admission pass.
func (l *Loop) WithReprioritizer(r Reprioritizer) *Loop {
	l.reprioritizer = r
	return l
}

// Trigger wakes the loop. Non-blocking: if already pending, does nothing.
func (l *Loop) Trigger() {
	select {
	case l.trigger <- struct{}{}:
	default:
	}
}

// Start runs the loop goroutine until ctx is cancelled.
func (l *Loop) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(l.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.trigger:
			case <-ticker.C:
			}
			if err := l.tick(ctx); err != nil {
				l.logger.Error("scheduler loop tick", zap.Error(err))
			}
		}
	}()
}
