package podexec

import (
	"runtime"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// The node is reported of the architecture the agent runs on, whatever its configured labels say,
// so a job is never placed on it for an image of another.
func TestTheNodeIsReportedOfTheArchitectureTheAgentRunsOn(t *testing.T) {
	e := newFakeExecutor(newFakeEngine(), "/dev/fake/0")
	e.nodeName = "box"
	e.nodeLabels = map[string]string{clusterapi.NodeArchitectureLabel: "s390x", "zone": "b"}

	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := labels["box"]; got[clusterapi.NodeArchitectureLabel] != runtime.GOARCH || got["zone"] != "b" {
		t.Fatalf("node labels = %v, want %s %s beside the configured ones", got, clusterapi.NodeArchitectureLabel, runtime.GOARCH)
	}
}
