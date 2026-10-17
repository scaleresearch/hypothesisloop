package placement

import (
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func slotSize(cores, memoryGi, storageGi int64) map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: cores * 1000, domain.NodeResourceMemoryBytes: memoryGi << 30, domain.NodeResourceStorageBytes: storageGi << 30,
	}
}

func slotLabels(name string, devices int) map[string]string {
	labels := map[string]string{domain.NodeSlotLabel: name}
	if devices > 0 {
		labels[domain.NodeSlotAcceleratorsLabel] = strconv.Itoa(devices)
		labels["nvidia.com/gpu.product"] = "NVIDIA-H100-80GB-HBM3"
	}
	return labels
}

func TestSlotFor(t *testing.T) {
	sizes := map[string]map[string]int64{
		"small-gpu": slotSize(8, 32, 100), "large-gpu": slotSize(32, 128, 400), "two-gpu": slotSize(16, 64, 200), "cpu": slotSize(4, 16, 50),
	}
	labels := map[string]map[string]string{
		"small-gpu": slotLabels("small-gpu", 1), "large-gpu": slotLabels("large-gpu", 1), "two-gpu": slotLabels("two-gpu", 2), "cpu": slotLabels("cpu", 0),
	}
	allFree := CloneNodes(sizes)
	smallTaken := CloneNodes(sizes)
	smallTaken["small-gpu"] = map[string]int64{}
	cpuJob := &domain.Experiment{Job: domain.JobSpec{CPU: "2", Memory: "1Gi", Storage: "1Gi"}}
	for name, tc := range map[string]struct {
		exp        *domain.Experiment
		free       map[string]map[string]int64
		wantNode   string
		wantFree   bool
		wantMisfit bool
	}{
		"the smallest slot that holds it":            {holdJob("j", domain.CapacityBurst, h100, 1, "4"), allFree, "small-gpu", true, false},
		"a larger slot when the job needs more":      {holdJob("j", domain.CapacityBurst, h100, 1, "16"), allFree, "large-gpu", true, false},
		"a free slot before a smaller taken one":     {holdJob("j", domain.CapacityBurst, h100, 1, "4"), smallTaken, "large-gpu", true, false},
		"the device count must match":                {holdJob("j", domain.CapacityBurst, h100, 2, "4"), allFree, "two-gpu", true, false},
		"a job without devices takes a slot of none": {cpuJob, allFree, "cpu", true, false},
		"no slot is large enough":                    {holdJob("j", domain.CapacityBurst, h100, 1, "64"), allFree, "", false, true},
		"a job of two nodes takes no slot":           {&domain.Experiment{AcceleratorType: h100, AcceleratorCount: 2, Job: domain.JobSpec{NumNodes: 2, AcceleratorCount: 1, AcceleratorType: h100}}, allFree, "", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			node, isFree, misfit := SlotFor(tc.exp, "c", sizes, tc.free, labels)
			if (misfit != "") != tc.wantMisfit || node != tc.wantNode || isFree != tc.wantFree {
				t.Fatalf("SlotFor = %q, %v, %q, want %q, %v, misfit %v", node, isFree, misfit, tc.wantNode, tc.wantFree, tc.wantMisfit)
			}
			if tc.wantMisfit && !strings.HasPrefix(misfit, domain.NotAdmittedCapacityUnavailable+":") {
				t.Fatalf("misfit %q is not a capacity_unavailable reason", misfit)
			}
		})
	}
}

func TestVacatingSlots(t *testing.T) {
	sizes := map[string]map[string]int64{"a": slotSize(8, 32, 100), "b": slotSize(8, 32, 100), "c": slotSize(8, 32, 100), "d": slotSize(8, 32, 100)}
	labels := map[string]map[string]string{"a": slotLabels("a", 1), "b": slotLabels("b", 1), "c": slotLabels("c", 1), "d": slotLabels("d", 1)}
	labels["d"][domain.NodeSlotNoOfferLabel] = "sold out"
	// a is free, b and c and d are taken; b is held by a claimed job, c and d by nothing.
	free := map[string]map[string]int64{"a": slotSize(8, 32, 100), "b": {}, "c": {}, "d": {}}
	held := &domain.Experiment{ClusterName: "cl", Job: domain.JobSpec{NodeSelector: map[string]string{domain.NodeSlotLabel: "b"}}}
	elsewhere := &domain.Experiment{ClusterName: "other", Job: domain.JobSpec{NodeSelector: map[string]string{domain.NodeSlotLabel: "c"}}}

	got := VacatingSlots("cl", sizes, free, labels, []*domain.Experiment{held, elsewhere})

	if len(got) != 1 || got["c"] == nil {
		t.Fatalf("VacatingSlots = %v, want slot c alone: a is free, b is held, d has no offer", got)
	}
}

func TestHoldSlotTakesEveryNodeOfTheSlot(t *testing.T) {
	byNode := map[string]map[string]int64{"a": {"h100": 1}, "b": {"h100": 1}}
	resources := map[string]map[string]int64{"a": slotSize(8, 32, 100), "b": slotSize(8, 32, 100)}
	labels := map[string]map[string]string{"a": slotLabels("a", 1), "b": slotLabels("b", 1)}

	HoldSlot(byNode, resources, labels, "a")

	if byNode["a"]["h100"] != 0 || resources["a"][domain.NodeResourceCPUMillicores] != 0 {
		t.Fatalf("slot a still reads free: %v %v", byNode["a"], resources["a"])
	}
	if byNode["b"]["h100"] != 1 || resources["b"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatalf("slot b was taken too: %v %v", byNode["b"], resources["b"])
	}
}

func TestSlotAcceleratorsAndHasSlots(t *testing.T) {
	if got := SlotAccelerators(slotLabels("a", 4)); got != 4 {
		t.Errorf("SlotAccelerators = %d, want 4", got)
	}
	if got := SlotAccelerators(map[string]string{domain.NodeSlotAcceleratorsLabel: "many"}); got != 0 {
		t.Errorf("SlotAccelerators of an unreadable label = %d, want 0", got)
	}
	if HasSlots(map[string]map[string]string{"n": {"zone": "a"}}) {
		t.Error("HasSlots answered true for a cluster of ordinary nodes")
	}
	if !HasSlots(map[string]map[string]string{"n": {"zone": "a"}, "s": slotLabels("s", 0)}) {
		t.Error("HasSlots answered false for a cluster with a slot")
	}
}
