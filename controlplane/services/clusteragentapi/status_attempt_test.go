package clusteragentapi

import (
	"encoding/json"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// The upgrade path: a cluster-agent that predates the attempt field pushes a report without one.
// Recorded as attempt 0 it would be indistinguishable from a genuine first attempt — first
// attempts would look fine while every retry read as a foreign generation and was requeued or
// evicted forever. Absence has to survive decoding as absence.
func TestAStatusReportWithNoAttemptIsRecordedAsUnknownNotZero(t *testing.T) {
	var rep clusterapi.StatusReport
	if err := json.Unmarshal([]byte(`{"experiment_id":"exp-1","phase":"running"}`), &rep); err != nil {
		t.Fatal(err)
	}
	if got := reportedAttempt(rep); got != workload.AttemptUnknown {
		t.Fatalf("recorded attempt = %d, want %d (AttemptUnknown) — an old agent's silence became a number",
			got, workload.AttemptUnknown)
	}
}

// A first attempt is a real generation and must survive the same path as the number it is.
func TestAStatusReportForTheFirstAttemptIsRecordedAsZero(t *testing.T) {
	var rep clusterapi.StatusReport
	if err := json.Unmarshal([]byte(`{"experiment_id":"exp-1","phase":"running","attempt":0}`), &rep); err != nil {
		t.Fatal(err)
	}
	if got := reportedAttempt(rep); got != 0 {
		t.Fatalf("recorded attempt = %d, want 0", got)
	}
}

// The fence itself. What it protects is narrow and easy to lose: a retry admitted back onto the
// cluster it just failed on, while that cluster is still tearing the failed workload down.
func TestAReportFromASupersededAttemptIsDroppedNotRecorded(t *testing.T) {
	const cluster = "cluster-a"
	current := domain.Placement{ClusterName: cluster, AttemptCount: 2}

	for name, tc := range map[string]struct {
		rep       clusterapi.StatusReport
		placement domain.Placement
		accepted  bool
	}{
		"the attempt being waited on": {
			rep: reportOfAttempt(2), placement: current, accepted: true,
		},
		// The whole point: the attempt that just failed is still reporting Failed.
		"the attempt just replaced": {
			rep: reportOfAttempt(1), placement: current, accepted: false,
		},
		// A workload that outlived the experiment's own numbering — never plausible as current.
		"an attempt past the current one": {
			rep: reportOfAttempt(3), placement: current, accepted: false,
		},
		// A cluster-agent that predates the field. Accepted, or its cluster goes dark on upgrade.
		"a report naming no attempt": {
			rep: clusterapi.StatusReport{ExperimentID: "exp-1", Phase: "running"}, placement: current, accepted: true,
		},
		// Ownership still decides first, whatever the generation says.
		"the right attempt on the wrong cluster": {
			rep:       reportOfAttempt(2),
			placement: domain.Placement{ClusterName: "cluster-b", AttemptCount: 2},
			accepted:  false,
		},
		// An experiment the control plane has no row for at all: the zero Placement owns nothing.
		"an experiment that no longer exists": {
			rep: reportOfAttempt(2), placement: domain.Placement{}, accepted: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			reason, ok := rejectReport(tc.rep, tc.placement, cluster)
			if ok != tc.accepted {
				t.Fatalf("accepted = %v, want %v (reason %q)", ok, tc.accepted, reason)
			}
			if !ok && reason == "" {
				t.Error("a dropped report was given no reason — the log line is the only trace it leaves")
			}
		})
	}
}

// A first attempt is a real generation, and the fence must let it through. Read as absent it
// would still be accepted here, so the assertion that matters is the pairing: attempt 0 against a
// current attempt of 1 is a superseded report and must not be waved past as "no attempt named".
func TestAFirstAttemptIsFencedLikeAnyOtherNumber(t *testing.T) {
	const cluster = "cluster-a"
	if _, ok := rejectReport(reportOfAttempt(0), domain.Placement{ClusterName: cluster}, cluster); !ok {
		t.Error("a report for attempt 0 against a job on attempt 0 was dropped")
	}
	if _, ok := rejectReport(reportOfAttempt(0), domain.Placement{ClusterName: cluster, AttemptCount: 1}, cluster); ok {
		t.Error("a report for attempt 0 was accepted for a job already on attempt 1 — absence and zero are being conflated")
	}
}

// A preempted job keeps its attempt. Readmitted to another slot of the same cluster, the instance
// it was preempted from still reports under that attempt, and its exit must not become the
// readmitted job's result.
func TestAReportFromASlotTheJobIsNoLongerPlacedOnIsDropped(t *testing.T) {
	const cluster = "rented"
	placed := domain.Placement{ClusterName: cluster, AttemptCount: 3, Slot: "h100-1"}
	from := func(node, phase string) clusterapi.StatusReport {
		rep := reportOfAttempt(3)
		rep.AdmittedNode, rep.Phase = node, phase
		return rep
	}
	if reason, ok := rejectReport(from("h100-0", "failed"), placed, cluster); ok || reason == "" {
		t.Errorf("the old slot's exit was accepted (reason %q)", reason)
	}
	if _, ok := rejectReport(from("h100-0", "running"), placed, cluster); ok {
		t.Error("the old slot's instance was accepted as the job running")
	}
	if _, ok := rejectReport(from("h100-1", "running"), placed, cluster); !ok {
		t.Error("the report from the slot the job is placed on was dropped")
	}
	// A report with no node describes no instance: the job has none yet, or it is gone.
	if _, ok := rejectReport(from("", "gone"), placed, cluster); !ok {
		t.Error("a report naming no node was dropped")
	}
	// A job on a cluster without slots is fenced on cluster and attempt alone, as before.
	if _, ok := rejectReport(from("node-7", "running"), domain.Placement{ClusterName: cluster, AttemptCount: 3}, cluster); !ok {
		t.Error("a report was dropped for its node on a cluster without slots")
	}
}

func reportOfAttempt(n int) clusterapi.StatusReport {
	return clusterapi.StatusReport{ExperimentID: "exp-1", Phase: "running", Attempt: &n}
}
