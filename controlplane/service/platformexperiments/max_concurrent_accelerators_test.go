package platformexperiments

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// createStore fakes the single write Create makes; every other Store method
// is inherited from the embedded nil interface.
type createStore struct {
	fakeStore
	created *domain.PlatformExperiment
}

func (f *createStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, _ []*domain.Agent, _ []domain.AgentSignup) error {
	f.created = pe
	return nil
}

func (f *createStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.created, nil
}

func (f *createStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus, _ domain.ResearcherPool) error {
	f.created = pe
	return nil
}

func intPtr(v int) *int { return &v }

func TestCreatePlatformExperimentRoundTripsMaxConcurrentAccelerators(t *testing.T) {
	store := &createStore{}
	svc := newSignupTestService(t, store)

	pe, err := svc.Create(context.Background(), CreatePlatformExperimentRequest{Spec: experimentspec.Spec{
		Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 1,
		MaxAgents: 5, ReportIntervalSeconds: 10,
		Metrics:                   []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		Stages:                    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		MaxConcurrentAccelerators: intPtr(4),
	}, StartsAt: time.Now()})
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

	_, err := svc.Create(context.Background(), CreatePlatformExperimentRequest{Spec: experimentspec.Spec{
		Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 1,
		MaxAgents: 5, ReportIntervalSeconds: 10,
		Metrics:                   []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		Stages:                    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		MaxConcurrentAccelerators: intPtr(0),
	}, StartsAt: time.Now()})
	if err == nil {
		t.Fatal("Create accepted max_concurrent_accelerators=0")
	}
}
