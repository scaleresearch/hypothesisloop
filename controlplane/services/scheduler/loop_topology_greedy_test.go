package scheduler

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const h100 = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"

// groupedExperiment is a heterogeneous job: one group per shape, in the order given.
func groupedExperiment(groups ...domain.JobGroup) *domain.Experiment {
	return &domain.Experiment{
		AcceleratorType: h100,
		Job:             domain.JobSpec{AcceleratorType: h100, Groups: groups},
	}
}

// reservePlacement walks ranks in declared order and hands each the first (alphabetical) node
// that fits, never backtracking. A small rank placed first can take the only node that could
// host a later large rank, so a job with a valid assignment is reported as not fitting.
//
// actor (1 GPU) is declared before learner (4 GPU); node-a has 4 GPUs, node-b has 1. The valid
// assignment is actor→node-b, learner→node-a. Greedy first-fit gives actor node-a and then finds
// no host for the learner.
func TestTopologyFitsGreedyOrderFalseNegativeOnAccelerators(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "actor", Replicas: 1, AcceleratorCount: 1},
		domain.JobGroup{Name: "learner", Replicas: 1, AcceleratorCount: 4},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 4}, "node-b": {h100: 1}}
	if !topologyFits(byNode, nil, nil, exp) {
		t.Fatal("actor→node-b, learner→node-a is a valid placement, but greedy first-fit rejected the job")
	}
}

// Same root cause on the fungible dimensions: identical accelerator needs, different CPU needs.
// Rank 1 wants 1 core, rank 2 wants 8; node-a has 8 cores free, node-b has 1. Rank 1 takes
// node-a (alphabetically first) and rank 2 is stranded.
func TestTopologyFitsGreedyOrderFalseNegativeOnCPU(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "small", Replicas: 1, AcceleratorCount: 1, CPU: "1"},
		domain.JobGroup{Name: "big", Replicas: 1, AcceleratorCount: 1, CPU: "8"},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 1}, "node-b": {h100: 1}}
	resources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 8000},
		"node-b": {domain.NodeResourceCPUMillicores: 1000},
	}
	if !topologyFits(byNode, resources, nil, exp) {
		t.Fatal("small→node-b, big→node-a is a valid placement, but greedy first-fit rejected the job")
	}
}

// The ordering problem does not depend on the distinct-host rule: with spreading disabled, a
// 1-GPU rank still lands on the 2-GPU node first and starves the 2-GPU rank that follows it.
func TestTopologyFitsGreedyOrderFalseNegativeWithoutSpread(t *testing.T) {
	spread := false
	exp := groupedExperiment(
		domain.JobGroup{Name: "small", Replicas: 1, AcceleratorCount: 1},
		domain.JobGroup{Name: "big", Replicas: 1, AcceleratorCount: 2},
	)
	exp.Job.Topology = &domain.TopologySpec{SpreadAcrossHosts: &spread}
	byNode := map[string]map[string]int64{"node-a": {h100: 2}, "node-b": {h100: 1}}
	if !topologyFits(byNode, nil, nil, exp) {
		t.Fatal("small→node-b, big→node-a is a valid placement, but greedy first-fit rejected the job")
	}
}

// desiredPlacementFits (the ClaimSubmitted re-check) replays already-desired jobs with the same
// first-fit walk, so it can strand capacity a candidate needs even though the runtime is free to
// place the desired job elsewhere. The claim then fails with errAdmissionCapacityChanged for a
// job the cluster can host.
func TestDesiredPlacementFitsGreedyReplayFalseNegative(t *testing.T) {
	spread := false
	pending := distributedExperiment(1, 1)
	candidate := distributedExperiment(1, 2)
	candidate.Job.Topology = &domain.TopologySpec{SpreadAcrossHosts: &spread}
	byNode := map[string]map[string]int64{"node-a": {h100: 2}, "node-b": {h100: 1}}
	if !desiredPlacementFits(byNode, nil, nil, []*domain.Experiment{pending}, candidate) {
		t.Fatal("pending→node-b leaves node-a for the 2-GPU candidate, but the greedy replay put pending on node-a and rejected the claim")
	}
}

// reservePlacement subtracts each rank as it is placed and does not roll back when a later rank
// finds no host. topologyFits hides this behind a clone, but the function's own contract is that
// a false answer has changed nothing — otherwise a caller that (like desiredPlacementFits)
// keeps using the map after a false has lost capacity that was never claimed.
func TestReservePlacementFailureLeavesInputUntouched(t *testing.T) {
	exp := distributedExperiment(2, 1)
	byNode := map[string]map[string]int64{"node-a": {h100: 1}}
	if reservePlacement(byNode, nil, nil, exp) {
		t.Fatal("two distinct ranks cannot fit on one node")
	}
	if got := byNode["node-a"][h100]; got != 1 {
		t.Fatalf("a failed reservation consumed capacity: node-a has %d free, want 1", got)
	}
}

// ExtraResources (arbitrary extended resources) are part of the cluster-scalar footprint but not
// of NodeShapes, so topologyFits never checks that any single node has them. Two nodes with one
// device each satisfy the cluster total of 2 while no node can run the rank that needs 2.
//
// Known gap, not fixable in the scheduler alone: cluster agents report only CPU/memory/storage
// per node (see clusteragentapi), so there is no per-node figure to check against. Skipped until
// the agent protocol carries extended resources per node; the assertion below is the contract.
func TestTopologyFitsIgnoresExtraResourcesPerNode(t *testing.T) {
	t.Skip("per-node extended resources are not reported by cluster agents yet")
	exp := &domain.Experiment{Job: domain.JobSpec{NumNodes: 1, CPU: "1", ExtraResources: map[string]string{"example.com/fpga": "2"}}}
	resources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 4000, "example.com/fpga": 1},
		"node-b": {domain.NodeResourceCPUMillicores: 4000, "example.com/fpga": 1},
	}
	if topologyFits(nil, resources, nil, exp) {
		t.Fatal("no node has 2 example.com/fpga, yet the placement was accepted on cluster-wide total alone")
	}
}

// A CPU-only job (TotalAccelerators == 0) is now judged on node layout too: cluster-a's aggregate
// is large enough (6 cores across two nodes) but no single node can host the whole rank, while
// cluster-b's one node can. Before this went through placeAtFlavor, clusterWithBestFit picked
// cluster-a on the scalar total alone and the tick then failed topologyFits there and went to
// preemption instead of using cluster-b.
func TestResolveClusterAndFootprintCPUOnlySkipsTopologyCheck(t *testing.T) {
	exp := &domain.Experiment{Job: domain.JobSpec{NumNodes: 1, CPU: "6"}}
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"cluster-a": fp, "cluster-b": fp}
	resources := map[string]map[string]map[string]int64{
		"cluster-a": {"node-a1": {domain.NodeResourceCPUMillicores: 3000}, "node-a2": {domain.NodeResourceCPUMillicores: 3000}},
		"cluster-b": {"node-b1": {domain.NodeResourceCPUMillicores: 6000}},
	}
	cluster, _ := resolveClusterAndFootprint(avail, nil, resources, nil, exp)
	if cluster != "cluster-b" {
		t.Fatalf("resolved cluster = %q, want cluster-b (the only cluster with a node that can hold a 6-core rank)", cluster)
	}
}

// The search must not turn a homogeneous cluster into an exponential walk: 64 identical ranks
// on 64 identical nodes, plus one rank too many, has to answer "no" quickly rather than trying
// every permutation of equivalent nodes.
func TestTopologyFitsInfeasibleHomogeneousJobAnswersFast(t *testing.T) {
	byNode := map[string]map[string]int64{}
	resources := map[string]map[string]int64{}
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("node-%02d", i)
		byNode[name] = map[string]int64{h100: 8}
		resources[name] = map[string]int64{domain.NodeResourceCPUMillicores: 64000}
	}
	exp := distributedExperiment(65, 8)
	exp.Job.CPU = "4"
	start := time.Now()
	if topologyFits(byNode, resources, nil, exp) {
		t.Fatal("65 distinct ranks were accepted on 64 nodes")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("infeasible placement took %s to reject", elapsed)
	}
	exp = distributedExperiment(64, 8)
	exp.Job.CPU = "4"
	if !topologyFits(byNode, resources, nil, exp) {
		t.Fatal("64 distinct ranks were rejected on 64 matching nodes")
	}
}

// Backtracking must not manufacture a placement: two 4-GPU ranks and one 1-GPU rank on nodes
// with 4, 2 and 1 free devices have no assignment however the search reorders them.
func TestTopologyFitsBacktrackingStillRejectsInfeasible(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "actor", Replicas: 1, AcceleratorCount: 1},
		domain.JobGroup{Name: "learner", Replicas: 2, AcceleratorCount: 4},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 4}, "node-b": {h100: 2}, "node-c": {h100: 1}}
	if topologyFits(byNode, nil, nil, exp) {
		t.Fatal("two 4-GPU ranks were accepted with only one 4-GPU node")
	}
}

// A layout that needs backtracking on both dimensions at once: the only node with 8 cores has
// 1 GPU, the 2-GPU node has 2 cores. Rank A wants 2 GPUs + 2 cores, rank B wants 1 GPU + 8 cores.
// Hardest-first by accelerators puts A first; best-fit then has to keep the 8-core node for B.
func TestTopologyFitsCrossDimensionAssignment(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "a", Replicas: 1, AcceleratorCount: 2, CPU: "2"},
		domain.JobGroup{Name: "b", Replicas: 1, AcceleratorCount: 1, CPU: "8"},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 2}, "node-b": {h100: 2}}
	resources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 8000},
		"node-b": {domain.NodeResourceCPUMillicores: 2000},
	}
	if !topologyFits(byNode, resources, nil, exp) {
		t.Fatal("a→node-b, b→node-a is a valid placement but was rejected")
	}
}

// A successful reservation subtracts exactly the planned assignment and nothing else, so the
// next job in the same tick sees the true remainder.
func TestReservePlacementCommitsExactlyThePlan(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "actor", Replicas: 1, AcceleratorCount: 1, CPU: "1"},
		domain.JobGroup{Name: "learner", Replicas: 1, AcceleratorCount: 4, CPU: "2"},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 4}, "node-b": {h100: 1}}
	resources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 4000},
		"node-b": {domain.NodeResourceCPUMillicores: 4000},
	}
	if !reservePlacement(byNode, resources, nil, exp) {
		t.Fatal("placement rejected")
	}
	if byNode["node-a"][h100] != 0 || byNode["node-b"][h100] != 0 {
		t.Fatalf("accelerators after reservation = %v, want both nodes drained", byNode)
	}
	if resources["node-a"][domain.NodeResourceCPUMillicores] != 2000 || resources["node-b"][domain.NodeResourceCPUMillicores] != 3000 {
		t.Fatalf("cpu after reservation = %v, want node-a 2000 / node-b 3000", resources)
	}
}

// A CPU-only job that fits nowhere in node layout falls back to the scalar best-fit cluster
// rather than reporting "no cluster" outright — the caller (placeAtFlavor's own caller in the
// tick) still re-checks topologyFits on the chosen cluster and routes to preemption from there.
// This documents that fallback rather than assuming resolveClusterAndFootprint itself proves
// feasibility for a CPU-only job when nothing actually fits.
func TestResolveClusterAndFootprintCPUOnlyFallsBackWhenNoNodeFits(t *testing.T) {
	exp := &domain.Experiment{Job: domain.JobSpec{NumNodes: 1, CPU: "6"}}
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"cluster-a": fp}
	resources := map[string]map[string]map[string]int64{
		"cluster-a": {"node-a1": {domain.NodeResourceCPUMillicores: 3000}, "node-a2": {domain.NodeResourceCPUMillicores: 3000}},
	}
	cluster, gotFP := resolveClusterAndFootprint(avail, nil, resources, nil, exp)
	if cluster != "cluster-a" {
		t.Fatalf("resolved cluster = %q, want the scalar best-fit fallback cluster-a", cluster)
	}
	if !domain.Fits(avail[cluster], gotFP) {
		t.Fatal("fallback cluster should still satisfy the scalar footprint even though no single node does")
	}
}

// placeAtFlavor's ok=false path: every candidate cluster fits on the scalar footprint but none
// has node layout for it.
func TestPlaceAtFlavorReturnsFalseWhenNoClusterHasLayout(t *testing.T) {
	exp := distributedExperiment(2, 1)
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"cluster-a": fp}
	nodeAvail := map[string]map[string]map[string]int64{
		"cluster-a": {"node-a": {h100: 2}}, // one node has both accelerators; a spread job needs two distinct hosts
	}
	cluster, ok := placeAtFlavor(avail, nodeAvail, nil, nil, exp)
	if ok {
		t.Fatalf("placeAtFlavor reported ok=true with cluster %q, want false: no cluster has two distinct qualifying hosts", cluster)
	}
}

// foldMatchingKey must find a live-reported accelerator key regardless of the casing the driver
// used, not just when it matches the job's key verbatim.
func TestFoldMatchingKeyIsCaseInsensitive(t *testing.T) {
	reported := map[string]int64{"NVIDIA.COM/GPU.PRODUCT=NVIDIA-H100-80GB-HBM3": 4}
	if got := foldMatchingKey(reported, h100); got != "NVIDIA.COM/GPU.PRODUCT=NVIDIA-H100-80GB-HBM3" {
		t.Fatalf("foldMatchingKey = %q, want the differently-cased key actually present in the map", got)
	}
	// No match at all, even case-insensitively: the input key is returned unchanged so a
	// subsequent lookup misses cleanly rather than panicking on a nonexistent key.
	if got := foldMatchingKey(map[string]int64{"other-flavor": 1}, h100); got != h100 {
		t.Fatalf("foldMatchingKey with no match = %q, want the original key echoed back", got)
	}
}

// The placement search must degrade to "does not fit" — never hang or panic — when an
// adversarial layout burns through its whole search budget. Lowering the budget to a handful of
// trials on a job that genuinely needs backtracking exercises that path directly rather than
// relying on a huge cluster to exhaust the real 50k budget.
func TestPlanPlacementBudgetExhaustionIsConservative(t *testing.T) {
	original := placementSearchBudget
	placementSearchBudget = 1
	defer func() { placementSearchBudget = original }()

	// The same layout as TestTopologyFitsCrossDimensionAssignment, which is feasible but only via
	// the second candidate tried for one of the ranks — exactly what a budget of 1 trial cannot
	// reach.
	exp := groupedExperiment(
		domain.JobGroup{Name: "a", Replicas: 1, AcceleratorCount: 2, CPU: "2"},
		domain.JobGroup{Name: "b", Replicas: 1, AcceleratorCount: 1, CPU: "8"},
	)
	byNode := map[string]map[string]int64{"node-a": {h100: 2}, "node-b": {h100: 2}}
	resources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 8000},
		"node-b": {domain.NodeResourceCPUMillicores: 2000},
	}
	if topologyFits(byNode, resources, nil, exp) {
		t.Fatal("a budget of 1 trial should not be enough to find this placement")
	}
	// The real budget must still find it.
	placementSearchBudget = original
	if !topologyFits(byNode, resources, nil, exp) {
		t.Fatal("with its real budget the same placement should still be found")
	}
}

// nodeHasRoom's own edge cases, exercised directly rather than only through a full reservation.
func TestNodeHasRoomEdgeCases(t *testing.T) {
	if !nodeHasRoom(nil, nil) {
		t.Fatal("a rank that needs nothing fits a node with no reported resources at all")
	}
	if nodeHasRoom(nil, map[string]int64{domain.NodeResourceCPUMillicores: 1}) {
		t.Fatal("a node the cluster reported no resources for cannot be proven to have room for a rank that needs some")
	}
	if !nodeHasRoom(map[string]int64{domain.NodeResourceCPUMillicores: 0}, nil) {
		t.Fatal("a rank that needs nothing fits even a node reporting zero of everything")
	}
}

// When an accelerator job has an alternate acceptable flavor and neither flavor fits anywhere,
// resolveClusterAndFootprint must restore the originally requested flavor (so the caller's own
// preemption path reasons about the flavor the job actually asked for) and return the requested
// flavor's own best-fit fallback, not the alternate's.
func TestResolveClusterAndFootprintRestoresRequestedFlavorWhenNothingFits(t *testing.T) {
	const altFlavor = "nvidia.com/gpu.product=NVIDIA-A100-80GB"
	exp := distributedExperiment(1, 8) // 8 accelerators, too many for either cluster below
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{altFlavor}
	fpRequested := exp.Footprint()
	avail := map[string]domain.Footprint{"cluster-a": fpRequested}
	cluster, fp := resolveClusterAndFootprint(avail, nil, nil, nil, exp)
	if exp.AcceleratorType != h100 {
		t.Fatalf("AcceleratorType left as %q after failed substitution, want restored to requested %q", exp.AcceleratorType, h100)
	}
	if cluster != "cluster-a" {
		t.Fatalf("fallback cluster = %q, want the requested flavor's own best-fit cluster-a", cluster)
	}
	if !reflect.DeepEqual(fp, fpRequested) {
		t.Fatalf("fallback footprint = %v, want the requested flavor's footprint %v", fp, fpRequested)
	}
}
