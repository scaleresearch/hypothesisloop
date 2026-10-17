package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const cloneSaid = "command exited with code 75: hl-clone: could not fetch the code: 4 tries failed"

func TestAJobWhoseCodeWasUnreachableIsRequeuedAndNeverFailed(t *testing.T) {
	for _, status := range []domain.ExperimentStatus{domain.StatusSubmitted, domain.StatusRunning} {
		store := &lifecycleStore{infraCeiling: 3}
		backend := &scaleUpBackend{phase: domain.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonCodeUnreachable, message: cloneSaid}
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
	backend := &scaleUpBackend{phase: domain.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonCodeUnreachable, message: cloneSaid}
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
	backend := &scaleUpBackend{phase: domain.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonContainerFailed, message: "command exited with code 1: boom"}
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

// The cause of a failure is often lines above the last one, and the line between them can be
// long: the update carries the last lines of output, each cut short, so the cause is still among them.
func TestAFailedJobsUpdateCarriesItsLastLinesOfOutput(t *testing.T) {
	rustc := "  process didn't exit successfully: `rustc --crate-name job " + strings.Repeat("-C opt ", 200) + "` (signal: 11, SIGSEGV)"
	tail := []string{"   Compiling job v0.1.0"}
	for i := range 30 {
		tail = append(tail, fmt.Sprintf("early line %d", i))
	}
	tail = append(tail, "qemu: uncaught target signal 11 (Segmentation fault) - core dumped", "error: could not compile `job`", "", "Caused by:", rustc, "command failed (101): cargo run")
	store := &lifecycleStore{infraCeiling: 3}
	backend := &scaleUpBackend{phase: domain.JobPhaseFailed, scheduledNodes: 1, reason: domain.PhaseReasonContainerFailed, message: "container exited with code 101", logTail: tail}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())

	exp := scaleUpExperiment(1, 0)
	exp.Status = domain.StatusRunning
	if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.finished) != 1 {
		t.Fatalf("finished %q", store.finished)
	}
	got := store.finished[0]
	head, output, ok := strings.Cut(got, "\nlast output:\n")
	if !ok || head != "FAILED container_failed: container exited with code 101" {
		t.Fatalf("finished with %q, want the cause and then the last output", got)
	}
	lines := strings.Split(output, "\n")
	if len(output) > failureOutputBytes || len(lines) > failureOutputLines {
		t.Errorf("last output is %d bytes in %d lines, want at most %d in %d", len(output), len(lines), failureOutputBytes, failureOutputLines)
	}
	if !strings.Contains(output, "qemu: uncaught target signal 11") || lines[len(lines)-1] != "command failed (101): cargo run" {
		t.Errorf("last output does not hold the segfault and end with the last line:\n%s", output)
	}
	if slices.Contains(lines, "") || slices.ContainsFunc(lines, func(l string) bool { return len(l) > failureOutputLineBytes }) {
		t.Errorf("last output holds a blank or an uncut line:\n%s", output)
	}

	if got := withLastOutput("container_failed", nil); got != "container_failed" {
		t.Errorf("with no output reported the cause is %q, want it alone", got)
	}
}

func TestAJobThatFoundNoMachineToRentGoesBackToTheQueue(t *testing.T) {
	said := "no machine to rent for shape rtx4090 in 3m0s of asking"
	store := &lifecycleStore{infraCeiling: 3}
	backend := &scaleUpBackend{phase: domain.JobPhasePending, reason: domain.PhaseReasonNoOffer, message: said}
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
	backend := &scaleUpBackend{phase: domain.JobPhasePending, reason: domain.PhaseReasonImagePullFailed, message: "manifest unknown"}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop())
	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 0), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != "unschedulable: image_pull_failed: manifest unknown" {
		t.Fatalf("terminal %q", store.terminalReasons)
	}

	if got := notRunningInTime(5*time.Minute, domain.PhaseDetail{Reason: domain.PhaseReasonInstanceStarting}); got != "admitted and not running within 5m0s, its runtime reporting instance_starting" {
		t.Fatalf("got %q", got)
	}
	createFailed := domain.PhaseDetail{Reason: domain.PhaseReasonCreateFailed, Message: `secrets "exp-job-1-pull" is forbidden`}
	if got := notRunningInTime(5*time.Minute, createFailed); got != `admitted and not running within 5m0s, its runtime reporting create_failed: secrets "exp-job-1-pull" is forbidden` {
		t.Fatalf("got %q", got)
	}
	if got := notRunningInTime(5*time.Minute, domain.PhaseDetail{}); got != "admitted and not running within 5m0s" {
		t.Fatalf("got %q", got)
	}
}
