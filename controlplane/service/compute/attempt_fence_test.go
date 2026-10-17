package compute

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func exitLine(code int) string {
	return exitLineAt(code, time.Unix(1000, 0), time.Unix(1001, 0))
}

func exitLineAt(code int, started, ended time.Time) string {
	return fmt.Sprintf("%s code=%d started=%d ended=%d", clusterapi.ExitLinePrefix, code, started.Unix(), ended.Unix())
}

// A requeue reuses the experiment id. The attempt in the instance name is what keeps the
// instance of the attempt just replaced from being read as the one the control plane waits on.
func TestARequeuedJobReplacesTheRunningInstanceOfItsPreviousAttempt(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 1, 1))
	p.Start("hl-job-t1-exp-1-a1-h100-0")
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhaseRunning || attempt != 1 {
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

	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a2-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
	if matches, err := e.WorkloadMatchesDesired(ctx, requeued); err != nil || !matches {
		t.Fatalf("the new attempt's instance does not match: %v, %v", matches, err)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhasePending || attempt != 2 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}
}

// The terminal instance of the previous attempt is still rented when the requeued job comes
// back. It must neither satisfy the new attempt nor be reported as it, so it is destroyed before
// the new attempt is rented and a status read finds one instance of the job.
func TestAnExitedInstanceOfThePreviousAttemptIsNeitherReusedNorReportedAsTheNewOne(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 1, 1))
	p.Start("hl-job-t1-exp-1-a1-h100-0")
	p.Exit("hl-job-t1-exp-1-a1-h100-0", exitLine(1))
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhaseFailed || attempt != 1 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}

	// The exited instance still holds h100-0, so admission could only have placed the new attempt
	// on the other slot.
	requeued := on(job(t, "exp-1", 2, 1), "h100-1")
	if ids, _ := e.ListManagedJobs(ctx); len(ids) != 0 {
		t.Fatalf("the exited instance is actual: %v", ids)
	}
	mustCreate(t, e, requeued)
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a2-h100-1"}) {
		t.Fatalf("rented %v", names)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhasePending || attempt != 2 {
		t.Fatalf("status = %v attempt %d, want the new attempt's", phase, attempt)
	}
	if reason, message, _, _, err := e.PollPhaseDetail(ctx, "exp-1", 2); err != nil || reason != clusterapi.PhaseReasonInstanceStarting {
		t.Fatalf("detail = %q %q, %v, want the new attempt's starting instance and nothing of the old one's exit", reason, message, err)
	}
}

// When the leftover cannot be destroyed, nothing is rented beside it: two instances of one job
// would leave a status read to choose between them.
func TestALeftoverThatCannotBeDestroyedHoldsBackTheNewInstance(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-1", 1, 1))
	p.Start("hl-job-t1-exp-1-a1-h100-0")
	p.Exit("hl-job-t1-exp-1-a1-h100-0", exitLine(1))
	p.Fail = failing("Destroy", "")

	requeued := on(job(t, "exp-1", 2, 1), "h100-1")
	if err := e.CreateWorkload(context.Background(), requeued); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v", err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a1-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
	p.Fail = nil
	mustCreate(t, e, requeued)
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a2-h100-1"}) {
		t.Fatalf("rented %v", names)
	}
}

// A preempted job is requeued under the attempt it had, and can be readmitted to the same
// cluster on another slot while the instance it was preempted from still stands. The attempt
// does not tell the two apart. The slot does: the old instance is not the job's workload, so it
// is deleted as drift and the job is rented where it was placed.
func TestAPreemptedJobReadmittedUnderTheSameAttemptIsRentedOnItsNewSlot(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const old, placed = "hl-job-t1-exp-1-a3-h100-0", "hl-job-t1-exp-1-a3-h100-1"
	mustCreate(t, e, job(t, "exp-1", 3, 1))
	p.Start(old)
	readmitted := on(job(t, "exp-1", 3, 1), "h100-1")

	matches, err := e.WorkloadMatchesDesired(ctx, readmitted)
	if err != nil || matches {
		t.Fatalf("the instance on the slot the job was preempted from matched: %v, %v", matches, err)
	}
	if err := e.CreateWorkload(ctx, readmitted); err == nil {
		t.Fatal("the job was rented a second time while its old instance still runs")
	}
	// What the reconcile pass does with a mismatch: delete without a window, create next pass.
	if err := e.DeleteWorkload(ctx, "exp-1", false); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, readmitted)
	if names := p.Names(); !reflect.DeepEqual(names, []string{placed}) {
		t.Fatalf("rented %v", names)
	}
	if matches, err := e.WorkloadMatchesDesired(ctx, readmitted); err != nil || !matches {
		t.Fatalf("the instance on the new slot does not match: %v, %v", matches, err)
	}
}

// The old instance may have exited before the pass sees it: stopped for the preemption, its
// command ended with 143. That exit is the preempted run's, and reporting it as the readmitted
// job's result failed the job and spent a retry.
func TestTheExitOfTheInstanceAJobWasPreemptedFromIsNotItsResult(t *testing.T) {
	for name, reapFirst := range map[string]bool{"create first": false, "reap first": true} {
		t.Run(name, func(t *testing.T) {
			e, p := newTestExecutor(t)
			ctx := context.Background()
			const old, placed = "hl-job-t1-exp-1-a3-h100-0", "hl-job-t1-exp-1-a3-h100-1"
			mustCreate(t, e, job(t, "exp-1", 3, 1))
			p.Start(old)
			p.Exit(old, exitLine(143))
			readmitted := on(job(t, "exp-1", 3, 1), "h100-1")

			if reapFirst {
				if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": readmitted}); err != nil {
					t.Fatal(err)
				}
				if names := p.Names(); len(names) != 0 {
					t.Fatalf("the exited instance on the old slot was kept as the job's: %v", names)
				}
			}
			mustCreate(t, e, readmitted)
			if names := p.Names(); !reflect.DeepEqual(names, []string{placed}) {
				t.Fatalf("rented %v, want the job on its new slot alone", names)
			}
			if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhasePending || attempt != 3 {
				t.Fatalf("status = %v attempt %d, want the new instance still booting", phase, attempt)
			}
		})
	}
}
