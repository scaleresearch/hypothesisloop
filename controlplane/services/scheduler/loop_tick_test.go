package scheduler

import (
	"strings"
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
	got := clusterWithBestFit(avail, nil, need)
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
	got := clusterWithBestFit(avail, nil, need)
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
	code := func(stored string) string {
		reason, _ := domain.SplitQueueReason(stored)
		return reason
	}
	// Never fit this cluster to begin with: waiting for the queue to drain cannot admit it, so
	// telling the submitter it was outranked would point them at the wrong remedy.
	if got := notAdmittedReasonFor(false, false, false, nil, "c", holders{}); code(got) != domain.NotAdmittedCapacityUnavailable {
		t.Fatalf("unchanged insufficient capacity reason = %q", got)
	}
	// Would have fit against the capacity the tick opened with; jobs ahead of it took that.
	if got := notAdmittedReasonFor(true, false, false, nil, "c", holders{}); code(got) != domain.NotAdmittedOutranked {
		t.Fatalf("capacity consumed earlier in tick reason = %q", got)
	}
	// A shortage vector alongside a fit at tick start still means outranked: the shortfall is
	// measured against what is left now, which is the definition of having lost it.
	if got := notAdmittedReasonFor(true, true, false, domain.Footprint{cpuKey: 1000}, "c", holders{}); code(got) != domain.NotAdmittedOutranked {
		t.Fatalf("outranked with a live shortfall reason = %q", got)
	}
}

// Right after a job ended, the next one read "short of 61.7 GiB memory, which no job holds: the
// cluster's nodes do not have it free". With every placed job counted the cluster had the room,
// and its last report did not show it yet: the job waits for a report, and is told so. A device
// can be held by something the platform never placed, so a job short of one is not promised it.
func TestAShortageDesiredStateHasFreedWaitsForTheNextCapacityReport(t *testing.T) {
	memory := domain.ResourceKey{Kind: domain.ResourceKindMemory}
	cores := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "hypothesisloop.dev/cpu=core"}
	chip := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "tenstorrent.com/chiparch=blackhole"}
	const pending = "capacity_unavailable: cluster box-b is short of 16 cores, 61.7 GiB memory by its last capacity report, and no job the platform has placed there holds it: a job that ended is not reported gone yet, and this one is waiting for the cluster's next capacity report"
	short := domain.Footprint{cores: 16, cpuKey: 16000, memory: 66302314496}
	if got := notAdmittedReasonFor(false, false, true, short, "box-b", holders{}); got != pending {
		t.Errorf("freed in desired state:\n%q, want\n%q", got, pending)
	}
	if got := notAdmittedReasonFor(false, false, true, short, "box-b", holders{others: 1}); got != pending {
		t.Errorf("freed in desired state beside a job that holds some:\n%q, want\n%q", got, pending)
	}
	for name, got := range map[string]string{
		"not freed in desired state": notAdmittedReasonFor(false, false, false, short, "box-b", holders{}),
		"short of a device":          notAdmittedReasonFor(false, false, true, domain.Footprint{chip: 1, memory: 1 << 30}, "box-b", holders{}),
		"outranked":                  notAdmittedReasonFor(true, false, true, short, "box-b", holders{}),
	} {
		if strings.Contains(got, "next capacity report") {
			t.Errorf("%s: %q promises a report will free it", name, got)
		}
	}
}

// A queue reason read as "short {accelerator:hypothesisloop.dev/cpu=core=16,cpu:=16000,
// memory:=66302314496,storage:=1967854743552}". It is a code and a sentence: the cluster, what
// it is short of in units a person reads, and who holds that, counted and never named.
func TestAWaitForCapacitySaysWhatIsShortAndWhoHoldsIt(t *testing.T) {
	cores := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "hypothesisloop.dev/cpu=core"}
	chip := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "tenstorrent.com/chiparch=blackhole"}
	memory := domain.ResourceKey{Kind: domain.ResourceKindMemory}
	storage := domain.ResourceKey{Kind: domain.ResourceKindStorage}
	for name, tc := range map[string]struct {
		scalarFits bool
		shortage   domain.Footprint
		cluster    string
		held       holders
		want       string
	}{
		"cores, held by both": {false, domain.Footprint{cores: 16, cpuKey: 16000, memory: 66302314496, storage: 1967854743552}, "box-b", holders{own: 1, others: 1},
			"cluster box-b is short of 16 cores, 61.7 GiB memory, 1.8 TiB storage, held by 2 jobs: 1 of this platform experiment and 1 of other platform experiments"},
		"a chip, held by another's": {false, domain.Footprint{chip: 1}, "box-b", holders{others: 1},
			"cluster box-b is short of 1 x tenstorrent.com/chiparch=blackhole, held by 1 job of other platform experiments"},
		"its own jobs": {false, domain.Footprint{cpuKey: 500}, "box-b", holders{own: 3},
			"cluster box-b is short of 0.5 cores, held by 3 jobs of this platform experiment"},
		"nobody holds it": {false, domain.Footprint{storage: 485902352384}, "box-b", holders{},
			"cluster box-b is short of 452.5 GiB storage, which no job holds: the cluster's nodes do not have it free"},
		"no node layout": {true, domain.Footprint{memory: 8 << 30}, "box-b", holders{own: 1},
			"cluster box-b has room for it in total and no node of it holds the job: the node nearest to holding it is short of 8 GiB memory"},
		"no cluster picked": {false, domain.Footprint{chip: 1, cpuKey: 8000}, "", holders{},
			"no cluster that runs it has room for it now: it is short of 1 x tenstorrent.com/chiparch=blackhole, 8 cores"},
	} {
		reason, message := domain.SplitQueueReason(notAdmittedReasonFor(false, tc.scalarFits, false, tc.shortage, tc.cluster, tc.held))
		if reason != domain.NotAdmittedCapacityUnavailable || message != tc.want {
			t.Errorf("%s:\n got %s: %s\nwant %s: %s", name, reason, message, domain.NotAdmittedCapacityUnavailable, tc.want)
		}
	}
}

func TestHoldersCountTheJobsThatHoldWhatIsShortOnTheCluster(t *testing.T) {
	job := func(id, pe, cluster, cpu string) *domain.Experiment {
		return &domain.Experiment{ID: id, PlatformExperimentID: pe, ClusterName: cluster, Job: domain.JobSpec{CPU: cpu, Storage: "1Gi"}}
	}
	chipJob := job("chip", "pe-other", "box-b", "")
	chipJob.AcceleratorType, chipJob.AcceleratorCount = "tenstorrent.com/chipArch=blackhole", 1
	chipJob.Job.Storage = ""
	claimed := []*domain.Experiment{
		job("mine", "pe-1", "box-b", "16"),
		job("theirs", "pe-2", "box-b", "16"),
		job("elsewhere", "pe-2", "other", "16"),
		job("waiting", "pe-1", "box-b", "16"),
		chipJob,
	}
	waiting := claimed[3]
	if got := holdersOf(claimed, "box-b", domain.Footprint{cpuKey: 16000}, waiting); got != (holders{own: 1, others: 1}) {
		t.Errorf("short of cores: got = %+v, want one of its own experiment and one of another, itself and the job that holds no cores left out", got)
	}
	if got := holdersOf(claimed, "box-b", domain.Footprint{acceleratorKey("tenstorrent.com/chiparch=blackhole"): 1}, waiting); got != (holders{others: 1}) {
		t.Errorf("short of a chip: got = %+v, want the one job that holds a chip", got)
	}
}

// groupedTwoNodeExperiment is a heterogeneous job of two nodes — the smallest thing that is not
// single-node and therefore cannot run on every runtime.
func groupedTwoNodeExperiment() *domain.Experiment {
	return &domain.Experiment{
		Job: domain.JobSpec{Image: "registry.test/job:1", Groups: []domain.JobGroup{
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

	candidates, _ := eligibleClusters(avail, capable, ownClusters(avail), groupedTwoNodeExperiment())
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

	candidates, _ := eligibleClusters(avail, map[string]bool{}, ownClusters(avail), groupedTwoNodeExperiment())
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
	exp := &domain.Experiment{Job: domain.JobSpec{Image: "registry.test/job:1", CPU: "1", Memory: "1Gi", Storage: "1Gi"}}

	candidates, _ := eligibleClusters(avail, map[string]bool{}, ownClusters(avail), exp)
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

// Preemption is scoped to the one cluster admission picked, so picking a cluster that does not
// own the requested hardware at all is not a cosmetic mistake: the guaranteed job is handed a
// target where no burst victim can free what it needs, finds nothing to take, and stays QUEUED
// forever while the cluster that does own the hardware sits full of evictable burst work. A
// cluster reporting zero free of a flavor it owns is the whole point of the preemption path; a
// cluster that never reports the flavor is not a candidate for it.
func TestClusterWithBestFitNeverTargetsAClusterThatDoesNotOwnTheRequestedFlavor(t *testing.T) {
	need := domain.Footprint{cpuKey: 250, acceleratorKey("flavor-l40"): 8}
	avail := map[string]domain.Footprint{
		// Sorts first, has ample CPU and no L40 whatsoever — its total shortage is the same 8 as
		// the saturated owner's, which is what made the tie land here.
		"a-no-l40-cluster": {cpuKey: 16000, acceleratorKey("flavor-blackhole"): 4},
		// Owns eight L40s, every one of them held by preemptible burst work.
		"z-saturated-l40-cluster": {cpuKey: 78000, acceleratorKey("flavor-l40"): 0},
	}
	got := clusterWithBestFit(avail, nil, need)
	if got != "z-saturated-l40-cluster" {
		t.Errorf("clusterWithBestFit = %q, want z-saturated-l40-cluster (the only cluster preemption could ever unblock)", got)
	}
}

// When no cluster owns the hardware there is no preemption target to name, and inventing one
// would send the evictor at live work on a cluster that can never satisfy the request. The empty
// name fails every downstream fit check, which is the capacity_unavailable the submitter needs.
func TestClusterWithBestFitNamesNoClusterWhenNoneOwnsTheRequestedFlavor(t *testing.T) {
	need := domain.Footprint{acceleratorKey("flavor-h200"): 2}
	avail := map[string]domain.Footprint{
		"cluster-a": {cpuKey: 16000, acceleratorKey("flavor-l40"): 8},
		"cluster-b": {cpuKey: 16000, acceleratorKey("flavor-blackhole"): 4},
	}
	if got := clusterWithBestFit(avail, nil, need); got != "" {
		t.Errorf("clusterWithBestFit = %q, want \"\" (no cluster owns this hardware)", got)
	}
}

// The reason written on a skipped row is the only thing a submitter can act on, so it must name
// what actually happened. A single-node job is never narrowed by the multi-node filter, so an
// empty candidate set for one can only mean no cluster reported capacity at all — a heartbeat
// gap that will pass. Reporting it as "no multi-node cluster" told an agent its plain one-node
// job had been refused for spanning hosts it never asked to span, and pointed it at a shrink it
// cannot make.
func TestASingleNodeJobIsNeverReportedAsNarrowedByTheMultiNodeFilter(t *testing.T) {
	exp := &domain.Experiment{Job: domain.JobSpec{Image: "registry.test/job:1", CPU: "1", Memory: "1Gi", Storage: "1Gi"}}

	candidates, ineligible := eligibleClusters(map[string]domain.Footprint{}, map[string]bool{}, singleOrg, exp)
	if len(candidates) != 0 {
		t.Fatalf("no cluster reported capacity yet %d candidates were offered", len(candidates))
	}
	if ineligible != "" {
		t.Errorf("a single-node job with no cluster connected was refused as %q: no filter looked at any cluster", ineligible)
	}
}

// groupedAcceleratorExperiment is a heterogeneous job whose accelerators live on its groups and
// nowhere else — the exact shape whose top-level accelerator_count a grouped submission REJECTS,
// and which the store therefore never back-fills.
func groupedAcceleratorExperiment() *domain.Experiment {
	const flavor = "nvidia.com/gpu.product=nvidia-l40"
	return &domain.Experiment{
		AcceleratorType:  flavor,
		AcceleratorCount: 3,
		Job: domain.JobSpec{Image: "registry.test/job:1",
			AcceleratorType: flavor,
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
	const flavor = "nvidia.com/gpu.product=nvidia-l40"
	exp := groupedAcceleratorExperiment()
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"a-empty-nodes": fp, "b-real-devices": fp}
	nodes := map[string]map[string]map[string]int64{
		"a-empty-nodes":  {"node-a1": {flavor: 0}, "node-a2": {flavor: 0}},
		"b-real-devices": {"node-b1": {flavor: 8}, "node-b2": {flavor: 8}},
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

	cluster, _ := resolveClusterAndFootprint(avail, nil, nodes, resources, nil, exp)
	if cluster != "b-real-devices" {
		t.Errorf("resolved cluster = %q, want b-real-devices — the only one whose nodes hold the requested accelerators", cluster)
	}
}
