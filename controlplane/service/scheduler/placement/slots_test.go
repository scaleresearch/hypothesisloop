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

func TestSlotsFor(t *testing.T) {
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
		"a job of more nodes than a shape has slots": {&domain.Experiment{AcceleratorType: h100, AcceleratorCount: 2, Job: domain.JobSpec{NumNodes: 2, CPU: "16", AcceleratorCount: 1, AcceleratorType: h100}}, allFree, "", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			nodes, isFree, misfit := SlotsFor(tc.exp, "c", sizes, tc.free, labels)
			if (misfit != "") != tc.wantMisfit || strings.Join(nodes, ",") != tc.wantNode || isFree != tc.wantFree {
				t.Fatalf("SlotsFor = %q, %v, %q, want %q, %v, misfit %v", nodes, isFree, misfit, tc.wantNode, tc.wantFree, tc.wantMisfit)
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
	held := &domain.Experiment{ClusterName: "cl", ResolvedJob: &domain.JobSpec{Slots: []string{"b"}}}
	elsewhere := &domain.Experiment{ClusterName: "other", ResolvedJob: &domain.JobSpec{Slots: []string{"c"}}}

	got := VacatingSlots("cl", sizes, free, labels, []*domain.Experiment{held, elsewhere})

	if len(got) != 1 || got["c"] == nil {
		t.Fatalf("VacatingSlots = %v, want slot c alone: a is free, b is held, d has no offer", got)
	}
}

func TestHoldSlotsTakesEverySlotOfTheJob(t *testing.T) {
	byNode := map[string]map[string]int64{"a": {"h100": 1}, "b": {"h100": 1}, "c": {"h100": 1}}
	resources := map[string]map[string]int64{"a": slotSize(8, 32, 100), "b": slotSize(8, 32, 100), "c": slotSize(8, 32, 100)}
	labels := map[string]map[string]string{"a": slotLabels("a", 1), "b": slotLabels("b", 1), "c": slotLabels("c", 1)}

	HoldSlots(byNode, resources, labels, []string{"a", "c"})

	for _, node := range []string{"a", "c"} {
		if byNode[node]["h100"] != 0 || resources[node][domain.NodeResourceCPUMillicores] != 0 {
			t.Fatalf("slot %s still reads free: %v %v", node, byNode[node], resources[node])
		}
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

// networked is a free slot whose shape one private network holds count nodes of now, and why it
// holds none.
func networked(labels map[string]string, count int, why string) map[string]string {
	labels[domain.NodeSlotNetworkLabel] = strconv.Itoa(count)
	if why != "" {
		labels[domain.NodeSlotNoNetworkLabel] = why
	}
	return labels
}

func h100Gang(nodes int, cpu string) *domain.Experiment {
	return &domain.Experiment{AcceleratorType: h100, AcceleratorCount: nodes,
		Job: domain.JobSpec{NumNodes: nodes, CPU: cpu, Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1, AcceleratorType: h100}}
}

// A job that spans nodes holds one slot per node, in rank order, all of the smallest shape that
// has a free slot for each, and those only while a private network holds every one of them.
func TestSlotsForAJobThatSpansNodes(t *testing.T) {
	sizes := map[string]map[string]int64{
		"small-0": slotSize(8, 32, 100), "small-1": slotSize(8, 32, 100), "small-2": slotSize(8, 32, 100),
		"large-0": slotSize(32, 128, 400), "large-1": slotSize(32, 128, 400),
	}
	labelsWith := func(small, large int) map[string]map[string]string {
		labels := map[string]map[string]string{}
		for node := range sizes {
			count := small
			if strings.HasPrefix(node, "large") {
				count = large
			}
			labels[node] = networked(slotLabels(node, 1), count, "")
		}
		return labels
	}
	allFree := CloneNodes(sizes)
	oneSmallTaken := CloneNodes(sizes)
	oneSmallTaken["small-0"] = map[string]int64{}
	for name, tc := range map[string]struct {
		exp      *domain.Experiment
		free     map[string]map[string]int64
		labels   map[string]map[string]string
		want     string
		wantFree bool
	}{
		"the smallest shape, its slots by name": {h100Gang(2, "4"), allFree, labelsWith(3, 2), "small-0,small-1", true},
		"free slots of the shape first":         {h100Gang(2, "4"), oneSmallTaken, labelsWith(3, 2), "small-1,small-2", true},
		"a larger shape when the smaller has too few": {h100Gang(2, "4"), func() map[string]map[string]int64 {
			f := CloneNodes(oneSmallTaken)
			f["small-1"] = map[string]int64{}
			return f
		}(), labelsWith(3, 2), "large-0,large-1", true},
		"a larger shape when no network holds the job":  {h100Gang(2, "4"), allFree, labelsWith(1, 2), "large-0,large-1", true},
		"the smallest shape to wait on when none holds": {h100Gang(2, "4"), allFree, labelsWith(1, 1), "small-0,small-1", false},
		"a network for every node of the job":           {h100Gang(3, "4"), allFree, labelsWith(2, 2), "small-0,small-1,small-2", false},
	} {
		t.Run(name, func(t *testing.T) {
			nodes, isFree, misfit := SlotsFor(tc.exp, "c", sizes, tc.free, tc.labels)
			if misfit != "" || strings.Join(nodes, ",") != tc.want || isFree != tc.wantFree {
				t.Fatalf("SlotsFor = %q, %v, %q, want %q, %v", nodes, isFree, misfit, tc.want, tc.wantFree)
			}
		})
	}
	if _, _, misfit := SlotsFor(h100Gang(4, "4"), "c", sizes, allFree, labelsWith(4, 4)); !strings.Contains(misfit, "has 4 slots that take the job's nodes") {
		t.Fatalf("misfit %q, want no shape with four slots", misfit)
	}
}

// Every group of a job that spans nodes holds slots of one shape, the smallest that takes every
// group, as one private network holds one shape. Each group is sized to it, so the job holds every
// slot. A job whose groups no one shape takes is refused, naming what each group takes.
func TestEveryGroupOfAJobThatSpansNodesHoldsSlotsOfOneShape(t *testing.T) {
	sizes := map[string]map[string]int64{
		"gpu-0": slotSize(8, 32, 100), "gpu-1": slotSize(8, 32, 100),
		"cpu-0": slotSize(4, 16, 50), "cpu-1": slotSize(4, 16, 50), "cpu-2": slotSize(4, 16, 50),
		"big-0": slotSize(8, 32, 100), "big-1": slotSize(8, 32, 100), "big-2": slotSize(8, 32, 100),
	}
	labels := map[string]map[string]string{}
	for node := range sizes {
		devices := 0
		if strings.HasPrefix(node, "gpu") {
			devices = 1
		}
		labels[node] = networked(slotLabels(node, devices), 3, "")
	}
	exp := &domain.Experiment{AcceleratorType: domain.CPUType, AcceleratorCount: 2, Job: domain.JobSpec{AcceleratorType: domain.CPUType, Groups: []domain.JobGroup{
		{Name: "actor", Replicas: 2, AcceleratorCount: 2, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
		{Name: "critic", Replicas: 1, AcceleratorCount: 6, CPU: "6", Memory: "1Gi", Storage: "1Gi"},
	}}}

	nodes, isFree, misfit := SlotsFor(exp, "c", sizes, sizes, labels)
	if misfit != "" || strings.Join(nodes, ",") != "big-0,big-1,big-2" || !isFree {
		t.Fatalf("SlotsFor = %q, %v, %q, want every group on the one shape that takes the critic", nodes, isFree, misfit)
	}
	labels["big-2"][domain.NodeSlotNetworkLabel] = "2"
	if _, isFree, _ := SlotsFor(exp, "c", sizes, sizes, labels); isFree {
		t.Fatal("three nodes on big slots were free on a slot whose network holds two")
	}
	labels["big-2"][domain.NodeSlotNetworkLabel] = "3"

	resolved, misfit := ResolveOntoSlots(exp, "c", sizes, sizes, labels)
	if misfit != "" {
		t.Fatal(misfit)
	}
	for i := range resolved.Groups {
		if g := resolved.Groups[i]; g.CPU != "8" {
			t.Errorf("group %s sized to %s cpu, want 8", g.Name, g.CPU)
		}
	}
	sized := *exp
	sized.ResolvedJob = resolved
	if got, want := sized.Footprint()[domain.ResourceKey{Kind: domain.ResourceKindCPU}], int64(8000*3); got != want {
		t.Errorf("footprint cpu %d, want the three slots' %d", got, want)
	}
	if exp.Job.Groups[0].CPU != "2" {
		t.Error("resolving rewrote the submitted job")
	}

	mixed := &domain.Experiment{AcceleratorType: h100, AcceleratorCount: 1, Job: domain.JobSpec{AcceleratorType: h100, Groups: []domain.JobGroup{
		{Name: "actor", Replicas: 2, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
		{Name: "learner", Replicas: 1, AcceleratorCount: 1, CPU: "4", Memory: "1Gi", Storage: "1Gi"},
	}}}
	_, _, misfit = SlotsFor(mixed, "c", sizes, sizes, labels)
	for _, want := range []string{domain.NotAdmittedCapacityUnavailable, "one private network", `group "actor" of the job takes 4 cpu`, `group "learner" of the job takes 1 of ` + string(h100)} {
		if !strings.Contains(misfit, want) {
			t.Errorf("misfit %q, want it to say %q", misfit, want)
		}
	}
}

// A job on CPU that spans nodes holds every core of each of its slots, as its count per node.
func TestAJobOnCPUThatSpansNodesIsSizedToItsSlotsCores(t *testing.T) {
	sizes := map[string]map[string]int64{"cpu-0": slotSize(4, 16, 50), "cpu-1": slotSize(4, 16, 50)}
	labels := map[string]map[string]string{"cpu-0": networked(slotLabels("cpu-0", 0), 2, ""), "cpu-1": networked(slotLabels("cpu-1", 0), 2, "")}
	exp := &domain.Experiment{AcceleratorType: domain.CPUType, AcceleratorCount: 4,
		Job: domain.JobSpec{NumNodes: 2, CPU: "2", Memory: "1Gi", Storage: "1Gi", AcceleratorType: domain.CPUType, AcceleratorCount: 2}}
	resolved, misfit := ResolveOntoSlots(exp, "c", sizes, sizes, labels)
	if misfit != "" || resolved.CPU != "4" || resolved.AcceleratorCount != 4 || resolved.NumNodes != 2 {
		t.Fatalf("resolved %+v, %q, want each node at the slot's 4 cores", resolved, misfit)
	}
}

// A job that spans nodes waits on a private network when its slots are free and no network holds
// them, with the runtime's reason; otherwise it does not wait on one.
func TestNetworkWait(t *testing.T) {
	sizes := map[string]map[string]int64{"a": slotSize(8, 32, 100), "b": slotSize(8, 32, 100)}
	labels := map[string]map[string]string{
		"a": networked(slotLabels("a", 1), 0, "no cluster has 2 machines"),
		"b": networked(slotLabels("b", 1), 0, "no cluster has 2 machines"),
	}
	ranks, shape, why, ok := NetworkWait(h100Gang(2, "4"), "c", sizes, sizes, labels)
	if !ok || ranks != 2 || shape != "1 of "+h100+", 8 cpu, 32Gi memory, 100Gi storage" || len(why) != 1 || why[0] != "no cluster has 2 machines" {
		t.Fatalf("NetworkWait = %d, %q, %q, %v", ranks, shape, why, ok)
	}
	taken := CloneNodes(sizes)
	taken["b"] = map[string]int64{}
	if _, _, _, ok := NetworkWait(h100Gang(2, "4"), "c", sizes, taken, labels); ok {
		t.Error("a job waiting for a taken slot waits on a network")
	}
	if _, _, _, ok := NetworkWait(h100Gang(1, "4"), "c", sizes, sizes, labels); ok {
		t.Error("a job on one node waits on a network")
	}
	labels["a"][domain.NodeSlotNetworkLabel], labels["b"][domain.NodeSlotNetworkLabel] = "2", "2"
	if _, _, _, ok := NetworkWait(h100Gang(2, "4"), "c", sizes, sizes, labels); ok {
		t.Error("a job a network holds waits on one")
	}
}
