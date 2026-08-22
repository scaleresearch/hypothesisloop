package scheduler

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var cpuKey = domain.ResourceKey{Kind: domain.ResourceKindCPU}

func acceleratorKey(flavor string) domain.ResourceKey {
	return domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: flavor}
}

func TestClusterWithBestFitPrefersOutrightFit(t *testing.T) {
	avail := map[string]domain.Footprint{
		"b-cluster": {cpuKey: 1000}, // too little
		"a-cluster": {cpuKey: 4000}, // fits
	}
	need := domain.Footprint{cpuKey: 2000}
	got := clusterWithBestFit(avail, need)
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
	got := clusterWithBestFit(avail, need)
	if got != "small-gap" {
		t.Errorf("clusterWithBestFit = %q, want small-gap (least total shortage)", got)
	}
}

func TestSubtractFootprintClampsAtZero(t *testing.T) {
	avail := domain.Footprint{cpuKey: 500}
	subtractFootprint(avail, domain.Footprint{cpuKey: 2000})
	if avail[cpuKey] != 0 {
		t.Errorf("avail[cpu] = %d, want 0 (clamped, not negative)", avail[cpuKey])
	}
}

func TestShortfallOnlyCountsDeficitDimensions(t *testing.T) {
	avail := domain.Footprint{cpuKey: 3000, acceleratorKey("flavor-t4"): 5}
	need := domain.Footprint{cpuKey: 4000, acceleratorKey("flavor-t4"): 2}
	got := shortfall(avail, need)
	if got[cpuKey] != 1000 {
		t.Errorf("shortfall[cpu] = %d, want 1000", got[cpuKey])
	}
	if v, ok := got[acceleratorKey("flavor-t4")]; ok && v > 0 {
		t.Errorf("shortfall[accelerator] = %d, want 0/absent (capacity already covers this dimension)", v)
	}
}

func TestNotAdmittedReasonDistinguishesScarcityFromOutranking(t *testing.T) {
	need := domain.Footprint{cpuKey: 2000, acceleratorKey("flavor-t4"): 1}

	// Never fit this cluster to begin with: waiting for the queue to drain cannot admit it, so
	// telling the submitter it was outranked would point them at the wrong remedy.
	if got := notAdmittedReasonFor(false, need, nil); got != domain.NotAdmittedCapacityUnavailable {
		t.Fatalf("unchanged insufficient capacity reason = %q", got)
	}
	// Would have fit against the capacity the tick opened with; jobs ahead of it took that.
	if got := notAdmittedReasonFor(true, need, nil); got != domain.NotAdmittedOutranked {
		t.Fatalf("capacity consumed earlier in tick reason = %q", got)
	}
	shortage := domain.Footprint{cpuKey: 1000}
	if got := notAdmittedReasonFor(false, need, shortage); got != domain.NotAdmittedCapacityUnavailable+": short "+footprintStr(shortage) {
		t.Fatalf("detailed shortage reason = %q", got)
	}
	// A shortage vector alongside a fit at tick start still means outranked: the shortfall is
	// measured against what is left now, which is the definition of having lost it.
	if got := notAdmittedReasonFor(true, need, shortage); got != domain.NotAdmittedOutranked {
		t.Fatalf("outranked with a live shortfall reason = %q", got)
	}
}

// groupedTwoNodeExperiment is a heterogeneous job of two nodes — the smallest thing that is not
// single-node and therefore cannot run on every runtime.
func groupedTwoNodeExperiment() *domain.Experiment {
	return &domain.Experiment{
		Job: domain.JobSpec{Groups: []domain.JobGroup{
			{Name: "learner", Replicas: 1, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
			{Name: "actor", Replicas: 1, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
		}},
	}
}

// Whether a cluster can run work spanning several nodes is a fact the cluster reports about
// itself, and admission has to filter on it. Without this, a distributed job is placed on a
// single-node runtime, claims a reservation, and only then fails at workload creation — an
// admission-time question answered at execution time, once per reconcile pass, forever.
func TestAGroupedJobIsNotPlacedOnAClusterReportingNoMultiNodeCapability(t *testing.T) {
	avail := map[string]domain.Footprint{
		"bare-metal": {cpuKey: 100000},
		"k8s":        {cpuKey: 100000},
	}
	capable := map[string]bool{"bare-metal": false, "k8s": true}

	candidates := eligibleClusters(avail, capable, groupedTwoNodeExperiment())
	if _, present := candidates["bare-metal"]; present {
		t.Fatalf("a two-node grouped job was offered a cluster that reports it cannot run multi-node work — it would be admitted there and fail at creation, holding a reservation the whole time")
	}
	if _, present := candidates["k8s"]; !present {
		t.Fatalf("the cluster that DOES report multi-node capability was filtered out — the job would sit queued forever with a runtime able to run it idle")
	}
}

// A cluster that has not reported recently is absent from the capability map, and absence must
// read as "cannot". Crediting a silent cluster with a capability is how a job gets placed on a
// runtime nobody has heard from, which is the one direction this filter must never fail in.
func TestAGroupedJobIsNotPlacedOnAClusterThatHasReportedNoCapabilityAtAll(t *testing.T) {
	avail := map[string]domain.Footprint{"silent": {cpuKey: 100000}}

	candidates := eligibleClusters(avail, map[string]bool{}, groupedTwoNodeExperiment())
	if len(candidates) != 0 {
		t.Fatalf("a cluster with no capability report was offered a multi-node job (%v) — missing evidence must fail closed, exactly as missing capacity data does in domain.Fits", candidates)
	}
}

// The backward-compatibility claim for placement: a single-node job must see every cluster it
// saw before, capability report or not. Applying the multi-node filter to it would strand every
// ordinary job on any cluster that had not yet reported the new field.
func TestASingleNodeJobStillSeesEveryClusterRegardlessOfMultiNodeCapability(t *testing.T) {
	avail := map[string]domain.Footprint{
		"bare-metal": {cpuKey: 100000},
		"k8s":        {cpuKey: 100000},
	}
	exp := &domain.Experiment{Job: domain.JobSpec{CPU: "1", Memory: "1Gi", Storage: "1Gi"}}

	candidates := eligibleClusters(avail, map[string]bool{}, exp)
	if len(candidates) != 2 {
		t.Fatalf("a single-node job was offered %d of 2 clusters — it fits on one node by definition, so no capability question arises and none may be asked", len(candidates))
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
	if reservePlacement(nil, nodeResources, nil, exp) {
		t.Fatal("a 16-core learner was placed on a 9-core host — the per-node check used an averaged shape, and no node ever holds the average of a learner and an actor")
	}

	nodeResources["node-a"][domain.NodeResourceCPUMillicores] = 32000
	if !reservePlacement(nil, nodeResources, nil, exp) {
		t.Fatal("a learner and an actor were not placed on a host big enough for the learner plus one with room for the actor — each node of a grouped job is placed against its own shape")
	}
}
