package placement

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var cpuKey = domain.ResourceKey{Kind: domain.ResourceKindCPU}

func acceleratorKey(acceleratorType string) domain.ResourceKey {
	return domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Name: acceleratorType}
}

func TestClusterWithBestFitPrefersOutrightFit(t *testing.T) {
	avail := map[string]domain.Footprint{
		"b-cluster": {cpuKey: 1000}, // too little
		"a-cluster": {cpuKey: 4000}, // fits
	}
	need := domain.Footprint{cpuKey: 2000}
	got := BestFit(avail, nil, "org-a", need)
	if got != "a-cluster" {
		t.Errorf("clusterWithBestFit = %q, want a-cluster (the one that actually fits)", got)
	}
}

func TestClusterWithBestFitFallsBackToSmallestShortage(t *testing.T) {
	avail := map[string]domain.Footprint{
		"big-gap":   {cpuKey: 500},  // short by 1500
		"small-gap": {cpuKey: 1800}, // short by 200
	}
	need := domain.Footprint{cpuKey: 2000}
	got := BestFit(avail, nil, "org-a", need)
	if got != "small-gap" {
		t.Errorf("clusterWithBestFit = %q, want small-gap (least total shortage)", got)
	}
}

func TestSubtractFootprintClampsAtZero(t *testing.T) {
	avail := domain.Footprint{cpuKey: 500}
	SubtractFootprint(avail, domain.Footprint{cpuKey: 2000})
	if avail[cpuKey] != 0 {
		t.Errorf("avail[cpu] = %d, want 0 (clamped, not negative)", avail[cpuKey])
	}
}

// Admission proves a job fits by placing each of its nodes on a real node, and for a grouped job
// those nodes are genuinely different. Averaging them is the failure this guards: the average of
// a 16-core learner and a 1-core actor is 8.5 cores, which two 9-core hosts would satisfy while
// neither could actually hold the learner — a job admitted, reserved, and unschedulable forever.
func TestReservePlacementPlacesEachGroupsOwnShapeRatherThanAnAverage(t *testing.T) {
	exp := groupedTwoNodeExperiment()
	nodeResources := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 9000, domain.NodeResourceMemoryBytes: 200 * 1 << 30, domain.NodeResourceStorageBytes: 100 * 1 << 30},
		"node-b": {domain.NodeResourceCPUMillicores: 9000, domain.NodeResourceMemoryBytes: 200 * 1 << 30, domain.NodeResourceStorageBytes: 100 * 1 << 30},
	}
	if Reserve(nil, nodeResources, nil, exp) {
		t.Fatal("a 16-core learner was placed on a 9-core host — the per-node check used an averaged shape, and no node ever holds the average of a learner and an actor")
	}

	nodeResources["node-a"][domain.NodeResourceCPUMillicores] = 32000
	if !Reserve(nil, nodeResources, nil, exp) {
		t.Fatal("a learner and an actor were not placed on a host big enough for the learner plus one with room for the actor — each node of a grouped job is placed against its own shape")
	}
}

// Preemption is scoped to the one cluster admission picked, so picking a cluster that does not
// own the requested hardware at all is not a cosmetic mistake: the guaranteed job is handed a
// target where no burst victim can free what it needs, finds nothing to take, and stays QUEUED
// forever while the cluster that does own the hardware sits full of evictable burst work. A
// cluster reporting zero free of an accelerator type it owns is the whole point of the preemption path; a
// cluster that never reports the accelerator type is not a candidate for it.
func TestClusterWithBestFitNeverTargetsAClusterThatDoesNotOwnTheRequestedAcceleratorType(t *testing.T) {
	need := domain.Footprint{cpuKey: 250, acceleratorKey("type-l40"): 8}
	avail := map[string]domain.Footprint{
		// Sorts first, has ample CPU and no L40 whatsoever — its total shortage is the same 8 as
		// the saturated owner's, which is what made the tie land here.
		"a-no-l40-cluster": {cpuKey: 16000, acceleratorKey("type-blackhole"): 4},
		// Owns eight L40s, every one of them held by preemptible burst work.
		"z-saturated-l40-cluster": {cpuKey: 78000, acceleratorKey("type-l40"): 0},
	}
	got := BestFit(avail, nil, "org-a", need)
	if got != "z-saturated-l40-cluster" {
		t.Errorf("clusterWithBestFit = %q, want z-saturated-l40-cluster (the only cluster preemption could ever unblock)", got)
	}
}

// When no cluster owns the hardware there is no preemption target to name, and inventing one
// would send the evictor at live work on a cluster that can never satisfy the request. The empty
// name fails every downstream fit check, which is the capacity_unavailable the submitter needs.
func TestClusterWithBestFitNamesNoClusterWhenNoneOwnsTheRequestedAcceleratorType(t *testing.T) {
	need := domain.Footprint{acceleratorKey("type-h200"): 2}
	avail := map[string]domain.Footprint{
		"cluster-a": {cpuKey: 16000, acceleratorKey("type-l40"): 8},
		"cluster-b": {cpuKey: 16000, acceleratorKey("type-blackhole"): 4},
	}
	if got := BestFit(avail, nil, "org-a", need); got != "" {
		t.Errorf("clusterWithBestFit = %q, want \"\" (no cluster owns this hardware)", got)
	}
}

// groupedAcceleratorExperiment is a heterogeneous job whose accelerators live on its groups and
// nowhere else — the exact shape whose top-level accelerator_count a grouped submission REJECTS,
// and which the store therefore never back-fills.
func groupedAcceleratorExperiment() *domain.Experiment {
	const acceleratorType = "nvidia.com/gpu.product=NVIDIA-L40"
	return &domain.Experiment{
		AcceleratorType:  acceleratorType,
		AcceleratorCount: 3,
		Job: domain.JobSpec{
			AcceleratorType: acceleratorType,
			Groups: []domain.JobGroup{
				{Name: "learner", Replicas: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 2},
				{Name: "actor", Replicas: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
			},
		},
	}
}

// Placement for a job that wants accelerators must be proven against real nodes, not against a
// cluster's scalar totals. A grouped job leaves the top-level accelerator_count empty by
// construction, so a branch keyed on that field routed every heterogeneous job down the
// no-accelerator path: it skipped the node-layout proof entirely and could be handed a cluster
// whose only qualifying node had not a single free device of the hardware it asked for.
func TestAGroupedJobIsPlacedOnAClusterWhoseNodesActuallyHaveItsAccelerators(t *testing.T) {
	const acceleratorType = "nvidia.com/gpu.product=NVIDIA-L40"
	exp := groupedAcceleratorExperiment()
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"a-empty-nodes": fp, "b-real-devices": fp}
	nodes := map[string]map[string]map[string]int64{
		"a-empty-nodes":  {"node-a1": {acceleratorType: 0}, "node-a2": {acceleratorType: 0}},
		"b-real-devices": {"node-b1": {acceleratorType: 8}, "node-b2": {acceleratorType: 8}},
	}
	roomy := map[string]int64{
		domain.NodeResourceCPUMillicores: 64000,
		domain.NodeResourceMemoryBytes:   1 << 40,
		domain.NodeResourceStorageBytes:  1 << 40,
	}
	resources := map[string]map[string]map[string]int64{
		"a-empty-nodes":  {"node-a1": roomy, "node-a2": roomy},
		"b-real-devices": {"node-b1": roomy, "node-b2": roomy},
	}

	cluster, _ := ClusterAndFootprint(avail, nil, nodes, resources, nil, exp)
	if cluster != "b-real-devices" {
		t.Errorf("resolved cluster = %q, want b-real-devices — the only one whose nodes hold the requested accelerators", cluster)
	}
}

// groupedTwoNodeExperiment is a heterogeneous job of two nodes.
func groupedTwoNodeExperiment() *domain.Experiment {
	return &domain.Experiment{
		Job: domain.JobSpec{Groups: []domain.JobGroup{
			{Name: "learner", Replicas: 1, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
			{Name: "actor", Replicas: 1, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
		}},
	}
}
