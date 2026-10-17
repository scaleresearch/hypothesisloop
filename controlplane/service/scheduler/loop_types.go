package scheduler

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// exp's admission footprint is domain.Experiment.Footprint() — CPU (millicores) and its
// accelerator (if any), jointly. One footprint shared by the SUBMITTED-footprint subtraction,
// both admission passes, and preemption victim accounting, so all three agree on cost.
//
// For a distributed job, exp.AcceleratorCount is already the job's TOTAL footprint (per-node
// count x NumNodes, set at submission), so tick()'s fit check requires the whole job's demand to
// fit before admitting, and submitJob claims every rank in one ClaimSubmitted. Admission is
// therefore atomic: either the whole job fits and is claimed together, or none of it is and the
// job stays QUEUED.
//
// Distributed accelerator jobs with hard host spreading are additionally checked against fresh
// per-node metrics, so capacity concentrated on too few hosts cannot pass admission.

// LoopStore is the persistence interface required by the scheduler loop.
type LoopStore interface {
	ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
	// ClaimSubmitted persists resolvedJob (exp.ResolvedJob, required) alongside the SUBMITTED
	// status transition, in the same transaction — see domain.Experiment.ResolvedJob's doc comment.
	ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error)
	// RequeuePreempted returns id to QUEUED and overwrites its duration plus the accelerator-hours
	// estimate with the caller's proportionally rescaled remaining amounts — see the store
	// implementation's doc comment for why both must move together.
	RequeuePreempted(ctx context.Context, id string, remainingHours, newCostAccH float64) (bool, error)
	UpdateNotAdmittedReason(ctx context.Context, id, reason string) error
	// MarkHeldJobs tells the owner of every job that has waited longer than its estimated
	// duration and longer than minimum why it is held, once per job.
	MarkHeldJobs(ctx context.Context, minimum time.Duration) error
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// GetPlatformExperiment supplies the current stage, whose max_job_hours gates admission of
	// jobs queued before the ladder moved.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	// ListCapacityClaimedExperiments returns every experiment that has already claimed
	// accelerator capacity toward the nodes it landed on — RUNNING and SUBMITTED, not
	// just RUNNING — see resolutionCache.loadClaimed for why the narrower RUNNING-only view
	// undercounts "installed" capacity when a job's per-accelerator share is measured.
	ListCapacityClaimedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	// GetClusterSettings returns the operator overrides for clusterID (scale_up_timeout_seconds,
	// max_speculative_accelerators), or nil if none were ever set — see domain.ClusterSettings.
	GetClusterSettings(ctx context.Context, clusterID string) (*domain.ClusterSettings, error)
	// RegisteredClusters returns every registered cluster by name, with its org and whether the
	// platform shares it: what eligibleClusters decides placement by org against.
	RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error)
	// RecentlyTriedClusters returns the cluster_ids ANY experiment failed a speculative scale-up
	// attempt on within ttl — the cross-job half of the tried-cluster backoff (autoscaler.md line
	// 109): a job never speculates onto a node group another job just watched fail to deliver.
	RecentlyTriedClusters(ctx context.Context, ttl time.Duration) (map[string]bool, error)
	// AssignEnvironment is the environment a job's commit declares, which it waits for, with its
	// build assigned to one of live and kept by one of holding (db.Store.AssignEnvironment).
	AssignEnvironment(ctx context.Context, key string, live, holding []string, lease time.Duration) (*domain.Environment, error)
	ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (domain.Termination, error)
	// EndQueuedSettled ends a queued job and releases its hold in one write (db.Store.EndQueuedSettled).
	EndQueuedSettled(ctx context.Context, id string, to domain.ExperimentStatus, reason string) (bool, error)
}

// LoopQuotaStore handles quota bookkeeping for the loop. Preemption requeues the victim without
// refunding (see RequeuePreempted) — its remaining estimate stays reserved, and the hours it
// already burned are written as observed usage at requeue time (see Loop.settleStint), so the two
// figures together always cover the whole job.
type LoopQuotaStore interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	ReserveAdmittedAcceleratorType(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, acceleratorCount int) error
}

// ObservedState is the actual state the loop reads back from the metrics store: how long a job has
// really been running, and which node it landed on. Postgres holds the desired state and answers
// what should exist; this answers what does.
//
// It is an interface for the same reason LoopStore is — the loop states what it needs, and the
// datastore package supplies it. gapCap is deployment configuration rather than a per-call
// decision, so it lives in the implementation instead of every call site.
type ObservedState interface {
	ObservedElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time) (float64, error)
	ObservedStintElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time) (float64, error)
	LatestExperimentNode(ctx context.Context, experimentID string, createdAt, now time.Time) (node string, found bool, err error)
	// RecordAdmission marks the start of a run, so that time queued before it is never billed.
	RecordAdmission(ctx context.Context, experimentID string, at time.Time) error
}

// ClusterCapacity is what the loop reads of the clusters' capacity: live reports net of desired
// footprint. Placing a job is its ClaimSubmitted row alone, which the cluster reconciles against.
type ClusterCapacity interface {
	// GetAcceleratorTypeCapacity returns per connected cluster what can be claimed now (available) and
	// what is free once every job already told to stop is gone (desiredFree). The loop
	// enumerates clusters from these maps.
	GetAcceleratorTypeCapacity(ctx context.Context) (available, desiredFree map[string]domain.Footprint, err error)
	GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error)
	// GetNodeResourceCapacity reports free CPU/memory/storage per node — see
	// placement.Reserve, which must prove a job fits one node in every dimension.
	GetNodeResourceCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)
	// GetNodeTotalCapacity reports each node's TOTAL installed capacity (not just what's
	// currently free — see GetNodeResourceCapacity) for CPU/memory/storage, keyed the same way:
	// cluster -> node -> domain.NodeResource* -> canonical units. Used to compute a job's
	// proportional share of the node it lands on (see resolveClusterLocalResources).
	GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	// GetMultiNodeCapability reports which clusters can run a job spanning more than one node.
	// A cluster absent from the map is treated as single-node only.
	GetMultiNodeCapability(ctx context.Context) (map[string]bool, error)
	// GetAutoscalerCapability reports which clusters sit behind a native autoscaler.
	// A cluster absent from the map is treated as not-autoscaled.
	GetAutoscalerCapability(ctx context.Context) (map[string]bool, error)
	// GetClusterIDs reports each connected cluster's runtime-derived stable identity, keyed by
	// cluster_name. A cluster absent from the map has not yet reported one.
	GetClusterIDs(ctx context.Context) (map[string]string, error)
	// GetLiveClusters reports true for the clusters that reported within the connected window,
	// those an environment's build may be assigned to, and false for those that reported only
	// within domain.EnvironmentBuildHold, which keep the builds they hold.
	GetLiveClusters(ctx context.Context) (map[string]bool, error)
	// GetTotalCapacity reports each cluster's installed capacity — the per-accelerator
	// CPU/memory share the disbalance evictor measures requests against. Clusters without a
	// fresh report are absent, never zero-filled. Only read when that evictor is enabled.
	GetTotalCapacity(ctx context.Context) (map[string]domain.Footprint, error)
}

// ExperimentEvictor terminates a RUNNING experiment through the platform's one canonical
// eviction path (status transition plus quota settlement). Implemented by *Service; kept as an
// interface, like Reprioritizer, so the loop never grows its own second accounting path.
type ExperimentEvictor interface {
	EvictExperiment(ctx context.Context, id string, reason domain.EvictionReason) error
}

// LoopQuotaSettler durably writes an experiment's observed usage so far — see
// service/settlement.Settler, the same implementation every other accounting path uses. An
// absolute, idempotent set of what the metrics store has actually observed, so it is meaningful
// for a job that is still going to run again, not only for a terminal one.
type LoopQuotaSettler interface {
	Settle(ctx context.Context, exp *domain.Experiment) error
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
	clusters      ClusterCapacity
	grace         domain.GraceLimits
	podResources  domain.PodResources
	reprioritizer Reprioritizer // called after each admission pass
	trigger       chan struct{}
	logger        *zap.Logger
	heartbeat     time.Duration
	// guaranteedFairnessWindow quantizes queued_at for sortGuaranteed's age-bucket fairness
	// tiebreak — see sortGuaranteed's doc comment.
	guaranteedFairnessWindow time.Duration
	// heldJobNotice is the least a job waits in the queue before its owner is told it is held.
	heldJobNotice time.Duration

	// ticking enforces the single-threaded invariant tick()/preempt() capacity accounting needs.
	// Unlike cluster-agent's reconcile loop (safe concurrently since it's purely declarative),
	// tick() is read-then-decide-then-write: two concurrent ticks could each read "1 accelerator
	// free" and both admit against it — a real double-booking. This panics loudly on reentrancy
	// instead of silently double-admitting/double-preempting.
	ticking atomic.Bool

	// observed lets preempt() rank victims by real observed runtime instead of wall-clock
	// ElapsedHours() — a job stuck in a reschedule/node-death gap isn't "the one that's made the
	// most progress" just because it was admitted a while ago.
	observed ObservedState

	// evictor and disbalanceTolerance drive the resource-disbalance evictor (see
	// loop_disbalance.go). Both are required: the pass has one behaviour, not an on and an off
	// one. It is the only thing in the loop that terminates a running job the queue never asked
	// to stop, which is an argument for how carefully it must prove its case — every guard in
	// evictDisbalanced — not for leaving stranded accelerators stranded on some deployments.
	evictor             ExperimentEvictor
	disbalanceTolerance float64

	// settler writes a preemption victim's already-burned hours as observed usage when it is
	// requeued. Required: without it those hours are in no figure at all — the requeued row only
	// reserves the shortened remainder — and the agent can re-admit against budget it has spent.
	settler LoopQuotaSettler

	// pendingEvictions tracks disbalance victims whose termination request has been issued but
	// whose freed capacity a fresh GetAcceleratorTypeCapacity/GetAcceleratorCapacityByNode read cannot yet
	// see — the node/agent that owns the accelerator hasn't reported it back yet, even though the
	// victim is already gone from ListRunningExperiments. Without this, every tick until that
	// report lands re-derives the same shortage and evicts a fresh set of victims to free capacity
	// that is already on its way back — see loop_disbalance.go's applyPendingEvictions and
	// evictDisbalanced. Keyed by experiment ID; single-threaded with tick() so no mutex is needed.
	pendingEvictions map[string]pendingEviction
	// evictionTTL bounds how long a pendingEvictions entry is trusted. There is no explicit
	// "capacity report caught up" signal to clear it on, so it is cleared by time instead: long
	// enough for the underlying node/agent to report the freed accelerator back across a few
	// ticks, short enough that a genuinely stuck report doesn't mask real capacity forever.
	evictionTTL time.Duration

	// triedClusterTTL bounds how long a cluster this job already failed a speculative attempt on
	// stays excluded from that job's candidate list — see speculativeCandidates.
	triedClusterTTL time.Duration
}

// DefaultEvictionTTL is evictionTTL's default: several scheduler heartbeats at the default
// interval, which is comfortably longer than the node-agent reconcile+report round trip an
// eviction needs to become visible as free capacity.
const DefaultEvictionTTL = 60 * time.Second

// NewLoop constructs a Loop.
func NewLoop(store LoopStore, quota LoopQuotaStore, clusters ClusterCapacity, grace domain.GraceLimits, logger *zap.Logger) *Loop {
	return &Loop{
		store:            store,
		quota:            quota,
		clusters:         clusters,
		grace:            grace,
		trigger:          make(chan struct{}, 1),
		logger:           logger,
		pendingEvictions: map[string]pendingEviction{},
		evictionTTL:      DefaultEvictionTTL,
	}
}

// WithQuotaSettler wires the settler used to record a preemption victim's consumed hours as
// observed usage at requeue time.
func (l *Loop) WithQuotaSettler(s LoopQuotaSettler) *Loop {
	l.settler = s
	return l
}

// WithPodResources is what admission writes into every job on a type beside what the job stated.
func (l *Loop) WithPodResources(p domain.PodResources) *Loop {
	l.podResources = p
	return l
}

// WithSpeculation wires speculative submission (autoscaler.md): guaranteed jobs
// that fit nowhere live may be submitted to an autoscaler-enabled cluster with no capacity yet,
// ahead of preemption. triedClusterTTL is scheduler.tried_cluster_ttl_seconds — how long a
// cluster a job already failed over from stays excluded from that job's next candidate search.
func (l *Loop) WithSpeculation(triedClusterTTL time.Duration) *Loop {
	l.triedClusterTTL = triedClusterTTL
	return l
}

func (l *Loop) WithHeartbeat(d time.Duration) *Loop {
	l.heartbeat = d
	return l
}

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step preempt() uses to rank
// victims by real observed runtime. Pass the same values every other observed-time consumer in
// this deployment uses.
// observedMetrics is the only production ObservedState: the metrics store, read with the loop's
// observation cadence.
type observedMetrics struct {
	metrics *metricsdb.Store
	gapCap  time.Duration
}

func (o observedMetrics) ObservedElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time) (float64, error) {
	return o.metrics.ObservedElapsedHours(ctx, experimentID, createdAt, now, o.gapCap)
}

func (o observedMetrics) ObservedStintElapsedHours(ctx context.Context, experimentID string, createdAt, now time.Time) (float64, error) {
	return o.metrics.ObservedStintElapsedHours(ctx, experimentID, createdAt, now, o.gapCap)
}

func (o observedMetrics) RecordAdmission(ctx context.Context, experimentID string, at time.Time) error {
	return o.metrics.RecordAdmission(ctx, experimentID, at)
}

func (o observedMetrics) LatestExperimentNode(ctx context.Context, experimentID string, createdAt, now time.Time) (string, bool, error) {
	return o.metrics.LatestExperimentNode(ctx, experimentID, createdAt, now)
}

func (l *Loop) WithObservedTimeConfig(metrics *metricsdb.Store, gapCap time.Duration) *Loop {
	l.observed = observedMetrics{metrics: metrics, gapCap: gapCap}
	return l
}

func (l *Loop) WithGuaranteedFairnessWindow(d time.Duration) *Loop {
	l.guaranteedFairnessWindow = d
	return l
}

func (l *Loop) WithHeldJobNotice(d time.Duration) *Loop {
	l.heldJobNotice = d
	return l
}

// WithDisbalanceEvictor enables the resource-disbalance evictor (loop_disbalance.go), which
// terminates running jobs whose CPU/memory/storage request is more than tolerance times their
// proportionate per-accelerator share while that request is provably stranding idle accelerators
// on their node. tolerance must be positive (config validation enforces it). Requires
// WithObservedTimeConfig, since victim selection needs the metrics store's job->node attribution.
func (l *Loop) WithDisbalanceEvictor(evictor ExperimentEvictor, tolerance float64) *Loop {
	l.evictor = evictor
	l.disbalanceTolerance = tolerance
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

// Start runs the loop until ctx is cancelled, each tick bounded by passTimeout.
//
// Panics on a loop that was never fully wired. The disbalance evictor has no disabled state — it
// is one of the passes a tick runs — so a nil evictor or a non-positive tolerance is a wiring
// mistake, not a configuration. Failing here, at startup, beats discovering it as a nil
// dereference on the first tick that finds a victim, hours into a run.
func (l *Loop) Start(ctx context.Context, passTimeout time.Duration) {
	if l.evictor == nil || l.disbalanceTolerance <= 0 {
		panic("scheduler: Loop started without WithDisbalanceEvictor — see loop_disbalance.go")
	}
	if l.settler == nil {
		panic("scheduler: Loop started without WithQuotaSettler — preempted jobs' consumed hours would be counted nowhere")
	}
	if l.reprioritizer == nil {
		panic("scheduler: Loop started without WithReprioritizer — queue order would never reflect new information")
	}
	if l.observed == nil {
		panic("scheduler: Loop started without WithObservedTimeConfig — preemption and disbalance eviction both read observed state")
	}
	if l.heartbeat <= 0 || l.guaranteedFairnessWindow <= 0 || l.heldJobNotice <= 0 || l.triedClusterTTL <= 0 {
		panic("scheduler: Loop started without WithHeartbeat, WithGuaranteedFairnessWindow, WithHeldJobNotice and WithSpeculation")
	}
	ticker := time.NewTicker(l.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.trigger:
		case <-ticker.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		if err := l.tick(passCtx); err != nil {
			l.logger.Error("scheduler loop tick", zap.Error(err))
		}
		cancel()
	}
}
