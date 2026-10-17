package domain

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func TestOnlyTheCodeUnreachableExitIsTheEnvironmentsFailure(t *testing.T) {
	if got := clusterapi.PhaseReasonOfExit(ExitCodeCodeUnreachable); got != PhaseReasonCodeUnreachable {
		t.Fatalf("exit %d is %q", ExitCodeCodeUnreachable, got)
	}
	for _, code := range []int{1, 2, 137} {
		if got := clusterapi.PhaseReasonOfExit(code); got != PhaseReasonContainerFailed {
			t.Fatalf("exit %d is %q", code, got)
		}
	}

	eviction, environment := PhaseDetail{Reason: PhaseReasonCodeUnreachable, Message: "command exited with code 75: launch: the repository did not answer in 7 tries\nmore"}.EvictionOfFailure()
	if !environment || eviction != "code_unreachable: command exited with code 75: launch: the repository did not answer in 7 tries" || !IsInfrastructureFault(eviction) {
		t.Fatalf("got %q %v, infrastructure %v", eviction, environment, IsInfrastructureFault(eviction))
	}
	if eviction, environment := (PhaseDetail{Reason: PhaseReasonCodeUnreachable}).EvictionOfFailure(); !environment || eviction != EvictionCodeUnreachable {
		t.Fatalf("got %q %v", eviction, environment)
	}
	for _, reason := range []string{PhaseReasonContainerFailed, PhaseReasonOOMKilled, ""} {
		if eviction, environment := (PhaseDetail{Reason: reason, Message: "boom"}).EvictionOfFailure(); environment {
			t.Fatalf("%q is the environment's failure: %q", reason, eviction)
		}
	}
}

// Reporting a failure must not by itself condemn a job that still has retries left, and a create
// the cluster refused is tried again; a job whose own configuration cannot start is ended.
func TestOnlyReasonsThatNeverHealEndAJobEarly(t *testing.T) {
	for _, r := range []string{PhaseReasonContainerFailed, PhaseReasonOOMKilled, PhaseReasonCreateFailed} {
		if NeverSelfHealsPhaseReasons[r] {
			t.Errorf("%s must not be in NeverSelfHealsPhaseReasons: it would evict before retries run", r)
		}
	}
	if !NeverSelfHealsPhaseReasons[PhaseReasonConfigError] {
		t.Errorf("%s is not one the control plane ends a job for", PhaseReasonConfigError)
	}
}
