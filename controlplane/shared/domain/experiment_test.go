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
