package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestFitsLargestNodeReportsNoProofForClusterWithNoNodes(t *testing.T) {
	exp := distributedExperiment(1, 2)
	if fitsLargestNode(exp, nil, nil, nil) {
		t.Fatal("a cluster reporting zero live nodes has no shape data to check against, so it must report no proof (false) — the caller decides separately whether to still speculate blind")
	}
}

func TestFitsLargestNodeAcceptsWhenSomeNodeCouldHostIt(t *testing.T) {
	exp := distributedExperiment(1, 2)
	accel := map[string]map[string]int64{"node-a": {h100: 8}}
	resources := map[string]map[string]int64{"node-a": {
		domain.NodeResourceCPUMillicores: 64000, domain.NodeResourceMemoryBytes: 1 << 40, domain.NodeResourceStorageBytes: 1 << 40,
	}}
	if !fitsLargestNode(exp, accel, resources, nil) {
		t.Fatal("a job that fits the largest known node must be a candidate")
	}
}

func TestFitsLargestNodeRejectsOversizedRequest(t *testing.T) {
	exp := distributedExperiment(1, 16) // more accelerators per rank than any node has
	accel := map[string]map[string]int64{"node-a": {h100: 8}}
	resources := map[string]map[string]int64{"node-a": {domain.NodeResourceCPUMillicores: 64000}}
	if fitsLargestNode(exp, accel, resources, nil) {
		t.Fatal("a request no node in the pool could ever host must never speculate — it would Pend forever")
	}
}

func TestFitsLargestNodeRequiresLabelMatch(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.Job.NodeSelector = map[string]string{"pool": "gpu"}
	accel := map[string]map[string]int64{"node-a": {h100: 8}}
	resources := map[string]map[string]int64{"node-a": {domain.NodeResourceCPUMillicores: 64000}}
	labels := map[string]map[string]string{"node-a": {"pool": "cpu"}}
	if fitsLargestNode(exp, accel, resources, labels) {
		t.Fatal("a node whose labels don't match the job's selector must not count")
	}
	labels["node-a"]["pool"] = "gpu"
	if !fitsLargestNode(exp, accel, resources, labels) {
		t.Fatal("a matching-label node with room should be a candidate")
	}
}

func TestTriedRecentlyExpiresAfterTTL(t *testing.T) {
	now := time.Now()
	tried := []domain.TriedCluster{{ClusterID: "c1", At: now.Add(-20 * time.Minute)}}
	if triedRecently(tried, "c1", 10*time.Minute, now) {
		t.Fatal("an entry older than the TTL must no longer exclude the cluster")
	}
	if !triedRecently(tried, "c1", 30*time.Minute, now) {
		t.Fatal("an entry within the TTL must still exclude the cluster")
	}
	if triedRecently(tried, "c2", 30*time.Minute, now) {
		t.Fatal("a different cluster's tried entry must not exclude this one")
	}
}

// speculationStore is the minimal LoopStore speculativeCandidates needs.
type speculationStore struct {
	LoopStore
	settings map[string]*domain.ClusterSettings
}

func (s *speculationStore) GetClusterSettings(_ context.Context, clusterID string) (*domain.ClusterSettings, error) {
	return s.settings[clusterID], nil
}

func (s *speculationStore) RecentlyTriedClusters(_ context.Context, _ time.Duration) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *speculationStore) ListCapacityClaimedExperiments(_ context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}

func speculationLoop(store LoopStore) *Loop {
	l := NewLoop(store, tickQuota{}, nil, zap.NewNop())
	return l.WithSpeculation(15 * time.Minute)
}

func nodeFixture() (map[string]map[string]map[string]int64, map[string]map[string]map[string]int64) {
	accel := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 8}}}
	resources := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {
		domain.NodeResourceCPUMillicores: 64000, domain.NodeResourceMemoryBytes: 1 << 40, domain.NodeResourceStorageBytes: 1 << 40,
	}}}
	return accel, resources
}

func TestSpeculativeCandidatesRequiresAutoscalerAndConnected(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.AcceleratorCount = 2
	accel, resources := nodeFixture()
	l := speculationLoop(&speculationStore{})

	cases := []struct {
		name       string
		autoscaler map[string]bool
		connected  map[string]bool
		want       int
	}{
		{"not autoscaler-enabled", map[string]bool{}, map[string]bool{"cluster-a": true}, 0},
		{"not connected", map[string]bool{"cluster-a": true}, map[string]bool{}, 0},
		{"both true", map[string]bool{"cluster-a": true}, map[string]bool{"cluster-a": true}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clusterIDs := map[string]string{"cluster-a": "cid-a"}
			got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, tc.autoscaler, tc.connected, clusterIDs, nil, accel, resources, nil, nil, nil)
			if err != nil {
				t.Fatalf("speculativeCandidates: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d candidates, want %d (%v)", len(got), tc.want, got)
			}
		})
	}
}

func TestSpeculativeCandidatesGangRequiresMultiNodeCapable(t *testing.T) {
	exp := distributedExperiment(3, 2) // 3-node gang
	accel := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 8}}}
	resources := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {domain.NodeResourceCPUMillicores: 64000}}}
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-a": true}
	connected := map[string]bool{"cluster-a": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, map[string]bool{}, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a gang must not speculate onto a cluster that hasn't reported multi_node_capable")
	}

	got, err = l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, map[string]bool{"cluster-a": true}, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("a multi-node-capable autoscaler cluster should be a gang candidate")
	}
}

func TestSpeculativeCandidatesExcludesTriedCluster(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.TriedClusters = []domain.TriedCluster{{ClusterID: "cid-a", At: time.Now()}}
	accel, resources := nodeFixture()
	l := speculationLoop(&speculationStore{})

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp,
		map[string]bool{"cluster-a": true}, map[string]bool{"cluster-a": true},
		map[string]string{"cluster-a": "cid-a"}, nil, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a cluster this job already failed over from within the TTL must be excluded")
	}
}

func TestSpeculativeCandidatesStableOrderByClusterID(t *testing.T) {
	exp := distributedExperiment(1, 2)
	accel := map[string]map[string]map[string]int64{
		"cluster-z": {"node-a": {h100: 8}},
		"cluster-a": {"node-a": {h100: 8}},
	}
	resources := map[string]map[string]map[string]int64{
		"cluster-z": {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
		"cluster-a": {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
	}
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-z": true, "cluster-a": true}
	connected := map[string]bool{"cluster-z": true, "cluster-a": true}
	// cluster_id order, not cluster_name order: cid-b sorts before cid-z though "cluster-a" the
	// name would sort first too here — use ids that disagree with the name order to prove it's
	// the id being sorted.
	clusterIDs := map[string]string{"cluster-z": "cid-b", "cluster-a": "cid-z"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 2 || got[0] != "cluster-z" {
		t.Fatalf("got %v, want cluster-z (cid-b) first by cluster_id order", got)
	}
}

// TestSpeculativeCandidatesExcludesClusterAlreadyWaitingForItsOwnScaleUp covers a gap the
// never-exclude-on-live-mismatch change (above) could otherwise reopen: a cluster whose desired-
// free already went negative in this job's accelerator dimension has a scale-up bet already in
// flight for exactly this shortage. Unlike a live-node shape/flavor mismatch (a preference, not a
// gate), this is direct proof of an outstanding claim — piling a second speculative submit on top
// would compound bets instead of waiting for the first one to land, so it must still exclude.
func TestSpeculativeCandidatesExcludesClusterAlreadyWaitingForItsOwnScaleUp(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.AcceleratorType = domain.AcceleratorType(h100)
	accel, resources := nodeFixture()
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-a": true}
	connected := map[string]bool{"cluster-a": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a"}
	desiredFree := domain.NewFootprint()
	desiredFree.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}, -2)

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, nil, map[string]domain.Footprint{"cluster-a": desiredFree})
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a cluster already carrying a negative desired-free in this job's accelerator dimension must be excluded, not offered a second speculative bet")
	}
}

func TestSpeculativeCandidatesRespectsMaxSpeculativeAccelerators(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.AcceleratorCount = 2
	accel, resources := nodeFixture()
	cap := 3
	store := &speculationStore{settings: map[string]*domain.ClusterSettings{
		"cid-a": {ClusterID: "cid-a", MaxSpeculativeAccelerators: &cap},
	}}
	l := speculationLoop(store)
	autoscaler := map[string]bool{"cluster-a": true}
	connected := map[string]bool{"cluster-a": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, map[string]int{"cluster-a": 2}, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("2 already outstanding + 2 requested exceeds the cap of 3, must be excluded")
	}

	got, err = l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, map[string]int{"cluster-a": 0}, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("0 outstanding + 2 requested is within the cap of 3, must be a candidate")
	}
}

// nilCallbackClaimStore captures submitJobTo's capacityAvailable callback so tests can both
// distinguish "no recheck at all" from "rechecks something" and drive the callback itself against
// a chosen `desired` set — ClaimSubmitted (the real store implementation) passes it the
// SUBMITTED/ADMITTED rows on the target cluster, read fresh under its own per-cluster advisory
// lock.
type nilCallbackClaimStore struct {
	LoopStore
	sawNilCallback bool
	claimed        bool
	capacityCheck  func(context.Context, []*domain.Experiment) (bool, error)
	settings       map[string]*domain.ClusterSettings
}

func (s *nilCallbackClaimStore) ClaimSubmitted(_ context.Context, _, _ string, _ *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.sawNilCallback = capacityAvailable == nil
	s.capacityCheck = capacityAvailable
	s.claimed = true
	return true, nil
}

func (s *nilCallbackClaimStore) GetClusterSettings(_ context.Context, clusterID string) (*domain.ClusterSettings, error) {
	return s.settings[clusterID], nil
}

// A speculative submit with no clusterID (e.g. GetClusterIDs unavailable this tick) has nothing
// to recheck a per-cluster speculative cap against and must still claim — the ID, not the
// speculative-ness, decides whether there's a recheck to run.
func TestSubmitJobSpeculativeWithNoClusterIDSkipsCapRecheck(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	store := &nilCallbackClaimStore{}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", "", exp.AcceleratorType, true); err != nil {
		t.Fatalf("submitJobTo(speculative=true) = %v, want nil", err)
	}
	if !store.claimed {
		t.Fatal("ClaimSubmitted was never invoked")
	}
	ok, err := store.capacityCheck(context.Background(), nil)
	if err != nil || !ok {
		t.Fatalf("capacityCheck with no clusterID = (%v, %v), want (true, nil)", ok, err)
	}
}

// A speculative submit's capacityAvailable callback re-reads max_speculative_accelerators against
// ClaimSubmitted's own fresh, lock-serialized `desired` set — not the tick-local snapshot
// speculativeCandidates saw, which another scheduler replica could have raced past concurrently
// (codex review caught this: the tick-local-only check let concurrent replicas overshoot the
// per-cluster cap by the sum of all their jobs, not just a few accelerators).
func TestSubmitJobSpeculativeReChecksClusterCapAgainstFreshDesired(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	exp.AcceleratorCount = 2
	cap := 3
	store := &nilCallbackClaimStore{settings: map[string]*domain.ClusterSettings{
		"cid-a": {MaxSpeculativeAccelerators: &cap},
	}}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", "cid-a", exp.AcceleratorType, true); err != nil {
		t.Fatalf("submitJobTo(speculative=true) = %v, want nil", err)
	}
	if store.sawNilCallback {
		t.Fatal("a speculative submit with a clusterID must still recheck the per-cluster cap at claim time")
	}

	// 0 already-desired + this job's 2 is within the cap of 3.
	ok, err := store.capacityCheck(context.Background(), nil)
	if err != nil || !ok {
		t.Fatalf("capacityCheck with room = (%v, %v), want (true, nil)", ok, err)
	}
	// A concurrently-claimed 2 more (now reflected in ClaimSubmitted's fresh `desired` read) plus
	// this job's 2 is 4 > cap 3 — this is exactly the race the tick-local snapshot could miss.
	racing := []*domain.Experiment{{AcceleratorCount: 2}}
	ok, err = store.capacityCheck(context.Background(), racing)
	if err != nil || ok {
		t.Fatalf("capacityCheck once a concurrent claim fills the cap = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestSubmitJobLiveFitPassesNonNilCapacityCallback(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	store := &nilCallbackClaimStore{}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", "", exp.AcceleratorType, false); err != nil {
		t.Fatalf("submitJobTo(speculative=false) = %v, want nil", err)
	}
	if store.sawNilCallback {
		t.Fatal("an ordinary live-fit submit must still re-check fresh capacity at claim time")
	}
}

func TestNegativeInDimension(t *testing.T) {
	avail := domain.Footprint{{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}: -2}
	if !negativeInDimension(avail, domain.AcceleratorType(h100)) {
		t.Fatal("a negative desired-free entry for this flavor must be detected regardless of case")
	}
	if negativeInDimension(avail, "a100") {
		t.Fatal("a different flavor's negative entry must not match")
	}
	if negativeInDimension(nil, domain.AcceleratorType(h100)) {
		t.Fatal("a nil footprint (e.g. cluster absent from gAvail) must not be treated as negative")
	}
}

func TestAllSpeculativeCandidatesTried(t *testing.T) {
	exp := distributedExperiment(1, 2)
	autoscaler := map[string]bool{"cluster-a": true, "cluster-b": true}
	connected := map[string]bool{"cluster-a": true, "cluster-b": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a", "cluster-b": "cid-b"}

	if allSpeculativeCandidatesTried(exp, autoscaler, connected, clusterIDs, 10*time.Minute) {
		t.Fatal("neither cluster has been tried yet")
	}

	exp.TriedClusters = []domain.TriedCluster{{ClusterID: "cid-a", At: time.Now()}}
	if allSpeculativeCandidatesTried(exp, autoscaler, connected, clusterIDs, 10*time.Minute) {
		t.Fatal("cluster-b is still untried")
	}

	exp.TriedClusters = append(exp.TriedClusters, domain.TriedCluster{ClusterID: "cid-b", At: time.Now()})
	if !allSpeculativeCandidatesTried(exp, autoscaler, connected, clusterIDs, 10*time.Minute) {
		t.Fatal("every autoscaler-enabled candidate has been tried within the TTL")
	}

	if allSpeculativeCandidatesTried(exp, map[string]bool{}, connected, clusterIDs, 10*time.Minute) {
		t.Fatal("no autoscaler-enabled cluster exists at all — this is not the no_scalable_capacity case")
	}

	if allSpeculativeCandidatesTried(exp, autoscaler, connected, clusterIDs, 0) {
		t.Fatal("speculation not opted into (ttl<=0) must never claim candidates were tried")
	}
}

// claimedExperimentsStore adds a fixed ListCapacityClaimedExperiments answer on top of
// speculationStore, for tests that need installedAcceleratorsByNode to see a node as more loaded
// than its raw free count says.
type claimedExperimentsStore struct {
	speculationStore
	claimed []*domain.Experiment
}

func (s *claimedExperimentsStore) ListCapacityClaimedExperiments(_ context.Context) ([]*domain.Experiment, error) {
	return s.claimed, nil
}

// TestSpeculativeCandidatesUsesInstalledNotFreeAccelerators covers the bug this fix addresses: a
// node with 0 FREE accelerators (fully saturated — exactly the state that should trigger a
// scale-up bet) must still be a speculative candidate once its INSTALLED count (free plus what the
// scheduler itself already claimed there) covers the request, because a node that already got
// SUBMITTED there proves the shape exists on this cluster's pool.
func TestSpeculativeCandidatesUsesInstalledNotFreeAccelerators(t *testing.T) {
	exp := distributedExperiment(1, 4)
	exp.AcceleratorCount = 4
	accel := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {h100: 0}}} // fully saturated: 0 free
	resources := map[string]map[string]map[string]int64{"cluster-a": {"node-a": {domain.NodeResourceCPUMillicores: 64000}}}
	already := &domain.Experiment{
		ID: "already-running", ClusterName: "cluster-a", AcceleratorType: domain.AcceleratorType(h100),
		AcceleratorCount: 8, Status: domain.StatusRunning,
		Job: domain.JobSpec{AcceleratorType: h100, AcceleratorCount: 8},
	}
	store := &claimedExperimentsStore{claimed: []*domain.Experiment{already}}
	l := speculationLoop(store)
	l.observed = fakeObserved{node: map[string]string{"already-running": "node-a"}}
	autoscaler := map[string]bool{"cluster-a": true}
	connected := map[string]bool{"cluster-a": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("a node with 0 free but 8 installed must still fit a 4-accelerator request — the old free-only check would wrongly reject this saturated cluster")
	}
}

// TestSpeculativeCandidatesAcceptsZeroNodeClusterBlindWithDefaultCap covers a cluster already
// scaled to zero nodes — no live data exists at all, so the old free/installed distinction is
// moot; the fix must still allow it as a candidate rather than excluding it forever, while
// defaulting the speculative cap to one job's own footprint (no operator override present) so an
// unbounded pile-up can't happen before this guess has actually been tried.
func TestSpeculativeCandidatesAcceptsZeroNodeClusterBlindWithDefaultCap(t *testing.T) {
	exp := distributedExperiment(1, 4)
	exp.AcceleratorCount = 4
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-a": true}
	connected := map[string]bool{"cluster-a": true}
	clusterIDs := map[string]string{"cluster-a": "cid-a"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, nil, nil, nil, map[string]int{"cluster-a": 0}, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("a cluster with zero live nodes and nothing outstanding yet must still be a candidate")
	}

	// One job's worth (4) already outstanding + this job's 4 exceeds the implicit one-job default
	// cap, since no operator cap is set for this cluster.
	got, err = l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, nil, nil, nil, map[string]int{"cluster-a": 4}, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("with no live data and no operator cap, a zero-node cluster must default to a one-job speculative cap")
	}
}

// TestSpeculativeCandidatesTiebreaksByFewestPendingSpeculativeJobs covers the new ordering: among
// multiple otherwise-eligible clusters, the one with fewer speculative jobs already outstanding
// sorts first, spreading bets instead of piling every guaranteed job onto the same cluster.
func TestSpeculativeCandidatesTiebreaksByFewestPendingSpeculativeJobs(t *testing.T) {
	exp := distributedExperiment(1, 2)
	accel := map[string]map[string]map[string]int64{
		"cluster-busy": {"node-a": {h100: 8}},
		"cluster-idle": {"node-a": {h100: 8}},
	}
	resources := map[string]map[string]map[string]int64{
		"cluster-busy": {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
		"cluster-idle": {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
	}
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-busy": true, "cluster-idle": true}
	connected := map[string]bool{"cluster-busy": true, "cluster-idle": true}
	// cluster IDs deliberately disagree with the desired tiebreak order, to prove footprint (not
	// cluster_id) decides it.
	clusterIDs := map[string]string{"cluster-busy": "cid-a", "cluster-idle": "cid-z"}
	footprint := map[string]int{"cluster-busy": 6, "cluster-idle": 0}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, footprint, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 2 || got[0] != "cluster-idle" {
		t.Fatalf("got %v, want cluster-idle (0 pending) first ahead of cluster-busy (6 pending)", got)
	}
}

// TestSpeculativeCandidatesIncludesMismatchedFlavorClusterButRanksItLast covers a real gap: a
// cluster can run several heterogeneous node groups, and the one that would actually fit a job may
// simply have no live nodes right now (scaled to zero) while a different, non-matching node group
// (e.g. a different accelerator flavor) is what happens to be live. The old design excluded any
// cluster whose live nodes didn't prove a fit, which wrongly ruled this cluster out forever. The
// fix must still include it as a candidate — just ranked behind a cluster that DOES have live proof
// — since "no live node proves this flavor fits" is not "this cluster can never produce it."
func TestSpeculativeCandidatesIncludesMismatchedFlavorClusterButRanksItLast(t *testing.T) {
	exp := distributedExperiment(1, 2)
	exp.AcceleratorType = domain.AcceleratorType(h100)
	exp.AcceleratorCount = 2
	accel := map[string]map[string]map[string]int64{
		// cluster-amd's only live node group is a different flavor entirely (heterogeneous
		// cluster with an h100 node group currently scaled to zero) — no live proof of fit.
		"cluster-amd":  {"node-a": {"amd.com/gpu.product=MI300": 8}},
		"cluster-h100": {"node-a": {h100: 8}},
	}
	resources := map[string]map[string]map[string]int64{
		"cluster-amd":  {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
		"cluster-h100": {"node-a": {domain.NodeResourceCPUMillicores: 64000}},
	}
	l := speculationLoop(&speculationStore{})
	autoscaler := map[string]bool{"cluster-amd": true, "cluster-h100": true}
	connected := map[string]bool{"cluster-amd": true, "cluster-h100": true}
	clusterIDs := map[string]string{"cluster-amd": "cid-a", "cluster-h100": "cid-b"}

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both clusters included — a live-node flavor mismatch must never exclude a cluster outright", got)
	}
	if got[0] != "cluster-h100" || got[1] != "cluster-amd" {
		t.Fatalf("got %v, want cluster-h100 (proven fit) ranked ahead of cluster-amd (no live proof, but still eligible)", got)
	}
}

func TestSpeculativeCandidatesDisabledWithoutWithSpeculation(t *testing.T) {
	exp := distributedExperiment(1, 2)
	accel, resources := nodeFixture()
	l := NewLoop(&speculationStore{}, tickQuota{}, nil, zap.NewNop()) // no WithSpeculation call

	got, err := l.speculativeCandidates(context.Background(), newResolutionCache(l), exp,
		map[string]bool{"cluster-a": true}, map[string]bool{"cluster-a": true},
		map[string]string{"cluster-a": "cid-a"}, nil, accel, resources, nil, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a loop that never opted into speculation must never produce a candidate")
	}
}
