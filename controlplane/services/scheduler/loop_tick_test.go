package scheduler

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var cpuKey = domain.ResourceKey{Kind: domain.ResourceKindCPU}

func acceleratorKey(flavor string) domain.ResourceKey {
	return domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: flavor}
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
