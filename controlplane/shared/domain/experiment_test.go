package domain

import "testing"

// Regression test for findings.md's P0 "extended resources bypass admission accounting":
// JobSpec.Footprint (used by Job/JobSpec-facing code) includes every ExtraResources entry as
// ResourceKindExtended, but Experiment.Footprint (what tick/preempt/submitJob actually use for
// admission) rebuilds the footprint from scratch and never copies Job.ExtraResources. A job
// requesting a TPU/Gaudi/Neuron-style extended resource therefore passes control-plane
// admission with zero accounting for that dimension. Expected to FAIL until
// Experiment.Footprint is fixed to include ExtraResources like JobSpec.Footprint already does.
func TestExperimentFootprintIncludesExtraResources(t *testing.T) {
	e := &Experiment{
		Job: JobSpec{
			CPU:            "500m",
			NumNodes:       1,
			ExtraResources: map[string]string{"google.com/tpu": "8"},
		},
	}
	fp := e.Footprint()
	key := ResourceKey{Kind: ResourceKindExtended, Flavor: "google.com/tpu"}
	if got := fp[key]; got != 8 {
		t.Errorf("Experiment.Footprint()[extended:google.com/tpu] = %d, want 8 (extended resources not carried through admission accounting)", got)
	}
}

// G1 (all num_nodes nodes are admitted together or none is) and G7 (billing counts N x the
// per-node footprint on every dimension) both rest on one arithmetic fact: nothing about a
// distributed job's cost is per-node except the shape itself. A dimension that forgot to scale
// would let a 3-node job be admitted against one node's worth of capacity, which is a gang that
// half-fits — so this asserts every dimension at once rather than sampling one.
func TestJobSpecFootprintScalesEveryDimensionByNodeCount(t *testing.T) {
	spec := JobSpec{
		CPU: "4", Memory: "8Gi", Storage: "10Gi",
		AcceleratorCount: 2, NumNodes: 3,
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		ExtraResources:  map[string]string{"google.com/tpu": "8"},
	}

	fp, err := spec.Footprint(spec.AcceleratorType)
	if err != nil {
		t.Fatal(err)
	}
	want := map[ResourceKey]int64{
		{Kind: ResourceKindCPU}:     12000,            // 4 cores x 3
		{Kind: ResourceKindMemory}:  3 * 8 * 1 << 30,  // 8Gi x 3
		{Kind: ResourceKindStorage}: 3 * 10 * 1 << 30, // 10Gi x 3
		{Kind: ResourceKindAccelerator, Flavor: "nvidia.com/gpu.product=nvidia-h100-80gb-hbm3"}: 6,  // 2 x 3
		{Kind: ResourceKindExtended, Flavor: "google.com/tpu"}:                                  24, // 8 x 3
	}
	for key, wantAmount := range want {
		if got := fp[key]; got != wantAmount {
			t.Errorf("Footprint()[%v] = %d, want %d — dimension not scaled by num_nodes", key, got, wantAmount)
		}
	}

	if got := spec.TotalAccelerators(); got != 6 {
		t.Errorf("TotalAccelerators() = %d, want 6 (2 per node x 3 nodes)", got)
	}
	if got := spec.Nodes(); got != 3 {
		t.Errorf("Nodes() = %d, want 3", got)
	}
}

// The counterpart of TestJobSpecFootprintScalesEveryDimensionByNodeCount for a heterogeneous
// job: where an ungrouped job's footprint is one shape multiplied, a grouped job's is its groups
// summed. This is the whole economic point of groups. Expressed as one job at num_nodes 65, the
// learner's shape would be charged 65 times and the job would pay 520 accelerators for the 8 it
// uses; expressed as two jobs the arithmetic is right but they are two experiments, admitted,
// evicted and billed separately. Every dimension is asserted at once, because a dimension that
// summed the groups while another multiplied one of them is exactly the half-fitting gang the
// ungrouped test exists to prevent.
func TestGroupedJobSpecFootprintSumsTheGroupsInsteadOfMultiplyingOne(t *testing.T) {
	spec := JobSpec{
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		Groups: []JobGroup{
			{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
			{Name: "actor", Replicas: 64, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
		},
	}

	fp, err := spec.Footprint(spec.AcceleratorType)
	if err != nil {
		t.Fatal(err)
	}
	want := map[ResourceKey]int64{
		{Kind: ResourceKindCPU}:     80000,               // 16 + 64 x 1 cores
		{Kind: ResourceKindMemory}:  384 * 1 << 30,       // 128Gi + 64 x 4Gi
		{Kind: ResourceKindStorage}: (10 + 64) * 1 << 30, // 10Gi + 64 x 1Gi
		{Kind: ResourceKindAccelerator, Flavor: "nvidia.com/gpu.product=nvidia-h100-80gb-hbm3"}: 8, // 8 + 64 x 0
	}
	for key, wantAmount := range want {
		if got := fp[key]; got != wantAmount {
			t.Fatalf("Footprint()[%v] = %d, want %d — a grouped job's demand is the sum of its groups, never a multiple of one of them", key, got, wantAmount)
		}
	}

	if got := spec.TotalAccelerators(); got != 8 {
		t.Fatalf("TotalAccelerators() = %d, want 8 (8 for the learner, 0 for each of the 64 actors) — anything else bills hardware nobody asked for", got)
	}
	if got := spec.Nodes(); got != 65 {
		t.Fatalf("Nodes() = %d, want 65 (1 learner + 64 actors) — the node count is what admission proves it has room for", got)
	}
}

// Experiment.Footprint is a second, independent implementation of the same arithmetic (it reads
// the substituted accelerator type off the experiment rather than the job), and it is the one
// admission, preemption and eviction actually call. The two agreeing is not automatic, so the
// experiment-level view of a grouped job is asserted on its own: if it kept multiplying by the
// node count, a 65-node job would be admitted against 65 learners' worth of CPU and memory.
func TestGroupedExperimentFootprintSumsTheGroupsLikeTheJobSpecDoes(t *testing.T) {
	exp := &Experiment{
		AcceleratorType:  "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		AcceleratorCount: 8,
		Job: JobSpec{
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
			Groups: []JobGroup{
				{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
				{Name: "actor", Replicas: 64, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
			},
		},
	}

	fp := exp.Footprint()
	if got := fp[ResourceKey{Kind: ResourceKindCPU}]; got != 80000 {
		t.Fatalf("Experiment.Footprint()[cpu] = %d millicores, want 80000 — the admission path must see the same 80 cores JobSpec.Footprint reports, not 65 learners", got)
	}
	if got := fp[ResourceKey{Kind: ResourceKindMemory}]; got != 384*1<<30 {
		t.Fatalf("Experiment.Footprint()[memory] = %d bytes, want %d (384Gi)", got, int64(384)*1<<30)
	}
	if got := fp[ResourceKey{Kind: ResourceKindAccelerator, Flavor: "nvidia.com/gpu.product=nvidia-h100-80gb-hbm3"}]; got != 8 {
		t.Fatalf("Experiment.Footprint()[accelerator] = %d, want 8 — the accelerator dimension is the experiment's own total, and a grouped job's total is the sum of its groups", got)
	}
}

// One way to say a thing. num_nodes replicates the top-level shape and groups state their own,
// so a spec carrying both is asking two incompatible questions: does num_nodes multiply the
// groups, or do the groups replace it? Every answer is a merge rule someone has to remember, and
// a silent choice here is a job that runs at a size nobody wrote down.
func TestSpecSettingBothGroupsAndNumNodesIsRejected(t *testing.T) {
	spec := JobSpec{
		NumNodes: 4,
		Groups:   []JobGroup{{Name: "worker", Replicas: 2, CPU: "1", Memory: "1Gi", Storage: "1Gi"}},
	}
	if err := spec.ValidateGroups(); err == nil {
		t.Fatalf("ValidateGroups() accepted a spec with both num_nodes and groups — a job whose node count has two contradictory sources runs at a size the submitter never stated")
	}
}

// The same rule for the top-level per-node resource fields: a grouped job states cpu/memory/
// storage/accelerator_count per group, so a top-level one has no meaning that isn't invented.
func TestSpecSettingBothGroupsAndTopLevelResourcesIsRejected(t *testing.T) {
	spec := JobSpec{
		CPU:    "8",
		Groups: []JobGroup{{Name: "worker", Replicas: 2, CPU: "1", Memory: "1Gi", Storage: "1Gi"}},
	}
	if err := spec.ValidateGroups(); err == nil {
		t.Fatalf("ValidateGroups() accepted a grouped spec that also sets job.cpu — there would be no rule saying which of the two a worker actually gets")
	}
}

// One accelerator type per job. The experiment carries a single AcceleratorType that admission
// filters on, flavor substitution rewrites and billing prices against; two groups naming
// different types would make that one field a lie, and the job would be billed entirely at
// whichever type happened to win.
func TestTwoGroupsNamingDifferentAcceleratorTypesAreRejected(t *testing.T) {
	spec := JobSpec{
		Groups: []JobGroup{
			{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi",
				AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
			{Name: "actor", Replicas: 4, AcceleratorCount: 1, CPU: "1", Memory: "4Gi", Storage: "1Gi",
				AcceleratorType: "nvidia.com/gpu.product=NVIDIA-A100-SXM4-40GB"},
		},
	}
	if err := spec.ValidateGroups(); err == nil {
		t.Fatalf("ValidateGroups() accepted groups on two different accelerator types — the experiment has one AcceleratorType, so one of the two groups would be billed at the other's rate")
	}
}

// Groups differing in accelerator COUNT, including zero, is the entire point and must stay
// legal — the rejection above is about types, and a rule that over-reached to counts would ban
// the learner-plus-CPU-actors shape the feature exists for.
func TestGroupsMayDifferInAcceleratorCountIncludingZero(t *testing.T) {
	spec := JobSpec{
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		Groups: []JobGroup{
			{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
			{Name: "actor", Replicas: 64, AcceleratorCount: 0, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
		},
	}
	if err := spec.ValidateGroups(); err != nil {
		t.Fatalf("ValidateGroups() rejected a learner alongside CPU-only actors: %v — that is the shape heterogeneous groups exist to express", err)
	}
}

// The backward-compatibility claim, asserted rather than assumed: an ungrouped spec must behave
// exactly as it did before groups existed. NodeGroups() is the single definition every derived
// field and both backends now read a job through, so if it reports anything other than "one group
// of NumNodes replicas carrying the top-level shape", every existing job changes size, cost or
// command at once.
func TestUngroupedSpecIsOneGroupOfIdenticalNodesCarryingTheTopLevelShape(t *testing.T) {
	spec := JobSpec{
		CPU: "4", Memory: "8Gi", Storage: "10Gi",
		AcceleratorCount: 2, NumNodes: 3,
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		Command:         []string{"python", "train.py"},
		Args:            []string{"--epochs", "3"},
		Env:             map[string]string{"SEED": "7"},
	}

	groups := spec.NodeGroups()
	if len(groups) != 1 {
		t.Fatalf("NodeGroups() returned %d groups for an ungrouped spec, want exactly 1 — every backend compiles one native workload per group, so more than one would split an existing job in two", len(groups))
	}
	group := groups[0]
	if group.Name != "" {
		t.Fatalf("NodeGroups()[0].Name = %q, want \"\" — a name would suffix the Job/container name of every job that already exists and orphan its running workload", group.Name)
	}
	if group.Replicas != 3 {
		t.Fatalf("NodeGroups()[0].Replicas = %d, want 3 (num_nodes)", group.Replicas)
	}
	if group.CPU != "4" || group.Memory != "8Gi" || group.Storage != "10Gi" || group.AcceleratorCount != 2 {
		t.Fatalf("NodeGroups()[0] shape = %s/%s/%s/%d accelerators, want 4/8Gi/10Gi/2 — the synthetic group must be the top-level per-node shape verbatim", group.CPU, group.Memory, group.Storage, group.AcceleratorCount)
	}
	if len(group.Command) != 2 || group.Command[0] != "python" || len(group.Args) != 2 || group.Env["SEED"] != "7" {
		t.Fatalf("NodeGroups()[0] carries command %v args %v env %v, want the job's own — an ungrouped job has no other place for them", group.Command, group.Args, group.Env)
	}
	if err := spec.ValidateGroups(); err != nil {
		t.Fatalf("ValidateGroups() rejected an ungrouped spec: %v — the group rules must not apply to a job that declared no groups", err)
	}
	if got := spec.Nodes(); got != 3 {
		t.Fatalf("Nodes() = %d, want 3", got)
	}
	if got := spec.TotalAccelerators(); got != 6 {
		t.Fatalf("TotalAccelerators() = %d, want 6 (2 per node x 3 nodes)", got)
	}
}

// NodeShapes is the placement view admission walks, and for an ungrouped job it must be N copies
// of one shape. An averaged or collapsed shape here would let a job be proven to fit against a
// node that holds a fraction of what it asked for.
func TestUngroupedSpecExpandsToOneIdenticalNodeShapePerNode(t *testing.T) {
	spec := JobSpec{CPU: "4", Memory: "8Gi", Storage: "10Gi", AcceleratorCount: 2, NumNodes: 3}

	shapes := spec.NodeShapes()
	if len(shapes) != 3 {
		t.Fatalf("NodeShapes() returned %d shapes, want 3 — one per node, since admission proves each node individually has somewhere to land", len(shapes))
	}
	for i, shape := range shapes {
		if shape.AcceleratorCount != 2 {
			t.Fatalf("NodeShapes()[%d].AcceleratorCount = %d, want 2 (the PER-NODE count, never the job total)", i, shape.AcceleratorCount)
		}
		if shape.Resources[NodeResourceCPUMillicores] != 4000 || shape.Resources[NodeResourceMemoryBytes] != 8*1<<30 {
			t.Fatalf("NodeShapes()[%d] resources = %v, want 4000 millicores and 8Gi — every node of an ungrouped job is identical by construction", i, shape.Resources)
		}
	}
}

// A grouped job's shapes must stay distinct, in group order. This is the invariant that keeps
// admission honest: no node ever holds the average of a learner and 64 actors, so a job proven to
// fit against an averaged shape fits nowhere and sits unschedulable holding a reservation.
func TestGroupedSpecExpandsToOneNodeShapePerReplicaOfEachGroup(t *testing.T) {
	spec := JobSpec{
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		Groups: []JobGroup{
			{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi"},
			{Name: "actor", Replicas: 2, CPU: "1", Memory: "4Gi", Storage: "1Gi"},
		},
	}

	shapes := spec.NodeShapes()
	if len(shapes) != 3 {
		t.Fatalf("NodeShapes() returned %d shapes, want 3 (1 learner + 2 actors)", len(shapes))
	}
	if shapes[0].Group != "learner" || shapes[0].AcceleratorCount != 8 || shapes[0].Resources[NodeResourceCPUMillicores] != 16000 {
		t.Fatalf("NodeShapes()[0] = %+v, want the learner's own 8 accelerators and 16 cores", shapes[0])
	}
	for i := 1; i < 3; i++ {
		if shapes[i].Group != "actor" || shapes[i].AcceleratorCount != 0 || shapes[i].Resources[NodeResourceCPUMillicores] != 1000 {
			t.Fatalf("NodeShapes()[%d] = %+v, want an actor with no accelerator and 1 core", i, shapes[i])
		}
	}
}
