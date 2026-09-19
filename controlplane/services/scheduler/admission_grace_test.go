package scheduler

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var testGraceLimits = domain.GraceLimits{DefaultShutdownSeconds: 5, MaxShutdownSeconds: 30, MaxCheckpointSeconds: 600}

// Admission is the one place grace is resolved: the claimed job carries the shutdown grace
// (default or capped request) and the capped checkpoint window the runtime applies verbatim.
func TestAdmissionWritesResolvedGraceIntoTheClaimedJob(t *testing.T) {
	requested, window := int64(90), int64(86400)
	exp := distributedExperiment(1, 1)
	exp.ID = "exp-1"
	exp.Job.TerminationGracePeriodSeconds = &requested
	exp.Job.CheckpointGraceSeconds = &window
	store := &nilCallbackClaimStore{}
	l := NewLoop(store, submitQuota{}, &liveWorkload{}, testGraceLimits, zap.NewNop())

	if err := l.submitJobTo(context.Background(), exp, "cluster-a", "", exp.AcceleratorType, true, true); err != nil {
		t.Fatalf("submitJobTo = %v", err)
	}
	got := store.resolvedJob
	if got == nil || got.TerminationGracePeriodSeconds == nil || got.CheckpointGraceSeconds == nil {
		t.Fatalf("claimed job = %+v, want resolved grace", got)
	}
	if *got.TerminationGracePeriodSeconds != 30 || *got.CheckpointGraceSeconds != 600 {
		t.Fatalf("claimed grace = (%d, %d), want (30, 600)", *got.TerminationGracePeriodSeconds, *got.CheckpointGraceSeconds)
	}
	if *exp.Job.TerminationGracePeriodSeconds != 90 {
		t.Fatal("admission rewrote the submitted job")
	}
}
