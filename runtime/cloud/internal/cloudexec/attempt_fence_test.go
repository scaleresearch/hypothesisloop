package cloudexec

import (
	"context"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

// A requeue reuses the experiment id. The attempt in the instance name is what keeps the
// instance of the attempt just replaced from being read as the one the control plane waits on.
func TestARequeuedJobReplacesTheRunningInstanceOfItsPreviousAttempt(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 1, 1))
	p.Start("hl-exp-1-a1-h100-0")
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhaseRunning || attempt != 1 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}

	requeued := job(t, "exp-1", 2, 1)
	matches, err := e.WorkloadMatchesDesired(ctx, requeued)
	if err != nil || matches {
		t.Fatalf("the previous attempt's instance matched the requeued job: %v, %v", matches, err)
	}
	// What the reconcile pass does with a mismatch: delete without a window, create next pass.
	if err := e.DeleteWorkload(ctx, "exp-1", false); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, requeued)

	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a2-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
	if matches, err := e.WorkloadMatchesDesired(ctx, requeued); err != nil || !matches {
		t.Fatalf("the new attempt's instance does not match: %v, %v", matches, err)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhasePending || attempt != 2 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}
}

// The terminal instance of the previous attempt is still rented when the requeued job comes
// back. It must neither satisfy the new attempt nor be reported as it.
func TestAnExitedInstanceOfThePreviousAttemptIsNeitherReusedNorReportedAsTheNewOne(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 1, 1))
	p.Start("hl-exp-1-a1-h100-0")
	p.Exit("hl-exp-1-a1-h100-0", exitline.Format(1))
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhaseFailed || attempt != 1 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}

	requeued := job(t, "exp-1", 2, 1)
	if ids, _ := e.ListManagedJobs(ctx); len(ids) != 0 {
		t.Fatalf("the exited instance is actual: %v", ids)
	}
	mustCreate(t, e, requeued)
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a1-h100-0", "hl-exp-1-a2-h100-1"}) {
		t.Fatalf("rented %v", names)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhasePending || attempt != 2 {
		t.Fatalf("status = %v attempt %d, want the new attempt's", phase, attempt)
	}
	// Asked about the new attempt, the old instance's failure is not its explanation.
	if reason, message, _, _, _, err := e.PollPhaseDetail(ctx, "exp-1", 2); err != nil || reason != "" || message != "" {
		t.Fatalf("detail = %q %q, %v", reason, message, err)
	}
	ids, err := e.ListManagedJobsForStatus(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{"exp-1"}) {
		t.Fatalf("status set = %v, %v, want the experiment once", ids, err)
	}

	if err := e.ReapTerminal(ctx, map[string]*domain.Experiment{"exp-1": requeued}); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a2-h100-1"}) {
		t.Fatalf("after reaping: %v", names)
	}
}
