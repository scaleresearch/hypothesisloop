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

func (f *createStore) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error {
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
}

func TestCreatePlatformExperimentRejectsZeroMaxConcurrentAccelerators(t *testing.T) {
	store := &createStore{}
	svc := newSignupTestService(t, store)

	_, err := svc.Create(context.Background(), CreatePlatformExperimentRequest{
		Name: "exp", BudgetAcceleratorHours: 10, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
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
