package db

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAJobTheAdmissionLoopHasNotSeenIsAwaitingAdmission(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps := NewExperimentsStore(pool)
	ctx := context.Background()
	reason := func(id string) string {
		t.Helper()
		exp, err := exps.GetExperiment(ctx, id)
		if err != nil || exp == nil {
			t.Fatalf("GetExperiment(%s): got = %v, %v", id, exp, err)
		}
		return exp.NotAdmittedReason
	}

	seedExperimentForCapTest(t, pool, "awaiting-new-"+peID, agentID, peID, 1, domain.StatusQueued)
	if got := reason("awaiting-new-" + peID); got != domain.NotAdmittedAwaitingAdmission {
		t.Errorf("a new job: not_admitted_reason = %q, want %q", got, domain.NotAdmittedAwaitingAdmission)
	}

	seedExperimentForCapTest(t, pool, "awaiting-requeued-"+peID, agentID, peID, 1, domain.StatusRunning)
	requeued, err := exps.RequeuePreempted(ctx, "awaiting-requeued-"+peID, 1, 1)
	if err != nil || !requeued {
		t.Fatalf("RequeuePreempted: got = %v, %v, want true", requeued, err)
	}
	if got := reason("awaiting-requeued-" + peID); got != domain.NotAdmittedAwaitingAdmission {
		t.Errorf("a requeued job: not_admitted_reason = %q, want %q", got, domain.NotAdmittedAwaitingAdmission)
	}
}

func TestMarkUnderperformingIsTrueOncePerJob(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps := NewExperimentsStore(pool)
	id := "underperforming-" + peID
	seedExperimentForCapTest(t, pool, id, agentID, peID, 1, domain.StatusRunning)

	for i, want := range []bool{true, false} {
		got, err := exps.MarkUnderperforming(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("call %d: MarkUnderperforming = %v, %v, want %v", i+1, got, err, want)
		}
	}
}

// A job read says when the job ended and carries the summary filed for it, so no reader has to
// infer the first from updated_at or search a hypothesis's findings for the second.
func TestAFinishedJobCarriesItsFinishTimeAndItsFiledSummary(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps, hyps := NewExperimentsStore(pool), NewHypothesesStore(pool)
	ctx := context.Background()
	id := "finished-" + peID
	seedExperimentForCapTest(t, pool, id, agentID, peID, 1, domain.StatusRunning)

	running, err := exps.GetExperiment(ctx, id)
	if err != nil || running.FinishedAt != nil {
		t.Fatalf("a running job: finished_at = %v, %v, want nil", running.FinishedAt, err)
	}
	if got, err := hyps.FindingSummary(ctx, id); err != nil || got != "" {
		t.Errorf("FindingSummary before one is filed: got = %q, %v, want empty", got, err)
	}

	if ok, err := exps.TransitionStatus(ctx, id, domain.StatusRunning, domain.StatusCompleted); err != nil || !ok {
		t.Fatalf("TransitionStatus: got = %v, %v, want true", ok, err)
	}
	if _, err := hyps.CreateHypothesisFinding(ctx, running.HypothesisID, peID, id, agentID, "it held"); err != nil {
		t.Fatalf("CreateHypothesisFinding: %v", err)
	}
	done, err := exps.GetExperiment(ctx, id)
	if err != nil || done.FinishedAt == nil || !done.FinishedAt.After(running.UpdatedAt) {
		t.Fatalf("a completed job: finished_at = %v, %v, want a time after %v", done.FinishedAt, err, running.UpdatedAt)
	}
	if err := exps.MarkQuotaSettled(ctx, id); err != nil {
		t.Fatalf("MarkQuotaSettled: %v", err)
	}
	if settled, err := exps.GetExperiment(ctx, id); err != nil || !settled.FinishedAt.Equal(*done.FinishedAt) {
		t.Errorf("after settlement: finished_at = %v, %v, want it unmoved at %v", settled.FinishedAt, err, done.FinishedAt)
	}
	if got, err := hyps.FindingSummary(ctx, id); err != nil || got != "it held" {
		t.Errorf("FindingSummary: got = %q, %v, want %q", got, err, "it held")
	}
}
