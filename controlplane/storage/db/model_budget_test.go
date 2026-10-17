package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A platform experiment keeps the model budget it states, a patch changes it, and one that states
// none reads as none, which is the platform's default.
func TestAPlatformExperimentKeepsItsModelBudget(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	now := time.Now().UTC()
	budget := 120.0
	pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "budgeted", BudgetAcceleratorHours: 10,
		MaxAgents: 10, StartsAt: now, DurationHours: 24, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now, ModelBudgetUSD: &budget,
	}
	if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := pes.GetPlatformExperiment(ctx, pe.ID)
	if err != nil || got.ModelBudgetUSD == nil || *got.ModelBudgetUSD != 120 {
		t.Fatalf("read: model_budget_usd = %v, %v, want 120", got.ModelBudgetUSD, err)
	}
	raised := 400.0
	got.ModelBudgetUSD = &raised
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpOpen, got.Researchers); err != nil {
		t.Fatalf("raise: %v", err)
	}
	listed, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range listed {
		if l.ID == pe.ID {
			found = l.ModelBudgetUSD != nil && *l.ModelBudgetUSD == 400
		}
	}
	if !found {
		t.Fatal("the list does not read the raised model budget")
	}
	plain := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, plain, 10)
	if got, err := pes.GetPlatformExperiment(ctx, plain); err != nil || got.ModelBudgetUSD != nil {
		t.Fatalf("one that states none: model_budget_usd = %v, %v, want none", got.ModelBudgetUSD, err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET model_budget_usd = 0 WHERE id = $1`, plain); err == nil {
		t.Fatal("a model budget of nothing was stored")
	}
}
