package scheduler

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The shape that produced this pass: a 16-core node carrying 4 accelerators, where a job holding
// one accelerator requests 14 cores and strands the other three.
const (
	disbalanceFlavor    = "tenstorrent.com/chipArch=blackhole"
	disbalanceNodeCores = 16.0
	disbalanceNodeChips = 4
)

var disbalanceMemKey = domain.ResourceKey{Kind: domain.ResourceKindMemory}

func disbalanceClusterTotal() domain.Footprint {
	return domain.CapacityFootprint(disbalanceNodeCores, map[string]int64{disbalanceFlavor: disbalanceNodeChips}, 64<<30, 0)
}

// disbalanceNodesTotal reports node-a's own installed CPU/memory/storage — the per-node
// denominator selectDisbalanceVictims now measures fair share against, in place of a cluster-wide
// average. Matches disbalanceClusterTotal's numbers exactly since every test here models a single
// node cluster.
func disbalanceNodesTotal() map[string]map[string]int64 {
	return map[string]map[string]int64{"node-a": {
		domain.NodeResourceCPUMillicores: int64(disbalanceNodeCores * 1000),
		domain.NodeResourceMemoryBytes:   64 << 30,
	}}
}

// disbalanceJob builds a RUNNING one-node accelerator job requesting cpu cores and 8Gi.
func disbalanceJob(id, cpu string, accelerators int) *domain.Experiment {
	return &domain.Experiment{
		ID:               id,
		ClusterName:      "cluster-a",
		Status:           domain.StatusRunning,
		AcceleratorType:  disbalanceFlavor,
		AcceleratorCount: accelerators,
		Job: domain.JobSpec{
			CPU:              cpu,
			Memory:           "8Gi",
			AcceleratorCount: accelerators,
			AcceleratorType:  disbalanceFlavor,
		},
	}
}

// disbalanceNodes reports freeChips accelerators still free on node-a, in the driver's own
// casing (which differs from the canonical lowercase footprint key on purpose).
func disbalanceNodes(freeChips int64) map[string]map[string]int64 {
	return map[string]map[string]int64{"node-a": {disbalanceFlavor: freeChips}}
}

func TestSelectDisbalanceVictimsEvictsHogStrandingIdleAccelerators(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	hog := disbalanceJob("hog", "14", 1) // 14 cores for 1 of 4 chips = 3.5x its 4-core share
	// Only 0.5 cores left on the cluster, but three of the four accelerators are free.
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the disproportionate job stranding the idle accelerators", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsSparesProportionateJob(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	// 3 cores for 1 of 4 chips is below the 4-core proportionate share — legitimately sized,
	// even though it is the reason the cluster has no CPU left.
	fair := disbalanceJob("fair", "3", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: fair, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: the job's CPU is proportionate to the accelerators it holds", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsSparesHogWhenNoAcceleratorsAreIdle(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	hog := disbalanceJob("hog", "14", 1)
	// Same disproportionate job, but every accelerator on the node is in use — nothing is being
	// stranded, so there is no harm to undo.
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 0}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(0), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: no accelerator is idle, so nothing is blocked", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsSparesHogWhenAcceleratorsAreThemselvesShort(t *testing.T) {
	// The blocked job wants two accelerators; node-a has only one free. Its shortage spans the
	// accelerator dimension, which means scarcity, not disproportion — evicting anything here
	// would be punishing a job for a shortage it did not cause.
	blocked := disbalanceJob("blocked", "2", 2)
	hog := disbalanceJob("hog", "14", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 1}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(1), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: the accelerator dimension is short too", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsFailsSafeOnMissingNodeAttribution(t *testing.T) {
	// A genuinely disbalanced job, but the metrics store has no job->node sample for it (agent
	// restarted, job just started), so evictDisbalanced never turns it into a placedExperiment.
	// Absence of evidence must not be evidence of violation.
	blocked := disbalanceJob("blocked", "2", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, nil, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: no running job has a proven node placement", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsFailsSafeOnMissingNodeAttributionForTheHogItself(t *testing.T) {
	// Attribution exists, but places the hog on a different node than the one with idle
	// accelerators. It cannot be what is blocking node-a.
	blocked := disbalanceJob("blocked", "2", 1)
	hog := disbalanceJob("hog", "14", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-b"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: the candidate is not on the node with idle accelerators", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsFailsSafeOnMissingClusterTotals(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	hog := disbalanceJob("hog", "14", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)
	placed := []placedExperiment{{experiment: hog, node: "node-a"}}

	// No fresh total-capacity report for this cluster at all: no denominator, no verdict.
	if got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, domain.NewFootprint(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, placed, DefaultDisbalanceTolerance); got != nil {
		t.Fatalf("victims = %v, want none when the cluster reports no totals", victimIDs(got))
	}

	// Cluster totals arrived, but the victim's own node has no fresh total-capacity report — the
	// per-node denominator this pass now measures share against (in place of a cluster-wide
	// average). No node-local totals, no verdict; a missing report must not fall back to the
	// average and invent a number.
	if got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		nil, disbalanceNodes(3), nil, nil, placed, DefaultDisbalanceTolerance); got != nil {
		t.Fatalf("victims = %v, want none when the victim's node reports no totals", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsRefusesPartialPlans(t *testing.T) {
	// The blocked job is short 8 cores, but the only disbalanced job on the node frees 5.
	// Evicting it would destroy work and still leave the blocked job queued.
	blocked := disbalanceJob("blocked", "6", 1)
	hog := disbalanceJob("hog", "5", 1)
	avail := domain.CapacityFootprint(0.0, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)
	avail[cpuKey] = -2000 // 6 cores wanted, 8 short after the running jobs' claims

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: evicting them would not cover the shortage", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsEvictsOnlyAsManyAsNeeded(t *testing.T) {
	// Two hogs on the stranded node; the worse one alone already covers the 1.5-core shortage,
	// so the fill-back pass must reprieve the other.
	blocked := disbalanceJob("blocked", "2", 1)
	worse := disbalanceJob("worse", "14", 1)
	milder := disbalanceJob("milder", "13", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(), disbalanceNodesTotal(), disbalanceNodes(3), nil, nil,
		[]placedExperiment{{experiment: milder, node: "node-a"}, {experiment: worse, node: "node-a"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "worse" {
		t.Fatalf("victims = %v, want only the most disproportionate job", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsHonorsNodeSelector(t *testing.T) {
	// The idle accelerators sit on a node the blocked job is not allowed to use, so they are not
	// idle *for it* and nothing on that node is blocking it.
	blocked := disbalanceJob("blocked", "2", 1)
	blocked.Job.NodeSelector = map[string]string{"vendor.example/zone": "wanted"}
	hog := disbalanceJob("hog", "14", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)
	labels := map[string]map[string]string{"node-a": {"vendor.example/zone": "other"}}

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, labels, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: the idle accelerators fail the job's node selector", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsSparesCPUOnlyNeighbour(t *testing.T) {
	// A job holding no accelerators cannot be measured against a per-accelerator share, and is
	// not what this pass is for.
	blocked := disbalanceJob("blocked", "2", 1)
	cpuOnly := disbalanceJob("cpu-only", "14", 0)
	cpuOnly.AcceleratorType = ""
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: cpuOnly, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: an accelerator-free job has no share to exceed", victimIDs(got))
	}
}

func TestSelectDisbalanceVictimsDetectsMemoryDisbalance(t *testing.T) {
	// Same mechanism on the memory dimension: CPU is proportionate, but 56Gi of a 64Gi node for
	// one of four accelerators is 3.5x the 16Gi share.
	blocked := disbalanceJob("blocked", "2", 1)
	blocked.Job.Memory = "4Gi"
	hog := disbalanceJob("hog", "3", 1)
	hog.Job.Memory = "56Gi"
	avail := domain.CapacityFootprint(8, map[string]int64{disbalanceFlavor: 3}, 1<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the memory-disproportionate job", victimIDs(got))
	}
	if avail[disbalanceMemKey] >= hog.Footprint()[disbalanceMemKey] {
		t.Fatal("test setup no longer exercises a memory shortage")
	}
}

// A job requesting EXACTLY its node-local fair share (4 cores for 1 of 4 chips on a 16-core node)
// must never be flagged, at tolerance=1.0's strict ">" boundary — this is the core guarantee the
// whole "max" feature depends on: a job resolved to its exact fair share must never be evicted
// later for being exactly what it was told to be.
func TestSelectDisbalanceVictimsSparesJobAtExactlyItsFairShare(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	exact := disbalanceJob("exact", "4", 1) // 16 cores / 4 chips = 4 cores/chip exactly
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: exact, node: "node-a"}}, DefaultDisbalanceTolerance)

	if got != nil {
		t.Fatalf("victims = %v, want none: exactly the proportionate share must never be flagged (strict > at the boundary)", victimIDs(got))
	}
}

// The smallest possible overshoot — one canonical unit past the exact fair share — must be
// flagged, proving the boundary sits exactly at ">share", not off-by-one loose in either
// direction.
func TestSelectDisbalanceVictimsEvictsJobOneCanonicalUnitOverFairShare(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	// Fair share is 4000 millicores/chip (16000m / 4 chips); request 4001m — 1 millicore over.
	hog := disbalanceJob("over", "4.001", 1)
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "over" {
		t.Fatalf("victims = %v, want the job that is 1 millicore over its fair share flagged", victimIDs(got))
	}
}

// Two differently-shaped nodes in the same cluster: node-a is CPU-heavy per chip (16 cores / 2
// chips = 8 cores/chip fair share) and node-b is CPU-light per chip (16 cores / 8 chips = 2
// cores/chip). A job requesting 6 cores for 1 chip on node-b is 3x node-b's own fair share — over
// tolerance measured node-locally — but UNDER the cluster-wide average share the old cluster-wide
// math would have computed ((16+16)/(2+8) = 3.2 cores/chip, making 6 cores only ~1.9x). If this
// test used the old cluster-wide-average math it would come out clean; the node-local math must
// catch it.
func TestSelectDisbalanceVictimsUsesNodeLocalShareNotClusterWideAverage(t *testing.T) {
	blocked := disbalanceJob("blocked", "1", 1)
	hog := disbalanceJob("hog", "6", 1) // on node-b: 6 cores for 1 of 8 chips = 3x node-b's 2-core share

	clusterTotal := domain.CapacityFootprint(32, map[string]int64{disbalanceFlavor: 10}, 128<<30, 0) // 16+16 cores, 2+8 chips
	nodesTotal := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 64 << 30}, // 2 chips: 8 cores/chip
		"node-b": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 64 << 30}, // 8 chips: 2 cores/chip
	}
	// Cluster-wide CPU is short (mirrors every other test in this file's shortage pattern), and
	// node-b has idle chips for `blocked` to land on; the hog sits on node-b.
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)
	// node-b has 7 chips free plus the hog's 1 held = 8 installed, matching the "8 chips" node
	// shape the comment above assumes: 16000m / 8 chips = 2000m (2 cores) per chip.
	nodeAvail := map[string]map[string]int64{"node-b": {disbalanceFlavor: 7}}

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, clusterTotal,
		nodesTotal, nodeAvail, nil, nil, []placedExperiment{{experiment: hog, node: "node-b"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the hog flagged against node-b's own 2-core/chip share (3x), which a cluster-wide 3.2-core/chip average would have missed", victimIDs(got))
	}
}

// disbalanceStore records whether the loop reached the database at all.
type disbalanceStore struct {
	LoopStore
	listRunningCalls int
}

func (s *disbalanceStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	s.listRunningCalls++
	return nil, nil
}

func TestEvictDisbalancedFailsSafeOnMissingClusterTotals(t *testing.T) {
	// The cluster is connected but its total-capacity report is stale, so GetTotalCapacity
	// omitted it and the loop hands in an empty footprint. No eviction, and no wasted queries.
	blocked := disbalanceJob("blocked", "2", 1)
	store := &disbalanceStore{}
	loop := &Loop{
		store:               store,
		logger:              zap.NewNop(),
		evictor:             nopEvictor{},
		disbalanceTolerance: DefaultDisbalanceTolerance,
		observed:            fakeObserved{},
	}
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	if err := loop.evictDisbalanced(context.Background(), blocked, "cluster-a", avail, nil, disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, blocked.Footprint()); err != nil {
		t.Fatalf("evictDisbalanced: %v", err)
	}
	if store.listRunningCalls != 0 {
		t.Fatalf("listRunningCalls = %d, want 0 when the cluster has no fresh totals", store.listRunningCalls)
	}
}

func TestEvictDisbalancedSkipsNodeAttributionWhenNothingIsStranded(t *testing.T) {
	// Fully enabled, but every accelerator on the node is busy. The pass must reach that verdict
	// from capacity numbers alone, without querying the store or the metrics DB — it runs for
	// every job skipped in a tick.
	blocked := disbalanceJob("blocked", "2", 1)
	store := &disbalanceStore{}
	loop := &Loop{
		store:               store,
		logger:              zap.NewNop(),
		evictor:             nopEvictor{},
		disbalanceTolerance: DefaultDisbalanceTolerance,
		observed:            fakeObserved{},
	}
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 0}, 32<<30, 0)

	if err := loop.evictDisbalanced(context.Background(), blocked, "cluster-a", avail, disbalanceClusterTotal(), disbalanceNodesTotal(), disbalanceNodes(0), nil, nil, blocked.Footprint()); err != nil {
		t.Fatalf("evictDisbalanced: %v", err)
	}
	if store.listRunningCalls != 0 {
		t.Fatalf("listRunningCalls = %d, want 0 when no accelerator is idle", store.listRunningCalls)
	}
}

type nopEvictor struct{}

func (nopEvictor) EvictExperiment(context.Context, string, domain.EvictionReason) error { return nil }

func victimIDs(victims []disbalanceVictim) []string {
	ids := make([]string, 0, len(victims))
	for _, v := range victims {
		ids = append(ids, v.experiment.ID)
	}
	return ids
}

// groupedDisbalanceJob is a heterogeneous blocked job: a learner and an actor, one chip each,
// with its counts on the groups where a grouped submission requires them and the top-level
// accelerator_count therefore empty.
func groupedDisbalanceJob(id, cpu string) *domain.Experiment {
	return &domain.Experiment{
		ID:               id,
		ClusterName:      "cluster-a",
		Status:           domain.StatusQueued,
		AcceleratorType:  disbalanceFlavor,
		AcceleratorCount: 2,
		Job: domain.JobSpec{
			AcceleratorType: disbalanceFlavor,
			Topology:        &domain.TopologySpec{SpreadAcrossHosts: new(bool)},
			Groups: []domain.JobGroup{
				{Name: "learner", Replicas: 1, CPU: cpu, Memory: "8Gi", AcceleratorCount: 1},
				{Name: "actor", Replicas: 1, CPU: cpu, Memory: "8Gi", AcceleratorCount: 1},
			},
		},
	}
}

// The disbalance pass exists for exactly the job a neighbour's disproportionate request has
// stranded, and a heterogeneous job is the likeliest such victim — its nodes differ, so it is
// hardest to fit around a hog. Keying its entry condition on the top-level accelerator_count,
// which a grouped submission rejects and the store never back-fills, excluded every one of them:
// they were read as CPU-only work with no accelerators to be stranded from, and sat queued while
// the idle devices they needed stayed idle.
func TestAGroupedJobBlockedByAHogIsStillSeenByTheDisbalancePass(t *testing.T) {
	blocked := groupedDisbalanceJob("blocked", "1")
	hog := disbalanceJob("hog", "14", 1) // 14 cores for 1 of 4 chips = 3.5x its 4-core share
	avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
		disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, []placedExperiment{{experiment: hog, node: "node-a"}}, DefaultDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the hog stranding the accelerators a grouped job is waiting for", victimIDs(got))
	}
}
