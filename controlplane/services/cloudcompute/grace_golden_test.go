package cloudcompute

import (
	"reflect"
	"testing"
)

// Every runtime reads runtime/shared/testdata/grace_answer.json (k8sexec and podexec have the twin
// test): one reconcile answer, the same stop graces. The answer's unresolved job asks for more
// than the resolved one, so a runtime that clamps or reads Job instead of the resolution fails.
//
// This runtime compiles one grace into the workload, the one a granted termination gets: the
// wrapper is only ever sent SIGTERM for those. A stop without a grant destroys the instance, so
// the ordinary grace has nothing to be compiled into, and the test pins that it is the resolved
// one that was read and found shorter.
func TestGraceFromTheSharedReconcileAnswer(t *testing.T) {
	golden := readGolden(t)
	e, _ := newTestExecutor(t)
	exp := job(t, golden.Experiment.ID, 0, 0)

	spec, err := e.buildSpec(exp)
	if err != nil {
		t.Fatal(err)
	}
	if want := wrapperCommand(golden.CheckpointStopGraceSeconds, append(append([]string(nil), exp.ResolvedJob.Command...), exp.ResolvedJob.Args...)); !reflect.DeepEqual(spec.Command, want) {
		t.Fatalf("command = %q, want %q", spec.Command, want)
	}
	ordinary, _, err := exp.EffectiveJob().ResolvedGrace()
	if err != nil || ordinary != golden.OrdinaryStopGraceSeconds {
		t.Fatalf("ordinary grace = %d, %v, want %d", ordinary, err, golden.OrdinaryStopGraceSeconds)
	}
}
