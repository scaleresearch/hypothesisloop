package controller

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A requeue settles the interrupted stint while the job lives on, and observed hours are
// cumulative over the job's whole life. A job running its second stint must move the boundary by
// its lifetime cost once.
func TestStageProgressCountsARequeuedJobsSettledStintOnce(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", BudgetAcceleratorHours: 100, CreatedAt: now.Add(-time.Hour)}
	requeued := &domain.Experiment{
		ID: "requeued", PlatformExperimentID: pe.ID,
		AcceleratorType: "h100", AcceleratorCount: 1,
		EstimatedDurationHours: 2, EstimatedCostAccH: 20, // 10 AccH per hour
	}
	c := &Controller{observed: fakeObserved{settledAccH: map[string]float64{
		"requeued": 10, // its first stint, one hour
		"finished": 5,
	}}}
	obs := &tickObservations{elapsedHours: map[string]float64{"requeued": 1.5}}

	got, err := c.stageProgress(context.Background(), pe, []*domain.Experiment{requeued}, obs, now)
	if err != nil {
		t.Fatalf("stageProgress: %v", err)
	}
	// 15 AccH over the requeued job's whole life plus 5 from the finished one.
	if want := 0.20; math.Abs(got-want) > 1e-9 {
		t.Fatalf("progress = %v, want %v: the settled stint is counted twice", got, want)
	}
}
