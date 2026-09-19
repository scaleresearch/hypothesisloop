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
