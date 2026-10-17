package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	key := ResourceKey{Kind: ResourceKindExtended, Name: "google.com/tpu"}
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

	fp := (&Experiment{Job: spec, AcceleratorType: spec.AcceleratorType, AcceleratorCount: spec.TotalAccelerators()}).Footprint()
	want := map[ResourceKey]int64{
		{Kind: ResourceKindCPU}:     12000,            // 4 cores x 3
		{Kind: ResourceKindMemory}:  3 * 8 * 1 << 30,  // 8Gi x 3
		{Kind: ResourceKindStorage}: 3 * 10 * 1 << 30, // 10Gi x 3
		{Kind: ResourceKindAccelerator, Name: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}: 6,  // 2 x 3
		{Kind: ResourceKindExtended, Name: "google.com/tpu"}:                                  24, // 8 x 3
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

	fp := (&Experiment{Job: spec, AcceleratorType: spec.AcceleratorType, AcceleratorCount: spec.TotalAccelerators()}).Footprint()
	want := map[ResourceKey]int64{
		{Kind: ResourceKindCPU}:     80000,               // 16 + 64 x 1 cores
		{Kind: ResourceKindMemory}:  384 * 1 << 30,       // 128Gi + 64 x 4Gi
		{Kind: ResourceKindStorage}: (10 + 64) * 1 << 30, // 10Gi + 64 x 1Gi
		{Kind: ResourceKindAccelerator, Name: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}: 8, // 8 + 64 x 0
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
	if got := fp[ResourceKey{Kind: ResourceKindAccelerator, Name: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}]; got != 8 {
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
	if err := ValidateGroups(spec); err == nil {
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
	if err := ValidateGroups(spec); err == nil {
		t.Fatalf("ValidateGroups() accepted a grouped spec that also sets job.cpu — there would be no rule saying which of the two a worker actually gets")
	}
}

// One accelerator type per job. The experiment carries a single AcceleratorType that admission
// filters on, accelerator type substitution rewrites and billing prices against; two groups naming
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
	if err := ValidateGroups(spec); err == nil {
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
	if err := ValidateGroups(spec); err != nil {
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
	if err := ValidateGroups(spec); err != nil {
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

	shapes := NodeShapes(spec)
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

	shapes := NodeShapes(spec)
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

// A job sized to a slot holds what admission resolved it to. Read from the submitted job, the
// cluster's desired-free capacity and a preemption's credit both counted it as holding less than
// the slot it fills.
func TestExperimentFootprintCountsWhatAdmissionResolved(t *testing.T) {
	exp := &Experiment{Job: JobSpec{CPU: "2", Memory: "4Gi", Storage: "10Gi"}}
	submitted := exp.Footprint()
	if submitted[ResourceKey{Kind: ResourceKindCPU}] != 2000 || submitted[ResourceKey{Kind: ResourceKindStorage}] != 10<<30 {
		t.Fatalf("before admission: %v", submitted)
	}

	resolved := exp.Job
	resolved.CPU, resolved.Memory, resolved.Storage = "8", "32Gi", "100Gi"
	exp.ResolvedJob = &resolved
	got := exp.Footprint()
	for key, want := range map[ResourceKey]int64{
		{Kind: ResourceKindCPU}: 8000, {Kind: ResourceKindMemory}: 32 << 30, {Kind: ResourceKindStorage}: 100 << 30,
	} {
		if got[key] != want {
			t.Errorf("%v = %d, want %d", key, got[key], want)
		}
	}
	if exp.Job.CPU != "2" {
		t.Error("the submitted job was rewritten")
	}
}

// A job states cpu as a number or as a string, and it is kept and answered as the string.
func TestAJobsCPUIsReadAsANumberOrAStringAndAnsweredAsTheString(t *testing.T) {
	for body, want := range map[string]string{
		`{"cpu":4,"groups":[{"cpu":2}]}`:        "4 2",
		`{"cpu":"500m","groups":[{"cpu":"1"}]}`: "500m 1",
		`{"cpu":0.5,"groups":[{"cpu":"2"}]}`:    "0.5 2",
		`{"image":"i","groups":[{}]}`:           " ",
	} {
		var job JobSpec
		if err := json.Unmarshal([]byte(body), &job); err != nil || job.CPU+" "+job.Groups[0].CPU != want {
			t.Errorf("%s: got = %q and %q, %v, want %q", body, job.CPU, job.Groups[0].CPU, err, want)
		}
	}
	var job JobSpec
	if err := json.Unmarshal([]byte(`{"image":"i","cpu":true}`), &job); err == nil {
		t.Error("cpu: true was accepted")
	}
	if err := json.Unmarshal([]byte(`{"cpu":4,"memory":"1Gi"}`), &job); err != nil || job.Memory != "1Gi" {
		t.Fatalf("the fields beside cpu: got = %+v, %v", job, err)
	}
	out, err := json.Marshal(job)
	if err != nil || !strings.Contains(string(out), `"cpu":"4"`) {
		t.Errorf("answered as %s, %v, want cpu as the string", out, err)
	}
}

// A run's researcher was refused for 0.08 and then admitted with 0.052 on the same 8 cores: the
// first submit stated 0.01 h and the second the measured 0.0065 h. The hold is the stated hours
// times the units at the type's rate, with no minimum, and an unpriced type has none.
func TestEstimateHeldIsRateTimesUnitsTimesHours(t *testing.T) {
	before := acceleratorRateRegistry
	t.Cleanup(func() { acceleratorRateRegistry = before })
	SetAcceleratorRates(map[string]float64{string(CPUType): 1})

	job := &Experiment{ID: "job-a", AcceleratorType: CPUType, AcceleratorCount: 8}
	for hours, want := range map[float64]float64{0.01: 0.08, 0.0065: 0.052} {
		job.EstimatedDurationHours = hours
		if got, err := job.EstimateHeld(); err != nil || got != want {
			t.Errorf("EstimateHeld() at %g h = %v %v, want %v", hours, got, err, want)
		}
	}
	job.AcceleratorType = "vendor.example/unpriced=x"
	if _, err := job.EstimateHeld(); err == nil || !strings.Contains(err.Error(), "no configured acch_rate") {
		t.Errorf("EstimateHeld() of an unpriced type = %v, want it refused for the missing rate", err)
	}
}

// A baseline agent's job is stored on the objective under the platform experiment's id, which
// is no hypothesis: read as one it answers 404, so no answer hands it out as hypothesis_id.
func TestAJobOnTheObjectiveIsAnsweredWithoutAHypothesisID(t *testing.T) {
	answered := func(e Experiment) map[string]any {
		t.Helper()
		raw, err := json.Marshal([]*Experiment{&e})
		if err != nil {
			t.Fatalf("Marshal: got = %v, want nil", err)
		}
		var out []map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal: got = %v, want nil", err)
		}
		return out[0]
	}
	baseline := Experiment{ID: "job-1", PlatformExperimentID: "pe-1", HypothesisID: "pe-1"}
	if got := answered(baseline); got["objective"] != true || got["id"] != "job-1" || got["platform_experiment_id"] != "pe-1" {
		t.Errorf("a job on the objective: got = %v, want objective true beside its other fields", got)
	} else if _, stated := got["hypothesis_id"]; stated {
		t.Errorf("a job on the objective: got hypothesis_id %v, want none", got["hypothesis_id"])
	}
	if !baseline.OnObjective() || baseline.HypothesisID != "pe-1" {
		t.Errorf("after the answer: got = %+v, want the job still on the objective as stored", baseline)
	}
	tested := answered(Experiment{ID: "job-2", PlatformExperimentID: "pe-1", HypothesisID: "hyp-1"})
	if _, stated := tested["objective"]; stated || tested["hypothesis_id"] != "hyp-1" {
		t.Errorf("a job testing a hypothesis: got = %v, want hypothesis_id hyp-1 and no objective", tested)
	}
}
