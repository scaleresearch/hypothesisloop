package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// --- submitJob TOCTOU: a node the tick's decision relied on vanishes before the claim commits ---

// claimStore's ClaimSubmitted runs the real capacityAvailable callback against liveWorkload
// (a second, independent capacity view), exactly like the production store does inside its
// transaction — the whole point being that the callback re-reads live state rather than trusting
// what the tick decided.
type claimStore struct {
	LoopStore
	claimed     bool
	claimCalled bool
}

func (s *claimStore) claimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.claimCalled = true
	ok, err := capacityAvailable(ctx, nil)
	if err != nil || !ok {
		return false, err
	}
	s.claimed = true
	return true, nil
}

// liveWorkload answers submitJob's re-check with whatever capacity is installed at call time —
// tests mutate it between building exp and invoking submitJob to simulate the cluster losing a
// node in the gap between the tick's placement decision and the claim.
type liveWorkload struct {
	ClusterCapacity
	guaranteed map[string]domain.Footprint
	burst      map[string]domain.Footprint
	nodeAccel  map[string]map[string]map[string]int64
	nodeRes    map[string]map[string]map[string]int64
	nodeLabels map[string]map[string]map[string]string
}

func (w *liveWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
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

type submitQuota struct{ LoopQuotaStore }

func (submitQuota) ReserveAdmittedAcceleratorType(context.Context, string, domain.AcceleratorType, float64, int) error {
	return nil
}

// The tick decided cluster-a fits (that decision isn't reproduced here — this test starts from
// "submitJob is now called for cluster-a"). Between that decision and this call, cluster-a's only
// qualifying node went away. submitJob's own re-check (ClaimSubmitted's capacityAvailable
// callback, which re-reads live per-node capacity via placement.DesiredFits) must catch this and
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
	l := NewLoop(store, submitQuota{}, workload, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}

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
}

// Same setup, but the node is still there at claim time: the claim must succeed. This is the control for the test above — it proves the rejection there is
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
	l := NewLoop(store, submitQuota{}, workload, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}

	if err := l.submitJob(context.Background(), exp, "cluster-a", exp.AcceleratorType); err != nil {
		t.Fatalf("submitJob = %v, want nil: the node the placement relied on is still there", err)
	}
	if !store.claimed {
		t.Fatal("claim should have succeeded")
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
func (s *noRunningListStore) ListSubmittedExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}
func (s *noRunningListStore) RecentlyTriedClusters(context.Context, time.Duration) (map[string]bool, error) {
	return map[string]bool{}, nil
}

type tickQuota struct{ LoopQuotaStore }

func (tickQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{}, nil
}

// layoutShortWorkload reports a cluster with enough aggregate capacity for the job (scalarFits)
// but with that capacity split across two nodes so a hard-spread two-rank job's distinct-host
// requirement fails (placement.TopologyFits is false).
type layoutShortWorkload struct{ ClusterCapacity }

func (layoutShortWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	fp := domain.CapacityFootprint(16, map[string]int64{h100: 2}, 64<<30, 64<<30)
	return map[string]domain.Footprint{"cluster-a": fp}, map[string]domain.Footprint{"cluster-a": domain.NewFootprint()}, nil
}
func (layoutShortWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	// Both devices sit on the same node: fine for scalar fit, not enough distinct hosts for two
	// spread ranks.
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 2}}}, nil
}
func (layoutShortWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (layoutShortWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
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

// --- Tick-level: preemption must not run when the cluster is already waiting for a scale-up ---

// scaleUpWaitingStore adds the two speculativeCandidates store methods on top of
// noRunningListStore, both answering "nothing to see here" so this test proves the skip is driven
// by the negative desired-free dimension alone, not by tried-cluster/cap machinery.
type scaleUpWaitingStore struct{ noRunningListStore }

func (s *scaleUpWaitingStore) GetClusterSettings(context.Context, string) (*domain.ClusterSettings, error) {
	return nil, nil
}
func (s *scaleUpWaitingStore) RecentlyTriedClusters(context.Context, time.Duration) (map[string]bool, error) {
	return nil, nil
}
func (s *scaleUpWaitingStore) ListSubmittedExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}

// scaleUpWaitingWorkload reports cluster-a as autoscaler-enabled with desired-free already
// negative in the job's accelerator dimension (a SUBMITTED row is already outstanding there) —
// live fit fails, and this job's own request is oversized for the only known node, so it never
// becomes a speculative candidate itself; it must fall through to preemption and be skipped there.
type scaleUpWaitingWorkload struct{ ClusterCapacity }

func (scaleUpWaitingWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	fp := domain.NewFootprint()
	// GetAcceleratorTypeCapacity's real implementations always build this via CapacityFootprint, which
	// lowercases the accelerator type key — matched here so negativeInDimension's lookup (keyed off
	// exp.AcceleratorType, lowercased) finds it, exactly as it would against real capacity.
	fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Name: strings.ToLower(h100)}, -2)
	return map[string]domain.Footprint{"cluster-a": fp}, map[string]domain.Footprint{"cluster-a": domain.NewFootprint()}, nil
}
func (scaleUpWaitingWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	// installedAcceleratorsByNode needs a positive count here to resolve the job's proportionate
	// share, independent of whether that's enough to actually admit — live fit is decided by
	// GetAcceleratorTypeCapacity's (negative) desired-free above, not by this per-node view.
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 1}}}, nil
}
func (scaleUpWaitingWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (scaleUpWaitingWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (scaleUpWaitingWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}}, nil
}
func (scaleUpWaitingWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (scaleUpWaitingWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}
func (scaleUpWaitingWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{"cluster-a": true}, nil
}
func (scaleUpWaitingWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{"cluster-a": "cid-a"}, nil
}

func TestTickSkipsPreemptionWhenClusterIsAlreadyWaitingForScaleUp(t *testing.T) {
	exp := distributedExperiment(1, 2) // needs more accelerators than the only known node has
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "1", "1Gi", "1Gi"
	exp.ID = "exp-1"
	exp.AgentID = "agent-1"
	exp.PlatformExperimentID = "pe-1"
	exp.CapacityTier = domain.CapacityGuaranteed
	exp.ClusterName = "cluster-a"

	store := &scaleUpWaitingStore{noRunningListStore{t: t, queued: []*domain.Experiment{exp}}}
	l := NewLoop(store, tickQuota{}, scaleUpWaitingWorkload{}, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}
	l.WithSpeculation(15 * time.Minute)

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
	if store.requeuePreemptedCall {
		t.Fatal("preemption must not run against a cluster whose desired-free is already negative for this dimension — a scale-up is outstanding, not a shortage preemption could fix")
	}
	if got := reasonCode(store.notAdmitted[exp.ID]); got != domain.NotAdmittedWaitingForScaleUp {
		t.Fatalf("not_admitted_reason = %q, want %q", got, domain.NotAdmittedWaitingForScaleUp)
	}
}

func TestTickSkipsPreemptionForLayoutOnlyShortfall(t *testing.T) {
	exp := distributedExperiment(2, 1)
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "1", "1Gi", "1Gi"
	exp.ID = "exp-1"
	exp.AgentID = "agent-1"
	exp.PlatformExperimentID = "pe-1"
	exp.CapacityTier = domain.CapacityGuaranteed
	exp.ClusterName = "cluster-a"

	store := &noRunningListStore{t: t, queued: []*domain.Experiment{exp}}
	l := NewLoop(store, tickQuota{}, layoutShortWorkload{}, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
	if got := store.notAdmitted[exp.ID]; !strings.HasPrefix(got, domain.NotAdmittedCapacityUnavailable+": cluster cluster-a has room for it in total and no node of it holds the job") {
		t.Fatalf("not_admitted_reason = %q, want it to say the layout is what does not fit", got)
	}
	if store.requeuePreemptedCall {
		t.Fatal("preemption requeued a victim for a shortfall that was purely about layout, not capacity")
	}
}

// --- Tick-level: a request above the per-accelerator share says so ---

// shareWorkload is one node with eight free accelerators and 6 cores in total, so one accelerator
// is entitled to 750m. Every scalar and topology check passes; only the share is too small.
type shareWorkload struct{ ClusterCapacity }

func (shareWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func() domain.Footprint {
		return domain.CapacityFootprint(6, map[string]int64{h100: 8}, 64<<30, 64<<30)
	}
	return map[string]domain.Footprint{"cluster-a": free()}, map[string]domain.Footprint{"cluster-a": free()}, nil
}
func (shareWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 8}}}, nil
}
func (shareWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": shareNodeTotal()}}, nil
}
func (shareWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": shareNodeTotal()}}, nil
}
func (shareWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}}, nil
}
func (shareWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{"cluster-a": false}, nil
}
func (shareWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func shareNodeTotal() map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: 6000,
		domain.NodeResourceMemoryBytes:   64 << 30,
		domain.NodeResourceStorageBytes:  64 << 30,
	}
}

func TestTickNamesTheAcceleratorShareWhenARequestExceedsIt(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			exp := &domain.Experiment{
				ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1", CapacityTier: tier,
				AcceleratorType: h100, AcceleratorCount: 1,
				Job: domain.JobSpec{CPU: "1", Memory: "1Gi", Storage: "1Gi", AcceleratorType: h100, AcceleratorCount: 1},
			}
			store := &noRunningListStore{t: t, queued: []*domain.Experiment{exp}}
			l := NewLoop(store, tickQuota{}, shareWorkload{}, testGraceLimits, zap.NewNop())
			l.observed = fakeObserved{}
			l.evictor = noopEvictor{}
			l.disbalanceTolerance = testDisbalanceTolerance
			l.reprioritizer = noopReprioritizer{}

			if err := l.tick(context.Background()); err != nil {
				t.Fatalf("tick() = %v, want nil", err)
			}
			want := `exceeds_accelerator_share: cpu 1 is above the 750m that 1 accelerator(s) get on cluster cluster-a (each node's cpu split evenly across its installed accelerators); request at most 750m`
			if got := store.notAdmitted[exp.ID]; got != want {
				t.Fatalf("not_admitted_reason = %q\nwant                  %q", got, want)
			}
		})
	}
}

// --- Tick-level: a job no cluster has the hardware for is told apart from one that waits ---

// fullWorkload is one node with two accelerators installed, both taken.
type fullWorkload struct{ shareWorkload }

func (fullWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func() domain.Footprint {
		return domain.CapacityFootprint(6, map[string]int64{h100: 0}, 64<<30, 64<<30)
	}
	return map[string]domain.Footprint{"cluster-a": free()}, map[string]domain.Footprint{"cluster-a": free()}, nil
}
func (fullWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 0}}}, nil
}
func (fullWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(6, map[string]int64{h100: 2}, 64<<30, 64<<30)}, nil
}

func TestTickTellsAJobAboveInstalledCapacityFromOneThatWaits(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		for count, want := range map[int]string{2: domain.NotAdmittedCapacityUnavailable, 3: domain.NotAdmittedExceedsClusterCapacity} {
			t.Run(fmt.Sprintf("%s/%d", tier, count), func(t *testing.T) {
				exp := &domain.Experiment{
					ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1", CapacityTier: tier,
					AcceleratorType: h100, AcceleratorCount: count,
					Job: domain.JobSpec{AcceleratorType: h100, AcceleratorCount: count},
				}
				store := &noRunningListStore{t: t, queued: []*domain.Experiment{exp}}
				l := NewLoop(store, tickQuota{}, fullWorkload{}, testGraceLimits, zap.NewNop())
				l.observed = fakeObserved{}
				l.evictor = noopEvictor{}
				l.disbalanceTolerance = testDisbalanceTolerance
				l.reprioritizer = noopReprioritizer{}

				if err := l.tick(context.Background()); err != nil {
					t.Fatalf("tick() = %v, want nil", err)
				}
				got := store.notAdmitted[exp.ID]
				if code, _, _ := strings.Cut(got, ":"); code != want {
					t.Fatalf("not_admitted_reason = %q, want code %q", got, want)
				}
				if domain.QueueReasonClearsByWaiting(got) != (want == domain.NotAdmittedCapacityUnavailable) {
					t.Fatalf("QueueReasonClearsByWaiting(%q) is wrong: only the job that fits the cluster waits", got)
				}
			})
		}
	}
}

// --- Tick-level: a waiting guaranteed job holds free capacity against the burst pass ---

const l40 = "nvidia.com/gpu.product=nvidia-l40"

// holdWorkload is cluster-a with four h100 installed and three free, two free l40, and cluster-b
// with one free h100. cpuCores is what cluster-a has free, terminating the h100 on cluster-a that
// desired state has released and a terminating pod still holds.
type holdWorkload struct {
	ClusterCapacity
	cpuCores    float64
	terminating int64
}

func (w holdWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func(terminating int64) map[string]domain.Footprint {
		return map[string]domain.Footprint{
			"cluster-a": domain.CapacityFootprint(w.cpuCores, map[string]int64{h100: 3 + terminating, l40: 2}, 64<<30, 64<<30),
			"cluster-b": domain.CapacityFootprint(16, map[string]int64{h100: 1}, 64<<30, 64<<30),
		}
	}
	return free(0), free(w.terminating), nil
}
func (w holdWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{
		"cluster-a": {"node-a": {h100: 3, l40: 2}},
		"cluster-b": {"node-b": {h100: 1}},
	}, nil
}
func (w holdWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	a := holdNodeTotal()
	a[domain.NodeResourceCPUMillicores] = int64(w.cpuCores * 1000)
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": a}, "cluster-b": {"node-b": holdNodeTotal()}}, nil
}
func (holdWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}, "cluster-b": {"node-b": holdNodeTotal()}}, nil
}
func (holdWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}, "cluster-b": {"node-b": {}}}, nil
}
func (holdWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (holdWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (holdWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{"cluster-a": "id-cluster-a", "cluster-b": "id-cluster-b"}, nil
}
func (holdWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{
		"cluster-a": domain.CapacityFootprint(16, map[string]int64{h100: 4, l40: 3}, 64<<30, 64<<30),
		"cluster-b": domain.CapacityFootprint(16, map[string]int64{h100: 1}, 64<<30, 64<<30),
	}, nil
}

func holdNodeTotal() map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: 16000,
		domain.NodeResourceMemoryBytes:   64 << 30,
		domain.NodeResourceStorageBytes:  64 << 30,
	}
}

type holdStore struct {
	noRunningListStore
	running   []*domain.Experiment
	submitted map[string]string
}

func (s *holdStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}
func (s *holdStore) ListCapacityClaimedExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}
func (s *holdStore) claimSubmitted(_ context.Context, id, cluster string, _ *domain.JobSpec, _ func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	if s.submitted == nil {
		s.submitted = map[string]string{}
	}
	s.submitted[id] = cluster
	return true, nil
}

type holdQuota struct{ tickQuota }

func (holdQuota) ReserveAdmittedAcceleratorType(context.Context, string, domain.AcceleratorType, float64, int) error {
	return nil
}

type recordingEvictor struct{ evicted []string }

func (e *recordingEvictor) EvictExperiment(_ context.Context, id string, _ domain.EvictionReason) error {
	e.evicted = append(e.evicted, id)
	return nil
}

func holdJob(id string, tier domain.CapacityTier, acceleratorType domain.AcceleratorType, count int, cpu string) *domain.Experiment {
	return &domain.Experiment{
		ID: id, AgentID: "agent-" + id, PlatformExperimentID: "pe-1", CapacityTier: tier,
		AcceleratorType: acceleratorType, AcceleratorCount: count,
		Job: domain.JobSpec{CPU: cpu, Memory: "1Gi", Storage: "1Gi", AcceleratorType: acceleratorType, AcceleratorCount: count},
	}
}

func holdLoop(store LoopStore, workload ClusterCapacity, evictor ExperimentEvictor) *Loop {
	l := NewLoop(store, holdQuota{}, workload, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}
	l.evictor = evictor
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}
	return l
}

func TestTickHoldsFreeCapacityForAWaitingGuaranteedJob(t *testing.T) {
	// The guaranteed job is pinned to cluster-a, which has three h100 free of four installed.
	for name, tc := range map[string]struct {
		guaranteedCount      int
		terminating          int64
		burstAcceleratorType domain.AcceleratorType
		burstCluster         string
		admitted             bool
	}{
		"capacity being freed holds the same accelerator type": {4, 1, h100, "cluster-a", false},
		"nothing being freed holds nothing":                    {4, 0, h100, "cluster-a", true},
		"above installed capacity holds nothing":               {5, 1, h100, "cluster-a", true},
		"another accelerator type is admitted":                 {4, 1, l40, "cluster-a", true},
		"another cluster is admitted":                          {4, 1, h100, "cluster-b", true},
	} {
		t.Run(name, func(t *testing.T) {
			guaranteed := holdJob("guaranteed", domain.CapacityGuaranteed, h100, tc.guaranteedCount, "1")
			guaranteed.ClusterName = "cluster-a"
			burst := holdJob("burst", domain.CapacityBurst, tc.burstAcceleratorType, 1, "1")
			burst.ClusterName = tc.burstCluster
			store := &holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{burst, guaranteed}}}
			l := holdLoop(store, holdWorkload{cpuCores: 16, terminating: tc.terminating}, noopEvictor{})

			if err := l.tick(context.Background()); err != nil {
				t.Fatalf("tick() = %v, want nil", err)
			}
			if _, ok := store.submitted["guaranteed"]; ok {
				t.Fatal("the guaranteed job was admitted; the case needs it waiting")
			}
			if _, ok := store.submitted["burst"]; ok != tc.admitted {
				t.Fatalf("burst job admitted = %v, want %v (reason %q)", ok, tc.admitted, store.notAdmitted["burst"])
			}
			if got, _ := domain.SplitQueueReason(store.notAdmitted["burst"]); !tc.admitted && got != domain.NotAdmittedOutranked {
				t.Fatalf("not_admitted_reason = %q, want %q", got, domain.NotAdmittedOutranked)
			}
		})
	}
}

// The guaranteed job is short one h100 and preempts for it, which leaves the cluster's disbalance
// verdict to the burst pass. Its hold takes every free core, so the l40 burst job is short on cpu
// alone beside a running job far above its cpu share: the disbalance premises.
func TestTickEvictsNothingForABurstJobHeldByAWaitingGuaranteedJob(t *testing.T) {
	guaranteed := holdJob("guaranteed", domain.CapacityGuaranteed, h100, 4, "4")
	guaranteed.ClusterName = "cluster-a"
	burst := holdJob("burst", domain.CapacityBurst, l40, 1, "1")
	hog := holdJob("hog", domain.CapacityBurst, l40, 1, "12")
	hog.ClusterName = "cluster-a"
	hog.Status = domain.StatusRunning
	victim := holdJob("victim", domain.CapacityBurst, h100, 1, "1")
	victim.ClusterName = "cluster-a"
	victim.Status = domain.StatusRunning

	store := &holdStore{
		noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{burst, guaranteed}},
		running:            []*domain.Experiment{hog, victim},
	}
	evictor := &recordingEvictor{}
	l := holdLoop(store, holdWorkload{cpuCores: 4}, evictor)
	l.observed = fakeObserved{node: map[string]string{"hog": "node-a", "victim": "node-a"}}
	l.settler = &recordingSettler{}

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
	if !store.requeuePreemptedCall {
		t.Fatal("the guaranteed job preempted nothing; the case needs the burst pass to own the disbalance verdict")
	}
	if len(evictor.evicted) != 0 {
		t.Fatalf("evicted %v for a burst job that only waits behind a guaranteed one", evictor.evicted)
	}
	if got, _ := domain.SplitQueueReason(store.notAdmitted["burst"]); got != domain.NotAdmittedOutranked {
		t.Fatalf("not_admitted_reason = %q, want %q", got, domain.NotAdmittedOutranked)
	}
}

// keepingStore requeues no victim a preemption planned.
type keepingStore struct{ holdStore }

func (*keepingStore) RequeuePreempted(context.Context, string, float64, float64) (bool, error) {
	return false, nil
}

type failingReprioritizer struct{}

func (failingReprioritizer) RePrioritize(context.Context) error {
	return errors.New("queue unreadable")
}

// A preemption that requeued none of its victims and a reprioritization that failed were logged
// and the tick answered nil, so a pass that did neither looked whole. Both are the tick's errors.
func TestTickReturnsAFailedPreemptionAndAFailedReprioritization(t *testing.T) {
	guaranteed := holdJob("guaranteed", domain.CapacityGuaranteed, h100, 4, "4")
	guaranteed.ClusterName = "cluster-a"
	victim := holdJob("victim", domain.CapacityBurst, h100, 1, "1")
	victim.ClusterName = "cluster-a"
	victim.Status = domain.StatusRunning
	store := &keepingStore{holdStore{
		noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{guaranteed}},
		running:            []*domain.Experiment{victim},
	}}
	l := holdLoop(store, holdWorkload{cpuCores: 16}, noopEvictor{})
	l.observed = fakeObserved{node: map[string]string{"victim": "node-a"}}
	l.settler = &recordingSettler{}
	l.reprioritizer = failingReprioritizer{}

	err := l.tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "experiment guaranteed: preempt on cluster-a") || !strings.Contains(err.Error(), "reprioritize after tick: queue unreadable") {
		t.Fatalf("tick() = %v, want the failed preemption and the failed reprioritization", err)
	}
	if reasonCode(store.notAdmitted["guaranteed"]) == "" {
		t.Errorf("the job the preemption failed for carries no reason: the pass did not finish it")
	}
}

// --- Tick-level: one shortage is preempted for once, however long the victim takes to go ---

// terminatingStore requeues a preempted job out of the running set at once.
type terminatingStore struct {
	holdStore
	requeued []string
}

func (s *terminatingStore) RequeuePreempted(_ context.Context, id string, _, _ float64) (bool, error) {
	for i, exp := range s.running {
		if exp.ID == id {
			s.running = append(s.running[:i], s.running[i+1:]...)
			s.requeued = append(s.requeued, id)
			return true, nil
		}
	}
	return false, nil
}

// terminatingWorkload is cluster-a with four h100 installed and one reported free on every read:
// the pods of requeued jobs never go away. Desired-free follows the store.
type terminatingWorkload struct {
	holdWorkload
	store *terminatingStore
}

func (w terminatingWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func(h100Free int64) map[string]domain.Footprint {
		return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(16, map[string]int64{h100: h100Free}, 64<<30, 64<<30)}
	}
	return free(1), free(4 - int64(len(w.store.running))), nil
}
func (terminatingWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 1}}}, nil
}

func TestTickPreemptsOnceWhileTheVictimTerminates(t *testing.T) {
	guaranteed := holdJob("guaranteed", domain.CapacityGuaranteed, h100, 2, "1")
	guaranteed.ClusterName = "cluster-a"
	store := &terminatingStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{guaranteed}}}}
	for _, id := range []string{"b1", "b2", "b3"} {
		b := holdJob(id, domain.CapacityBurst, h100, 1, "1")
		b.ClusterName = "cluster-a"
		b.Status = domain.StatusRunning
		store.running = append(store.running, b)
	}
	l := holdLoop(store, terminatingWorkload{holdWorkload: holdWorkload{cpuCores: 16}, store: store}, noopEvictor{})
	l.observed = observedOnNode([]string{"b1", "b2", "b3"}, "node-a")
	l.settler = &recordingSettler{}

	for tick := 1; tick <= 4; tick++ {
		if err := l.tick(context.Background()); err != nil {
			t.Fatalf("tick %d = %v, want nil", tick, err)
		}
		if len(store.requeued) != 1 {
			t.Fatalf("after tick %d preempted %v, want one victim for a shortage of one", tick, store.requeued)
		}
	}
	if _, ok := store.submitted["guaranteed"]; ok {
		t.Fatal("the guaranteed job was admitted while its capacity was still held")
	}
}

// --- Tick-level: a job that ran stays on the accelerator type it ran on ---

// The job asked for h100, accepts l40 and its row is at l40, which its quota does not cover while
// h100 is free. One that never ran goes back to the type it asked for. One that ran on l40 may be
// placed on no other type (placement.CandidateTypes): it waits and says why.
func TestTickDoesNotMoveAJobThatRanWhenItsQuotaNoLongerCoversItsType(t *testing.T) {
	withCPUType(t)
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		for name, ran := range map[string]bool{"never ran": false, "ran on the alternative": true} {
			t.Run(string(tier)+"/"+name, func(t *testing.T) {
				exp := holdJob("a1", tier, l40, 1, "1")
				exp.Job.AcceleratorType, exp.Job.AcceptableAcceleratorTypes = h100, []domain.AcceleratorType{l40}
				exp.EstimatedDurationHours = 1
				if ran {
					exp.RanOnAcceleratorType = l40
				}
				store := &holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{exp}}}
				l := holdLoop(store, holdWorkload{cpuCores: 16}, noopEvictor{})

				if err := l.tick(context.Background()); err != nil {
					t.Fatalf("tick() = %v, want nil", err)
				}
				cluster, admitted := store.submitted["a1"]
				if !ran {
					if !admitted || exp.AcceleratorType != h100 {
						t.Fatalf("admitted = %v at %q (reason %q), want it back on the type it asked for", admitted, exp.AcceleratorType, store.notAdmitted["a1"])
					}
					return
				}
				if admitted || exp.AcceleratorType != l40 {
					t.Fatalf("placed on %q at %q, want it waiting at the type it ran on", cluster, exp.AcceleratorType)
				}
				if got := store.notAdmitted["a1"]; !strings.HasPrefix(got, domain.NotAdmittedInsufficientQuota+": ") || !strings.Contains(got, l40) {
					t.Fatalf("not_admitted_reason = %q, want %s naming the type", got, domain.NotAdmittedInsufficientQuota)
				}
			})
		}
	}
}

func (*scaleUpWaitingStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }

func (*noRunningListStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }

// staleMemoryWorkload is cluster-a a moment after a job ended there: with every placed job
// counted its memory is free, and the runtime's last report still shows 1 GiB of it.
type staleMemoryWorkload struct{ holdWorkload }

func (w staleMemoryWorkload) GetAcceleratorTypeCapacity(ctx context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	available, desiredFree, err := w.holdWorkload.GetAcceleratorTypeCapacity(ctx)
	available["cluster-a"][domain.ResourceKey{Kind: domain.ResourceKindMemory}] = 1 << 30
	return available, desiredFree, err
}

func (w staleMemoryWorkload) GetNodeResourceCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error) {
	nodes, err := w.holdWorkload.GetNodeResourceCapacity(ctx)
	nodes["cluster-a"]["node-a"][domain.NodeResourceMemoryBytes] = 1 << 30
	return nodes, err
}

func TestTickTellsAJobItWaitsForACapacityReportWhenDesiredStateHasTheRoom(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		job := holdJob("job", tier, h100, 1, "1")
		job.Job.Memory = "8Gi"
		job.ClusterName = "cluster-a"
		store := &holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{job}}}
		l := holdLoop(store, staleMemoryWorkload{holdWorkload{cpuCores: 16}}, noopEvictor{})

		tickOnce(t, l)

		const want = "capacity_unavailable: cluster cluster-a is short of 7 GiB memory by its last capacity report, and no job the platform has placed there holds it: a job that ended is not reported gone yet, and this one is waiting for the cluster's next capacity report"
		if got := store.notAdmitted["job"]; got != want {
			t.Errorf("%s: not_admitted_reason =\n%q, want\n%q", tier, got, want)
		}
	}
}

func (fullWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (fullWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (layoutShortWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (layoutShortWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (shareWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (shareWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *claimStore) ClaimSubmitted(ctx context.Context, id, cluster string, resolved *domain.JobSpec, fits func(context.Context, []*domain.Experiment) (bool, error)) (*domain.ClaimRefusal, error) {
	return refusedUnless(s.claimSubmitted(ctx, id, cluster, resolved, fits))
}

func (s *holdStore) ClaimSubmitted(ctx context.Context, id, cluster string, resolved *domain.JobSpec, fits func(context.Context, []*domain.Experiment) (bool, error)) (*domain.ClaimRefusal, error) {
	return refusedUnless(s.claimSubmitted(ctx, id, cluster, resolved, fits))
}

// refusedUnless is a fake's claim as ClaimSubmitted answers it: nil when it placed the job.
func refusedUnless(claimed bool, err error) (*domain.ClaimRefusal, error) {
	if err != nil || claimed {
		return nil, err
	}
	return &domain.ClaimRefusal{Code: domain.NotAdmittedCapacityUnavailable}, nil
}
