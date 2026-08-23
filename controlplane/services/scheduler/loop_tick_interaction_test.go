package scheduler

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// --- submitJob TOCTOU: a node the tick's decision relied on vanishes before the claim commits ---

// claimStore's ClaimSubmitted runs the real capacityAvailable callback against liveWorkload
// (a second, independent capacity view), exactly like the production store does inside its
// transaction — the whole point being that the callback re-reads live state rather than trusting
// what the tick decided.
type claimStore struct {
	LoopStore
	claimed     bool
	markQueued  string
	claimCalled bool
}

func (s *claimStore) ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.claimCalled = true
	ok, err := capacityAvailable(ctx, nil)
	if err != nil || !ok {
		return false, err
	}
	s.claimed = true
	return true, nil
}

func (s *claimStore) MarkQueued(_ context.Context, id, reason string) error {
	s.markQueued = reason
	return nil
}

// liveWorkload answers submitJob's re-check with whatever capacity is installed at call time —
// tests mutate it between building exp and invoking submitJob to simulate the cluster losing a
// node in the gap between the tick's placement decision and the claim.
type liveWorkload struct {
	LoopWorkloadClient
	guaranteed map[string]domain.Footprint
	burst      map[string]domain.Footprint
	nodeAccel  map[string]map[string]map[string]int64
	nodeRes    map[string]map[string]map[string]int64
	nodeLabels map[string]map[string]map[string]string
	created    bool
}

func (w *liveWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	return w.guaranteed, w.burst, nil
}
func (w *liveWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.nodeAccel, nil
}
func (w *liveWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.nodeRes, nil
}
func (w *liveWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return w.nodeLabels, nil
}
func (w *liveWorkload) CreateWorkload(context.Context, *domain.Experiment) error {
	w.created = true
	return nil
}

type submitQuota struct{ LoopQuotaStore }

func (submitQuota) ReserveAdmittedFlavor(context.Context, string, domain.AcceleratorType, float64) error {
	return nil
}

// The tick decided cluster-a fits (that decision isn't reproduced here — this test starts from
// "submitJob is now called for cluster-a"). Between that decision and this call, cluster-a's only
// qualifying node went away. submitJob's own re-check (ClaimSubmitted's capacityAvailable
// callback, which re-reads live per-node capacity via desiredPlacementFits) must catch this and
// refuse the claim — not persist SUBMITTED against a node that no longer exists.
func TestSubmitJobRejectsClaimWhenNodeVanishesBeforeClaim(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	exp.ClusterName = "cluster-a"

	store := &claimStore{}
	workload := &liveWorkload{
		guaranteed: map[string]domain.Footprint{"cluster-a": exp.Footprint()},
		burst:      map[string]domain.Footprint{"cluster-a": domain.NewFootprint()},
		// The node the tick counted on is simply gone by claim time — the cluster-wide scalar
		// still looks fine (it is derived from the same now-stale snapshot in a real run), but
		// the fresh per-node read used by the callback below reports it empty.
		nodeAccel: map[string]map[string]map[string]int64{"cluster-a": {}},
		nodeRes:   map[string]map[string]map[string]int64{"cluster-a": {}},
	}
	l := NewLoop(store, submitQuota{}, workload, zap.NewNop())

	err := l.submitJob(context.Background(), exp, "cluster-a", exp.AcceleratorType)
	if !errors.Is(err, errAdmissionCapacityChanged) {
		t.Fatalf("submitJob error = %v, want errAdmissionCapacityChanged", err)
	}
	if !store.claimCalled {
		t.Fatal("ClaimSubmitted was never invoked")
	}
	if store.claimed {
		t.Fatal("claim succeeded despite the node the placement relied on being gone by claim time")
	}
	if workload.created {
		t.Fatal("CreateWorkload ran even though the claim was rejected")
	}
}

// Same setup, but the node is still there at claim time: the claim must succeed and the workload
// must be created. This is the control for the test above — it proves the rejection there is
// because the node vanished, not because the harness always refuses.
func TestSubmitJobSucceedsWhenNodeStillPresentAtClaim(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	exp.ClusterName = "cluster-a"

	store := &claimStore{}
	workload := &liveWorkload{
		guaranteed: map[string]domain.Footprint{"cluster-a": exp.Footprint()},
		burst:      map[string]domain.Footprint{"cluster-a": domain.NewFootprint()},
		nodeAccel:  map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 1}}},
		nodeRes:    map[string]map[string]map[string]int64{"cluster-a": {"node-a": {}}},
	}
	l := NewLoop(store, submitQuota{}, workload, zap.NewNop())

	if err := l.submitJob(context.Background(), exp, "cluster-a", exp.AcceleratorType); err != nil {
		t.Fatalf("submitJob = %v, want nil: the node the placement relied on is still there", err)
	}
	if !store.claimed {
		t.Fatal("claim should have succeeded")
	}
	if !workload.created {
		t.Fatal("CreateWorkload should have run after a successful claim")
	}
}

// --- Tick-level: preemption must not run against a layout-only shortfall ---

// noRunningListStore's ListRunningExperiments legitimately gets called by evictDisbalanced (the
// node-aware pass, which is supposed to run for a layout-only shortfall) — so an empty running
// set is returned rather than failing the test on the call itself. The actual signal that
// preemption did NOT run is RequeuePreempted, which only preempt()'s victim-eviction step calls:
// see the explicit "skipping preemption: cluster has the capacity, the layout does not fit"
// branch in tick(), which exists precisely so a preemption pass never destroys burst work to fix
// a shortfall that isn't really about how much capacity there is.
type noRunningListStore struct {
	LoopStore
	t                    *testing.T
	queued               []*domain.Experiment
	notAdmitted          map[string]string
	requeuePreemptedCall bool
}

func (s *noRunningListStore) ListQueuedExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.queued, nil
}
func (s *noRunningListStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}
func (s *noRunningListStore) RequeuePreempted(context.Context, string, float64, float64) (bool, error) {
	s.requeuePreemptedCall = true
	return true, nil
}
func (s *noRunningListStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id}, nil
}
func (s *noRunningListStore) IsAgentCut(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *noRunningListStore) HasUnsummarizedCompleted(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *noRunningListStore) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	if s.notAdmitted == nil {
		s.notAdmitted = map[string]string{}
	}
	s.notAdmitted[id] = reason
	return nil
}
func (s *noRunningListStore) ListCapacityClaimedExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}

type tickQuota struct{ LoopQuotaStore }

func (tickQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{}, nil
}

// layoutShortWorkload reports a cluster with enough aggregate capacity for the job (scalarFits)
// but with that capacity split across two nodes so a hard-spread two-rank job's distinct-host
// requirement fails (topologyFits is false).
type layoutShortWorkload struct{ LoopWorkloadClient }

func (layoutShortWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	fp := domain.NewFootprint()
	fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: h100}, 2)
	return map[string]domain.Footprint{"cluster-a": fp}, map[string]domain.Footprint{"cluster-a": domain.NewFootprint()}, nil
}
func (layoutShortWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	// Both devices sit on the same node: fine for scalar fit, not enough distinct hosts for two
	// spread ranks.
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 2}}}, nil
}
func (layoutShortWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {}}}, nil
}
func (layoutShortWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {}}}, nil
}
func (layoutShortWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}}, nil
}
func (layoutShortWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{"cluster-a": true}, nil
}
func (layoutShortWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func TestTickSkipsPreemptionForLayoutOnlyShortfall(t *testing.T) {
	exp := distributedExperiment(2, 1)
	exp.ID = "exp-1"
	exp.AgentID = "agent-1"
	exp.PlatformExperimentID = "pe-1"
	exp.CapacityTier = domain.CapacityGuaranteed
	exp.ClusterName = "cluster-a"

	store := &noRunningListStore{t: t, queued: []*domain.Experiment{exp}}
	l := NewLoop(store, tickQuota{}, layoutShortWorkload{}, zap.NewNop())
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = DefaultDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
	if store.notAdmitted[exp.ID] == "" {
		t.Fatal("job should have been marked not-admitted for the layout-only shortfall")
	}
	if store.requeuePreemptedCall {
		t.Fatal("preemption requeued a victim for a shortfall that was purely about layout, not capacity")
	}
}
