package scheduler

import (
	"strconv"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A node that publishes a CPU class beside its chips: 16 cores, 4 chips and 16 core units, one
// per core. A job's share of the node is counted among the units of its own flavor, 4 cores a
// chip and 1 core a core unit, which is what admission bounded its request by.

func mixedClusterTotal() domain.Footprint {
	return domain.CapacityFootprint(disbalanceNodeCores, map[string]int64{disbalanceFlavor: disbalanceNodeChips, cpuClass: 16}, 64<<30, 0)
}

// mixedNode is node-a with freeChips chips and freeCores core units free.
func mixedNode(freeChips, freeCores int64) map[string]map[string]int64 {
	return map[string]map[string]int64{"node-a": {disbalanceFlavor: freeChips, cpuClass: freeCores}}
}

// coreJob is a RUNNING job on cores of the CPU class, whose cpu is its count.
func coreJob(id string, cores int, memory string) *domain.Experiment {
	exp := disbalanceJob(id, "", cores)
	exp.AcceleratorType, exp.Job.AcceleratorType = cpuClass, cpuClass
	exp.Job.CPU, exp.Job.Memory = strconv.Itoa(cores), memory
	return exp
}

func placedOnNodeA(jobs ...*domain.Experiment) []placedExperiment {
	placed := make([]placedExperiment, 0, len(jobs))
	for _, job := range jobs {
		placed = append(placed, placedExperiment{experiment: job, node: "node-a"})
	}
	return placed
}

// With every flavor pooled the node had 20 devices and a chip job's share was 0.8 cores, so a
// chip job holding the 4 cores it was admitted with read as five times over and was evicted.
func TestAChipJobAtItsShareIsSparedOnANodeThatAlsoPublishesACPUClass(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	fair := disbalanceJob("fair", "4", 1)
	cores := coreJob("cores", 11, "8Gi")
	// 15 of 16 cores are held; three chips and five core units are free.
	avail := domain.CapacityFootprint(1, map[string]int64{disbalanceFlavor: 3, cpuClass: 5}, 40<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, mixedClusterTotal(),
		disbalanceNodesTotal(), mixedNode(3, 5), nil, nil, placedOnNodeA(fair, cores), testDisbalanceTolerance)

	if len(got) != 0 {
		t.Fatalf("victims = %v, want none: one chip of four holds 4 of 16 cores and a core job holds one core a unit", victimIDs(got))
	}
}

func TestAChipHogIsStillEvictedOnANodeThatAlsoPublishesACPUClass(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	hog := disbalanceJob("hog", "14", 1)
	cores := coreJob("cores", 1, "2Gi")
	avail := domain.CapacityFootprint(1, map[string]int64{disbalanceFlavor: 3, cpuClass: 15}, 40<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, mixedClusterTotal(),
		disbalanceNodesTotal(), mixedNode(3, 15), nil, nil, placedOnNodeA(hog, cores), testDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the chip job holding 14 cores for one chip of four", victimIDs(got))
	}
	if got[0].share != 4000 || got[0].requested != 14000 {
		t.Errorf("evidence: requested %d of a share of %v, want 14000 of 4000 millicores", got[0].requested, got[0].share)
	}
}

// A job on cores holds exactly its share of the cpu by the rule that makes its count its cores,
// so it is never the disproportionate one in that dimension, however many cores it holds.
func TestACoreJobIsNeverEvictedForTheCoresItsCountGaveIt(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	big := coreJob("big", 15, "8Gi")
	avail := domain.CapacityFootprint(1, map[string]int64{disbalanceFlavor: 4, cpuClass: 1}, 56<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, mixedClusterTotal(),
		disbalanceNodesTotal(), mixedNode(4, 1), nil, nil, placedOnNodeA(big), testDisbalanceTolerance)

	if len(got) != 0 {
		t.Fatalf("victims = %v, want none: 15 core units hold 15 cores", victimIDs(got))
	}
}

// The pass serves a job on cores as it serves one on chips: free core units it cannot reach
// because a chip job holds far more cores than its chips entitle it to.
func TestACoreJobBlockedByAChipHogHasTheHogEvicted(t *testing.T) {
	blocked := coreJob("blocked", 4, "4Gi")
	blocked.Status = domain.StatusQueued
	hog := disbalanceJob("hog", "14", 1)
	avail := domain.CapacityFootprint(2, map[string]int64{disbalanceFlavor: 3, cpuClass: 16}, 56<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, mixedClusterTotal(),
		disbalanceNodesTotal(), mixedNode(3, 16), nil, nil, placedOnNodeA(hog), testDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "hog" {
		t.Fatalf("victims = %v, want the chip hog that leaves 2 cores beside 16 free core units", victimIDs(got))
	}
}

// Memory is shared the same way: a core job that named more than its part of the node's memory
// is over its share, measured among the core units and not among units and chips together.
func TestACoreJobOverItsMemoryShareIsMeasuredAmongTheCoreUnits(t *testing.T) {
	blocked := disbalanceJob("blocked", "1", 1)
	blocked.Job.Memory = "16Gi"
	// 4 of 16 core units: a share of 16Gi of the node's 64Gi. It holds 40Gi.
	greedy := coreJob("greedy", 4, "40Gi")
	modest := coreJob("modest", 4, "16Gi")
	avail := domain.CapacityFootprint(8, map[string]int64{disbalanceFlavor: 4, cpuClass: 8}, 8<<30, 0)

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, mixedClusterTotal(),
		disbalanceNodesTotal(), mixedNode(4, 8), nil, nil, placedOnNodeA(greedy, modest), testDisbalanceTolerance)

	if len(got) != 1 || got[0].experiment.ID != "greedy" || got[0].dimension != domain.ResourceKindMemory {
		t.Fatalf("victims = %v, want only the core job holding 40Gi for a 16Gi share", victimIDs(got))
	}
}

// A runtime lists one device under every label of its vendor's domain. Summed over flavors, four
// chips under two labels were eight and a job at its share read as twice over.
func TestADeviceListedUnderTwoLabelsIsCountedOnce(t *testing.T) {
	const family = "tenstorrent.com/boardType=7"
	blocked := disbalanceJob("blocked", "2", 1)
	fair := disbalanceJob("fair", "4", 1)
	total := domain.CapacityFootprint(disbalanceNodeCores, map[string]int64{disbalanceFlavor: disbalanceNodeChips, family: disbalanceNodeChips}, 64<<30, 0)
	avail := domain.CapacityFootprint(1, map[string]int64{disbalanceFlavor: 3, family: 3}, 40<<30, 0)
	nodeAvail := map[string]map[string]int64{"node-a": {disbalanceFlavor: 3, family: 3}}

	got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, total,
		disbalanceNodesTotal(), nodeAvail, nil, nil, placedOnNodeA(fair), testDisbalanceTolerance)

	if len(got) != 0 {
		t.Fatalf("victims = %v, want none: the node has four chips, whatever number of labels names them", victimIDs(got))
	}
}

// One flavor on a node: the count per flavor is the count of the node, and every decision of the
// pass is what it was.
func TestOnANodeOfOneFlavorTheShareIsWhatItWas(t *testing.T) {
	blocked := disbalanceJob("blocked", "2", 1)
	for cpu, evicted := range map[string]bool{"4": false, "4001m": true, "14": true, "1": false} {
		job := disbalanceJob("running", cpu, 1)
		avail := domain.CapacityFootprint(0.5, map[string]int64{disbalanceFlavor: 3}, 32<<30, 0)
		got := selectDisbalanceVictims(blocked, blocked.Footprint(), avail, disbalanceClusterTotal(),
			disbalanceNodesTotal(), disbalanceNodes(3), nil, nil, placedOnNodeA(job), testDisbalanceTolerance)
		if (len(got) == 1) != evicted {
			t.Errorf("a chip job holding %s cores of a 4-core share: victims = %v, want evicted = %v", cpu, victimIDs(got), evicted)
		}
	}
}
