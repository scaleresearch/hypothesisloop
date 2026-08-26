package scheduler

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestFitsLargestNodeRejectsClusterWithNoNodes(t *testing.T) {
	exp := distributedExperiment(1, 2)
	if fitsLargestNode(exp, nil, nil, nil) {
		t.Fatal("a cluster reporting zero nodes must never be a speculative candidate")
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

func TestSpeculativeCandidatesRequiresAutoscalerConnectedAndFit(t *testing.T) {
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
			got, err := l.speculativeCandidates(context.Background(), exp, tc.autoscaler, tc.connected, clusterIDs, nil, accel, resources, nil, nil)
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

	got, err := l.speculativeCandidates(context.Background(), exp, autoscaler, connected, clusterIDs, map[string]bool{}, accel, resources, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a gang must not speculate onto a cluster that hasn't reported multi_node_capable")
	}

	got, err = l.speculativeCandidates(context.Background(), exp, autoscaler, connected, clusterIDs, map[string]bool{"cluster-a": true}, accel, resources, nil, nil)
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

	got, err := l.speculativeCandidates(context.Background(), exp,
		map[string]bool{"cluster-a": true}, map[string]bool{"cluster-a": true},
		map[string]string{"cluster-a": "cid-a"}, nil, accel, resources, nil, nil)
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

	got, err := l.speculativeCandidates(context.Background(), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 2 || got[0] != "cluster-z" {
		t.Fatalf("got %v, want cluster-z (cid-b) first by cluster_id order", got)
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

	got, err := l.speculativeCandidates(context.Background(), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, map[string]int{"cluster-a": 2})
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("2 already outstanding + 2 requested exceeds the cap of 3, must be excluded")
	}

	got, err = l.speculativeCandidates(context.Background(), exp, autoscaler, connected, clusterIDs, nil, accel, resources, nil, map[string]int{"cluster-a": 0})
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("0 outstanding + 2 requested is within the cap of 3, must be a candidate")
	}
}

// nilCallbackClaimStore asserts submitJobTo's speculative path claims with a nil
// capacityAvailable callback — ClaimSubmitted (the real store implementation) treats that as "no
// live-fit predicate", relying on the advisory lock plus the status='QUEUED' predicate alone.
type nilCallbackClaimStore struct {
	LoopStore
	sawNilCallback bool
	claimed        bool
}

func (s *nilCallbackClaimStore) ClaimSubmitted(_ context.Context, _, _ string, _ *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.sawNilCallback = capacityAvailable == nil
	s.claimed = true
	return true, nil
}

func TestSubmitJobSpeculativePassesNilCapacityCallback(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	store := &nilCallbackClaimStore{}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", exp.AcceleratorType, true); err != nil {
		t.Fatalf("submitJobTo(speculative=true) = %v, want nil", err)
	}
	if !store.claimed {
		t.Fatal("ClaimSubmitted was never invoked")
	}
	if !store.sawNilCallback {
		t.Fatal("a speculative submit must claim with a nil capacityAvailable callback — there is no live node to re-check")
	}
}

func TestSubmitJobLiveFitPassesNonNilCapacityCallback(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	store := &nilCallbackClaimStore{}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", exp.AcceleratorType, false); err != nil {
		t.Fatalf("submitJobTo(speculative=false) = %v, want nil", err)
	}
	if store.sawNilCallback {
		t.Fatal("an ordinary live-fit submit must still re-check fresh capacity at claim time")
	}
}

func TestSpeculativeCandidatesDisabledWithoutWithSpeculation(t *testing.T) {
	exp := distributedExperiment(1, 2)
	accel, resources := nodeFixture()
	l := NewLoop(&speculationStore{}, tickQuota{}, nil, zap.NewNop()) // no WithSpeculation call

	got, err := l.speculativeCandidates(context.Background(), exp,
		map[string]bool{"cluster-a": true}, map[string]bool{"cluster-a": true},
		map[string]string{"cluster-a": "cid-a"}, nil, accel, resources, nil, nil)
	if err != nil {
		t.Fatalf("speculativeCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a loop that never opted into speculation must never produce a candidate")
	}
}
