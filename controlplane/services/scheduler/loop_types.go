package scheduler

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// exp's admission footprint is domain.Experiment.Footprint() — CPU (millicores) and its
// accelerator (if any), jointly. One footprint shared by the SUBMITTED-footprint subtraction,
// both admission passes, and preemption victim accounting, so all three agree on cost.
//
// For a distributed job, exp.AcceleratorCount is already the job's TOTAL footprint (per-node
// count x NumNodes, set at submission), so tick()'s fit check requires the whole job's demand to
// fit before admitting, and submitJob creates every rank in one CreateWorkload call. Admission
// is therefore atomic: either the whole job fits and is created together, or none of it is and
// the job stays QUEUED.
//
// Distributed accelerator jobs with hard host spreading are additionally checked against fresh
// per-node metrics, so capacity concentrated on too few hosts cannot pass admission.

// LoopStore is the persistence interface required by the scheduler loop.
type LoopStore interface {
	ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListAdmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
	MarkQueued(ctx context.Context, id, reason string) error
	ClaimSubmitted(ctx context.Context, id, clusterName string, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error)
	// RequeuePreempted returns id to QUEUED and overwrites its duration plus every resource
	// estimate with the caller's proportionally rescaled remaining amounts — see the store
	// implementation's doc comment for why all four must move together.
	RequeuePreempted(ctx context.Context, id string, remainingHours, newCostAccH, newCPUCoreHours, newRAMGBHours, newStorageGBHours float64) error
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	UpdateNotAdmittedReason(ctx context.Context, id, reason string) error
	// HasUnsummarizedCompleted enforces the summary gate during admission so batch-submitted
	// jobs cannot bypass it.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error)
}

// LoopQuotaStore handles quota bookkeeping for the loop. Preemption requeues the victim without
// refunding (see RequeuePreempted) — its estimate stays reserved until completion/eviction
// writes its real observed cost, same as every other termination path.
type LoopQuotaStore interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	ReserveAdmittedFlavor(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64) error
}

// LoopWorkloadClient is the backend-agnostic interface needed by the loop. No DeleteWorkload:
// preemption requeues the victim (status -> QUEUED) and stops there — that status change alone
// tells the cluster-agent the Job should go away; the loop never waits for it.
type LoopWorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	// GetFlavorCapacity returns available capacity as a domain.Footprint per cluster —
	// guaranteed[cluster] and burst[cluster] — every configured cluster has an entry, even at
	// zero, so the loop can enumerate clusters from these maps alone.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error)
	GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)
}

// Reprioritizer recomputes priority scores for all queued experiments. Implemented by *Service.
// Called after every admission pass so new information is reflected in queue ordering.
type Reprioritizer interface {
	RePrioritize(ctx context.Context) error
}

// Loop is the single-threaded scheduler loop. One goroutine, no concurrent ticks.
type Loop struct {
	store         LoopStore
	quota         LoopQuotaStore
	workload      LoopWorkloadClient
	reprioritizer Reprioritizer // optional; called after each admission pass
	trigger       chan struct{}
	logger        *zap.Logger
	heartbeat     time.Duration
	// guaranteedFairnessWindow quantizes queued_at for sortGuaranteed's age-bucket fairness
	// tiebreak — see sortGuaranteed's doc comment.
	guaranteedFairnessWindow time.Duration

	// ticking enforces the single-threaded invariant tick()/preempt() capacity accounting needs.
	// Unlike cluster-agent's reconcile loop (safe concurrently since it's purely declarative),
	// tick() is read-then-decide-then-write: two concurrent ticks could each read "1 accelerator
	// free" and both admit against it — a real double-booking. This panics loudly on reentrancy
	// instead of silently double-admitting/double-preempting.
	ticking atomic.Bool

	// metricsDBURL, observedGapCap, observedStep let preempt() rank victims by real observed
	// runtime instead of wall-clock ElapsedHours() — a job stuck in a reschedule/node-death gap
	// isn't "the one that's made the most progress" just because it was admitted a while ago.
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
		guaranteedFairnessWindow: 60 * time.Second,
	}
}

func (l *Loop) WithHeartbeat(d time.Duration) *Loop {
	l.heartbeat = d
	return l
}

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step preempt() uses to rank
// victims by real observed runtime. Pass the same values every other observed-time consumer in
// this deployment uses.
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
