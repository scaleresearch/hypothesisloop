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

// Every phase reads back from its wire name, no two share one, and a name of none is refused,
// never read as pending.
func TestEveryPhaseReadsBackFromItsWireName(t *testing.T) {
	names := map[string]bool{}
	for _, p := range JobPhases {
		if names[p.String()] {
			t.Errorf("two phases are named %q", p.String())
		}
		names[p.String()] = true
		if got, ok := ParseJobPhase(p.String()); !ok || got != p {
			t.Errorf("ParseJobPhase(%q) = %v, %v, want %v", p.String(), got, ok, p)
		}
	}
	for _, name := range []string{"", "Pending", "RUNNING", "completed", "unknown"} {
		if _, ok := ParseJobPhase(name); ok {
			t.Errorf("ParseJobPhase(%q) read a phase", name)
		}
	}
}
