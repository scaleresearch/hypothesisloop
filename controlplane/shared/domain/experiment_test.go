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
