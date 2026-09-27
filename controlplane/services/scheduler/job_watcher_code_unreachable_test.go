package scheduler

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

const cloneSaid = "command exited with code 75: hl-clone: could not fetch the code: 4 tries failed"

func TestAJobWhoseCodeWasUnreachableIsRequeuedAndNeverFailed(t *testing.T) {
	for _, status := range []domain.ExperimentStatus{domain.StatusSubmitted, domain.StatusRunning} {
		store := &lifecycleStore{infraCeiling: 3}
		backend := &scaleUpBackend{phase: workload.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonCodeUnreachable, message: cloneSaid}
		w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())
		exp := scaleUpExperiment(1, 0)
		exp.Status = status

		if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil {
			t.Fatal(err)
		}
		want := "code_unreachable: " + cloneSaid
		if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != want {
			t.Fatalf("%s: requeued with %q, want %q", status, store.requeuedReasons, want)
		}
		if len(store.finished) != 0 || len(store.terminalReasons) != 0 || store.markedSettled != 0 {
			t.Fatalf("%s: finished %q, terminal %q, settled %d: the job was ended", status, store.finished, store.terminalReasons, store.markedSettled)
		}
	}
}

func TestAJobWhoseCodeStaysUnreachableEndsEvictedWithThatReason(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 0}
	backend := &scaleUpBackend{phase: workload.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonCodeUnreachable, message: cloneSaid}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 0), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != "code_unreachable: "+cloneSaid || len(store.finished) != 0 {
		t.Fatalf("terminal %q, finished %q: want one eviction and no FAILED", store.terminalReasons, store.finished)
	}
}

func TestAJobWhoseOwnCommandFailedIsFailed(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
	backend := &scaleUpBackend{phase: workload.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonContainerFailed, message: "command exited with code 1: boom"}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())

	exp := scaleUpExperiment(1, 0)
	exp.Status = domain.StatusRunning
	if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 0 || len(store.finished) != 1 || store.finished[0] != "FAILED container_failed: command exited with code 1: boom" {
		t.Fatalf("requeued %q, finished %q", store.requeuedReasons, store.finished)
	}
}

func TestAJobThatFoundNoMachineToRentGoesBackToTheQueue(t *testing.T) {
	said := "no machine to rent for shape rtx4090 in 3m0s of asking"
	store := &lifecycleStore{infraCeiling: 3}
	backend := &scaleUpBackend{phase: workload.JobPhasePending, reason: domain.PhaseReasonNoOffer, message: said}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 0), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != "no_offer: "+said || len(store.terminalReasons) != 0 {
		t.Fatalf("requeued %q, terminal %q", store.requeuedReasons, store.terminalReasons)
	}
}

// The update about an eviction is its reason: what the runtime said goes into it, where the
// phase detail alone had it.
func TestAnEvictionSaysWhatTheRuntimeSaid(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
	backend := &scaleUpBackend{phase: workload.JobPhasePending, reason: domain.PhaseReasonImagePullFailed, message: "manifest unknown"}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())
	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 0), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != "unschedulable: image_pull_failed: manifest unknown" {
		t.Fatalf("terminal %q", store.terminalReasons)
	}

	if got := notRunningInTime(5*time.Minute, domain.PhaseReasonInstanceStarting); got != "admitted and not running within 5m0s, its runtime reporting instance_starting" {
		t.Fatalf("got %q", got)
	}
	if got := notRunningInTime(5*time.Minute, ""); got != "admitted and not running within 5m0s" {
		t.Fatalf("got %q", got)
	}
}
