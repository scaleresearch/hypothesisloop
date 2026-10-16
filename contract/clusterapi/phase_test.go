package clusterapi

import "testing"

// The names are the wire format, not an implementation detail: changing one is a protocol break
// against every already-deployed cluster agent, which this pins so the change has to be deliberate.
func TestJobPhaseWireNamesAreStable(t *testing.T) {
	for phase, want := range map[JobPhase]string{
		JobPhasePending:   "pending",
		JobPhaseRunning:   "running",
		JobPhaseSucceeded: "succeeded",
		JobPhaseFailed:    "failed",
		JobPhaseGone:      "gone",
	} {
		if got := phase.String(); got != want {
			t.Errorf("phase %d renders as %q, want %q — deployed cluster agents send and expect the old name", phase, got, want)
		}
	}
}
