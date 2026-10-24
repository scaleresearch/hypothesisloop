package controller

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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

// Settlement bills a CPU job at its estimate's rate, so the boundary counts it while it runs.
func TestStageProgressCountsARunningCPUJob(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", BudgetAcceleratorHours: 100, CreatedAt: now.Add(-time.Hour)}
	cpu := &domain.Experiment{
		ID: "cpu", PlatformExperimentID: pe.ID,
		EstimatedDurationHours: 2, EstimatedCostAccH: 20,
	}
	c := &Controller{observed: fakeObserved{}}
	obs := &tickObservations{elapsedHours: map[string]float64{"cpu": 1}}

	got, err := c.stageProgress(context.Background(), pe, []*domain.Experiment{cpu}, obs, now)
	if err != nil {
		t.Fatalf("stageProgress: %v", err)
	}
	if want := 0.10; math.Abs(got-want) > 1e-9 {
		t.Fatalf("progress = %v, want %v", got, want)
	}
}

// Stages were measured from the accept, so the wait for a baseline ended stage 1 four minutes
// after the executors started. The ladder's clock is the run's: it counts from started_at to
// ends_at, and what came before the start moves no boundary.
func TestStageProgressCountsTheRunFromItsStartAndNotFromItsAccept(t *testing.T) {
	now := time.Now().UTC()
	c := &Controller{observed: fakeObserved{}}
	obs := &tickObservations{}
	stages := []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60}}
	started := now.Add(-4 * time.Minute)
	pe := &domain.PlatformExperiment{
		ID: "pe-1", BudgetAcceleratorHours: 100, Stages: stages, CurrentStage: 1,
		CreatedAt: now.Add(-18 * time.Minute), StartsAt: now.Add(-18 * time.Minute),
		StartedAt: started, DurationHours: 0.75, EndsAt: started.Add(45 * time.Minute),
	}
	got, err := c.stageProgress(context.Background(), pe, nil, obs, now)
	if err != nil {
		t.Fatalf("stageProgress: %v", err)
	}
	if want := 4.0 / 45; math.Abs(got-want) > 1e-9 {
		t.Errorf("4 minutes into a 45 minute run accepted 18 minutes ago: progress = %v, want %v", got, want)
	}
	if boundary := domain.BoundaryProgress(stages, 1); got >= boundary {
		t.Errorf("progress %v is past the first boundary %v: the wait for the baseline was counted", got, boundary)
	}

	waiting := *pe
	waiting.StartedAt, waiting.EndsAt = time.Time{}, time.Time{}
	if got, err := c.stageProgress(context.Background(), &waiting, nil, obs, now); err != nil || got != 0 {
		t.Errorf("before the start: progress = %v, %v, want 0", got, err)
	}
}
