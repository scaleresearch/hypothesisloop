package quota

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// createStore fakes the single write Create makes; every other PlatformExperimentsStore method
// is inherited from the embedded nil interface.
type createStore struct {
	PlatformExperimentsStore
	created *domain.PlatformExperiment
}

func (f *createStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment) error {
	f.created = pe
	return nil
}

func (f *createStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.created, nil
}

func (f *createStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus) error {
	f.created = pe
	return nil
}

func intPtr(v int) *int { return &v }

func TestCreatePlatformExperimentRoundTripsMaxConcurrentAccelerators(t *testing.T) {
	store := &createStore{}
	svc := newSignupTestService(t, store)

	pe, err := svc.Create(context.Background(), CreatePlatformExperimentRequest{
		Name: "exp", BudgetAcceleratorHours: 10, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
		MaxAgents: 5, ReportIntervalSeconds: 10,
		Metrics:                   []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		Stages:                    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		MaxConcurrentAccelerators: intPtr(4),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pe.MaxConcurrentAccelerators == nil || *pe.MaxConcurrentAccelerators != 4 {
		t.Fatalf("Create: got MaxConcurrentAccelerators = %v, want 4", pe.MaxConcurrentAccelerators)
	}
	if pe.DefaultQuotaTier != domain.QuotaTierGuaranteed {
		t.Fatalf("Create: got DefaultQuotaTier = %q, want guaranteed", pe.DefaultQuotaTier)
	}
}

func TestCreatePlatformExperimentRejectsZeroMaxConcurrentAccelerators(t *testing.T) {
	store := &createStore{}
	svc := newSignupTestService(t, store)

	_, err := svc.Create(context.Background(), CreatePlatformExperimentRequest{
		Name: "exp", BudgetAcceleratorHours: 10, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
		MaxAgents: 5, ReportIntervalSeconds: 10,
		Metrics:                   []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		Stages:                    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		MaxConcurrentAccelerators: intPtr(0),
	})
	if err == nil {
		t.Fatal("Create accepted max_concurrent_accelerators=0")
	}
}

func TestUpdatePlatformExperimentChangesMaxConcurrentAccelerators(t *testing.T) {
	store := &createStore{created: &domain.PlatformExperiment{
		ID: "pe-1", Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
	}}
	svc := newSignupTestService(t, store)

	pe, err := svc.Update(context.Background(), "pe-1", CreatePlatformExperimentRequest{
		MaxConcurrentAccelerators: intPtr(8),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if pe.MaxConcurrentAccelerators == nil || *pe.MaxConcurrentAccelerators != 8 {
		t.Fatalf("Update: got MaxConcurrentAccelerators = %v, want 8", pe.MaxConcurrentAccelerators)
	}
}

func TestUpdateRunningPlatformExperimentNamesTheLockedFieldsThatDiffer(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &createStore{created: &domain.PlatformExperiment{
		ID: "pe-1", Status: domain.PlatformExpRunning, Description: "old",
		BudgetAcceleratorHours: 20, MaxAgents: 4, StartsAt: start, EndsAt: start.Add(time.Hour),
	}}
	svc := newSignupTestService(t, store)

	_, err := svc.Update(context.Background(), "pe-1", CreatePlatformExperimentRequest{
		Description: "new", BudgetAcceleratorHours: 10, MaxAgents: 4, StartsAt: start.Add(-24 * time.Hour),
	})
	want := "experiment_running: only name and description can be amended once running; these fields differ from the live experiment: budget_accelerator_hours, starts_at; set them to the live values"
	if err == nil || err.Error() != want {
		t.Fatalf("Update error = %v\nwant %s", err, want)
	}

	pe, err := svc.Update(context.Background(), "pe-1", CreatePlatformExperimentRequest{
		Description: "new", BudgetAcceleratorHours: 20, StartsAt: start,
	})
	if err != nil {
		t.Fatalf("Update with only live values besides the description: %v", err)
	}
	if pe.Description != "new" {
		t.Fatalf("Description = %q, want %q", pe.Description, "new")
	}
}
