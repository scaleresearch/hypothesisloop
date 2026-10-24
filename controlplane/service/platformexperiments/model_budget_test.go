package platformexperiments

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// budgetStore is createStore for a patch, which reads the signups of the platform experiment.
type budgetStore struct{ createStore }

func (*budgetStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	return nil, nil
}

func budgetRequest(budget *float64) CreatePlatformExperimentRequest {
	return CreatePlatformExperimentRequest{Spec: experimentspec.Spec{
		Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 1,
		MaxAgents: 5, ReportIntervalSeconds: 10,
		Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		Stages:  []domain.Stage{{LengthPct: 100, EvictPct: 0}},
	}, StartsAt: time.Now(), ModelBudgetUSD: budget}
}

func TestAModelBudgetIsStatedUpToThePlatformsMost(t *testing.T) {
	store := &budgetStore{}
	svc := newSignupTestService(t, store)
	for _, over := range []float64{0, -1, 500.01} {
		_, err := svc.Create(context.Background(), budgetRequest(&over))
		if refusal.ReasonOf(err) != domain.ReasonModelBudgetTooHigh {
			t.Errorf("model_budget_usd %g: %v, want %s", over, err, domain.ReasonModelBudgetTooHigh)
		}
	}
	stated := 120.0
	pe, err := svc.Create(context.Background(), budgetRequest(&stated))
	if err != nil {
		t.Fatal(err)
	}
	if pe.ModelBudgetUSD == nil || *pe.ModelBudgetUSD != 120 {
		t.Fatalf("model_budget_usd = %v, want 120", pe.ModelBudgetUSD)
	}
	pe.Status = domain.PlatformExpOpen
	raised := 400.0
	if _, err := svc.Update(context.Background(), pe.ID, PlatformExperimentPatch{ModelBudgetUSD: &raised}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if *store.created.ModelBudgetUSD != 400 {
		t.Fatalf("raised to %v, want 400", *store.created.ModelBudgetUSD)
	}
	tooHigh := 501.0
	if _, err := svc.Update(context.Background(), pe.ID, PlatformExperimentPatch{ModelBudgetUSD: &tooHigh}); refusal.ReasonOf(err) != domain.ReasonModelBudgetTooHigh {
		t.Fatalf("a raise past the most: %v", err)
	}
}

// What a platform experiment spent is the sum of the costs recorded with its model calls, read
// in one query, against its own budget or the platform's default.
func TestAModelBudgetIsSpentByTheRecordedCosts(t *testing.T) {
	var mu sync.Mutex
	queries := 0
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[["agent-a","anthropic","model-default",3,1000001,1000000,1000000,10000000,18.5],["coordinator","openrouter","moonshotai/kimi-k2",1,0,500000,0,0,4.5]]}}]}`))
	}))
	t.Cleanup(metrics.Close)
	store := &createStore{created: &domain.PlatformExperiment{ID: "pe-1"}}
	svc := reaching(NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(metrics.URL), 3*time.Minute), domain.CPUType)
	budget, err := svc.ModelBudget(context.Background(), "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(budget.UsedUSD-(18.5+4.5)) > 1e-9 || budget.BudgetUSD != 50 || budget.Spent() || queries != 1 {
		t.Fatalf("budget = %+v after %d queries, want $23 of the default $50 from one", budget, queries)
	}
	usage, err := svc.ModelUsage(context.Background(), "pe-1")
	if err != nil || len(usage.Usage) != 2 || math.Abs(usage.Usage[0].CostUSD-18.5) > 1e-9 || usage.Usage[1].CostUSD != 4.5 {
		t.Fatalf("usage = %+v, %v, want each entry at its cost", usage, err)
	}
	stated := 20.0
	store.created.ModelBudgetUSD = &stated
	if budget, err = svc.ModelBudget(context.Background(), "pe-1"); err != nil || !budget.Spent() {
		t.Fatalf("budget = %+v, %v, want $23 of $20 spent", budget, err)
	}
}
