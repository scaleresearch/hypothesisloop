package workload

import "testing"

// JobPhase crosses a process boundary as a string: the cluster agent encodes it, the control plane
// decodes it, and the two are built and deployed separately. Nothing else checks that the pair
// still agree, so a renamed case would be caught only by a cluster silently reporting every job as
// pending.
func TestJobPhaseRoundTripsThroughItsWireForm(t *testing.T) {
	for _, phase := range []JobPhase{JobPhasePending, JobPhaseRunning, JobPhaseSucceeded, JobPhaseFailed, JobPhaseGone} {
		if got := ParseJobPhase(phase.String()); got != phase {
			t.Errorf("ParseJobPhase(%q) = %v, want %v — the encoder and decoder disagree about this phase",
				phase.String(), got, phase)
		}
	}
}

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

// An unrecognized phase must decode to pending, never to a terminal state. A newer cluster agent
// reporting a phase this build has never heard of would otherwise have its jobs terminated —
// settled, billed and closed out — on a value that means nothing here.
func TestAnUnrecognizedPhaseDecodesToPendingRatherThanATerminalState(t *testing.T) {
	for _, s := range []string{"", "unknown", "Running", "SUCCEEDED", "terminating"} {
		if got := ParseJobPhase(s); got != JobPhasePending {
			t.Errorf("ParseJobPhase(%q) = %v, want JobPhasePending — an unreadable phase must never terminate a live job", s, got)
		}
	}
}
