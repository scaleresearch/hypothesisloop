package scheduler

import (
	"strconv"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A node of 16 cores and 4 chips that runs jobs on cores beside jobs on chips. A job's share of
// the node is counted among the units of its own type, 4 cores a chip and 1 core a core, which is
// what admission bounded its request by.

// coreJob is a RUNNING job on cores, whose cpu is its count.
func coreJob(id string, cores int, memory string) *domain.Experiment {
	exp := disbalanceJob(id, "", cores)
	exp.AcceleratorType, exp.Job.AcceleratorType = cpuType, cpuType
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

func TestSharesOnANodeOfChipsAndCoresAreCountedPerType(t *testing.T) {
	chipJob := func(id, cpu, memory string) *domain.Experiment {
		exp := disbalanceJob(id, cpu, 1)
		if memory != "" {
			exp.Job.Memory = memory
		}
		return exp
	}
	queuedCores := coreJob("blocked", 4, "4Gi")
	queuedCores.Status = domain.StatusQueued
	for name, tc := range map[string]struct {
		blocked              *domain.Experiment
		running              []*domain.Experiment
		freeCores, freeChips int64
		freeMemory           int64
		victim               string
		dimension            domain.ResourceKind
	}{
		// With every type pooled the node had 20 units and a chip job's share was 0.8 cores.
		"a chip job at its 4 cores is spared": {
			chipJob("blocked", "2", ""), []*domain.Experiment{chipJob("fair", "4", ""), coreJob("cores", 11, "8Gi")}, 1, 3, 40 << 30, "", ""},
		"a chip job holding 14 cores is evicted": {
			chipJob("blocked", "2", ""), []*domain.Experiment{chipJob("hog", "14", ""), coreJob("cores", 1, "2Gi")}, 1, 3, 40 << 30, "hog", domain.ResourceKindCPU},
		"a core job is never over its cores": {
			chipJob("blocked", "2", ""), []*domain.Experiment{coreJob("big", 15, "8Gi")}, 1, 4, 56 << 30, "", ""},
		// 4 of 16 cores: a share of 16Gi of the node's 64Gi.
		"a core job holding 40Gi for 4 cores is evicted": {
			chipJob("blocked", "1", "16Gi"), []*domain.Experiment{coreJob("greedy", 4, "40Gi"), coreJob("modest", 4, "16Gi")}, 8, 4, 8 << 30, "greedy", domain.ResourceKindMemory},
		// The cores it lacks are held, and no chip idles for want of them.
		"nobody is evicted for a job on cores": {
			queuedCores, []*domain.Experiment{chipJob("hog", "14", "")}, 2, 3, 56 << 30, "", ""},
	} {
		units := map[string]int64{placement.AcceleratorKey(disbalanceFlavor): tc.freeChips, string(cpuType): tc.freeCores}
		total := domain.CapacityFootprint(disbalanceNodeCores, map[string]int64{disbalanceFlavor: disbalanceNodeChips, string(cpuType): 16}, 64<<30, 0)
		avail := domain.CapacityFootprint(float64(tc.freeCores), units, tc.freeMemory, 0)

		got := selectDisbalanceVictims(tc.blocked, tc.blocked.Footprint(), avail, total,
			disbalanceNodesTotal(), map[string]map[string]int64{"node-a": units}, nil, nil, placedOnNodeA(tc.running...), testDisbalanceTolerance)

		switch {
		case tc.victim == "" && len(got) != 0:
			t.Errorf("%s: victims = %v, want none", name, victimIDs(got))
		case tc.victim != "" && (len(got) != 1 || got[0].experiment.ID != tc.victim || got[0].dimension != tc.dimension):
			t.Errorf("%s: victims = %v, want %s over its %s", name, victimIDs(got), tc.victim, tc.dimension)
		}
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
	nodeAvail := map[string]map[string]int64{"node-a": {placement.AcceleratorKey(disbalanceFlavor): 3, family: 3}}

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
