package podexec

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
)

// Both runtimes read runtime/shared/testdata/grace_answer.json (k8sexec has the twin test): one
// reconcile answer, the same stop graces. The answer's unresolved job asks for more than the
// resolved one, so a runtime that clamps or reads Job instead of the resolution fails here.
func TestGraceFromTheSharedReconcileAnswer(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/testdata/grace_answer.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Experiment                 clusterapi.DesiredJob `json:"experiment"`
		OrdinaryStopGraceSeconds   int64                 `json:"ordinary_stop_grace_seconds"`
		CheckpointStopGraceSeconds int64                 `json:"checkpoint_stop_grace_seconds"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	cs, err := (&Executor{apiURL: "http://example.invalid:8083"}).BuildContainerSpec(&golden.Experiment, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := stopGrace(cs.Labels, false)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := stopGrace(cs.Labels, true)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary != golden.OrdinaryStopGraceSeconds+workloadenv.LaunchKillMargin || checkpoint != golden.CheckpointStopGraceSeconds+workloadenv.LaunchKillMargin {
		t.Fatalf("stop grace = (%d, %d), want (%d, %d) and the launch margin", ordinary, checkpoint, golden.OrdinaryStopGraceSeconds, golden.CheckpointStopGraceSeconds)
	}
}
