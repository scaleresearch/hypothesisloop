package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func withData(agentID string, v float64) ranked {
	return ranked{agentID: agentID, value: v, hasData: true}
}
func noData(agentID string) ranked { return ranked{agentID: agentID} }

func assertCut(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("cut = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("cut = %v, want %v", got, want)
		}
	}
}

func TestCutBottomMaximize(t *testing.T) {
	order := []ranked{withData("a", 9), withData("b", 1), withData("c", 5), withData("d", 3), withData("e", 7), withData("f", 2)}
	// 6 survivors, 50% → cut the 3 lowest.
	assertCut(t, cutBottom(order, "maximize", 50), "b", "f", "d")
}

func TestCutBottomMinimize(t *testing.T) {
	order := []ranked{withData("a", 9), withData("b", 1), withData("c", 5), withData("d", 3), withData("e", 7), withData("f", 2)}
	// Lower is better, so the 3 highest go.
	assertCut(t, cutBottom(order, "minimize", 50), "a", "e", "c")
}

// A tie group straddling the line is kept whole — the cut takes fewer than k, never more.
func TestCutBottomTieGroupStraddlingLineIsKept(t *testing.T) {
	// 6 survivors, 50% → k=3, but ranks 2..4 (0-indexed) all sit at 5.
	order := []ranked{withData("a", 9), withData("b", 1), withData("c", 5), withData("d", 5), withData("e", 5), withData("f", 2)}
	assertCut(t, cutBottom(order, "maximize", 50), "b", "f")
}

// A tie group ending exactly on the line is cut in full — nothing straddles.
func TestCutBottomTieGroupEndingOnLineIsCut(t *testing.T) {
	order := []ranked{withData("a", 9), withData("b", 1), withData("c", 1), withData("d", 7), withData("e", 8), withData("f", 6)}
	// 34% of 6 → k=2, and the pair at 1 ends exactly there.
	assertCut(t, cutBottom(order, "maximize", 34), "b", "c")
}

// Agents with no data rank below every agent with data.
func TestCutBottomNoDataRanksLast(t *testing.T) {
	order := []ranked{withData("a", 9), noData("b"), withData("c", 5), withData("d", 3), withData("e", 7), withData("f", 2)}
	assertCut(t, cutBottom(order, "maximize", 50), "b", "f", "d")
}

// Every agent lacking data is one tie group, so a cut that would split it keeps all of them.
func TestCutBottomAllNoDataIsOneTieGroup(t *testing.T) {
	order := []ranked{noData("a"), noData("b"), noData("c"), noData("d"), noData("e"), noData("f")}
	assertCut(t, cutBottom(order, "maximize", 50))
}

// The floor is absolute: a cut may never take the field below two survivors.
func TestCutBottomHonoursSurvivorFloor(t *testing.T) {
	order := []ranked{withData("a", 1), withData("b", 2), withData("c", 3), withData("d", 4), withData("e", 5)}
	// 99% of 5 is 4, which would leave one agent; clamped to 3.
	cut := cutBottom(order, "maximize", 99)
	if len(cut) != 3 {
		t.Fatalf("cut %d agents, want 3 (floor of 2 survivors)", len(cut))
	}
	assertCut(t, cut, "a", "b", "c")
}

func TestCutBottomZeroEvictCutsNobody(t *testing.T) {
	order := []ranked{withData("a", 1), withData("b", 2), withData("c", 3), withData("d", 4), withData("e", 5)}
	assertCut(t, cutBottom(order, "maximize", 0))
}

func TestStageReleaseSplitsBudgetShareAndReclaimsUnspent(t *testing.T) {
	dim := db.StageRedistribution{
		ResourceType: domain.ResourceAcceleratorHours,
		Budget:       100,
		ReleaseFrac:  0.6,
		// cut2 overspent its guarantee — nothing to reclaim, and it must not subtract.
		UsedByAgent: map[string]float64{"cut1": 6, "cut2": 12},
	}
	allocated := map[string]float64{"cut1": 10, "cut2": 10}

	// 60% of a 100 AccH budget, plus cut1's 4 unspent hours.
	if got, want := db.StageReleaseTotal(dim, allocated), 64.0; got != want {
		t.Fatalf("release = %v, want %v", got, want)
	}
}

// A dimension the platform experiment does not budget releases nothing.
func TestStageReleaseSkipsUntrackedDimension(t *testing.T) {
	dim := db.StageRedistribution{ResourceType: domain.ResourceCPUCoreHours, Budget: 0, ReleaseFrac: 0.6}
	if got := db.StageReleaseTotal(dim, map[string]float64{}); got != 0 {
		t.Fatalf("release = %v, want 0 for an untracked dimension", got)
	}
}

// The whole ladder's releases must sum back to the total budget.
func TestStageReleasesSumToBudget(t *testing.T) {
	stages := []domain.Stage{{LengthPct: 20, EvictPct: 25}, {LengthPct: 30, EvictPct: 25}, {LengthPct: 50}}
	const budget = 1000.0
	released := budget * stages[0].LengthPct / 100 // allocated at Start
	for i := 1; i < len(stages); i++ {
		released += budget * stages[i].LengthPct / 100 // released at each boundary
	}
	if released != budget {
		t.Fatalf("released %v across the ladder, want %v", released, budget)
	}
}

// rolesStagesStore answers only the three reads currentSurvivors makes. Every other StagesStore
// method is inherited from the embedded nil interface, so a test that reaches one panics loudly
// instead of quietly getting a zero value back.
type rolesStagesStore struct {
	StagesStore
	quotaAgents []string
	competitors []string
	cut         []string
}

func (f *rolesStagesStore) ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	quotas := make([]*domain.AgentQuota, 0, len(f.quotaAgents))
	for _, agentID := range f.quotaAgents {
		quotas = append(quotas, &domain.AgentQuota{AgentID: agentID, PlatformExperimentID: platformExpID})
	}
	return quotas, nil
}

func (f *rolesStagesStore) ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error) {
	cuts := make([]domain.AgentCut, 0, len(f.cut))
	for _, agentID := range f.cut {
		cuts = append(cuts, domain.AgentCut{AgentID: agentID})
	}
	return cuts, nil
}

func (f *rolesStagesStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, fmt.Errorf("unexpected role read: %s", role)
	}
	return f.competitors, nil
}

// bestPerAgentServer answers BestPerAgentOnMetric's single PromQL query with one sample per
// agent, so a cut can be computed against real values rather than a stubbed ranking.
func bestPerAgentServer(t *testing.T, values map[string]float64) *httptest.Server {
	t.Helper()
	results := make([]string, 0, len(values))
	for agentID, v := range values {
		results = append(results, fmt.Sprintf(
			`{"metric":{"agent_id":%q,"metric_basis":"raw","job_id":"job-%s"},"value":[1,"%v"]}`, agentID, agentID, v))
	}
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(results, ","))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func rolesController(store *rolesStagesStore, metricsURL string) *Controller {
	return &Controller{stagesStore: store, metricsDBURL: metricsURL}
}

func rankedPE(evictPct float64) (*domain.PlatformExperiment, domain.Stage) {
	pe := &domain.PlatformExperiment{
		ID:        "pe-roles",
		Metrics:   []domain.MetricDefinition{{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking}},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	return pe, domain.Stage{LengthPct: 100, EvictPct: evictPct}
}

// A baseline or a reviewer holds quota and runs jobs exactly like a competitor, so the only thing
// keeping it out of every ranking derived from the ladder is that it is not a survivor.
func TestCurrentSurvivorsExcludesAgentsSignedUpInANonCompetitorRole(t *testing.T) {
	store := &rolesStagesStore{
		quotaAgents: []string{"a", "b", "baseline", "reviewer"},
		competitors: []string{"a", "b"},
	}
	got, err := rolesController(store, "").currentSurvivors(context.Background(), &domain.PlatformExperiment{ID: "pe-roles"})
	if err != nil {
		t.Fatalf("currentSurvivors err = %v, want nil", err)
	}
	assertCut(t, got, "a", "b")
}

// A cut at a boundary must be taken from the competitors alone: a non-competitor is never
// eliminated, and its presence must not enlarge the field the evict percentage is applied to.
func TestCutWithFiveCompetitorsAndTwoNonCompetitorsCutsOnlyFromTheFive(t *testing.T) {
	// The two non-competitors sit at the very bottom on the metric — exactly where a cut would
	// take them if roles were ignored.
	srv := bestPerAgentServer(t, map[string]float64{
		"c1": 50, "c2": 40, "c3": 30, "c4": 20, "c5": 10, "baseline": 1, "reviewer": 2,
	})
	defer srv.Close()
	store := &rolesStagesStore{
		quotaAgents: []string{"c1", "c2", "c3", "c4", "c5", "baseline", "reviewer"},
		competitors: []string{"c1", "c2", "c3", "c4", "c5"},
	}
	pe, stage := rankedPE(30) // 30% of 5 competitors floors to 1; of all 7 it would floor to 2.
	kept, cut, err := rolesController(store, srv.URL).computeCut(context.Background(), pe, stage)
	if err != nil {
		t.Fatalf("computeCut err = %v, want nil", err)
	}
	assertCut(t, cut, "c5")
	assertCut(t, kept, "c1", "c2", "c3", "c4")
}

// minSurvivorsForCut counts the field actually being compared. Letting non-competitors pad the
// roster past the guardrail would start eliminating agents from a field too small to compare.
func TestBoundaryWithFourCompetitorsAndThreeNonCompetitorsCutsNobody(t *testing.T) {
	store := &rolesStagesStore{
		quotaAgents: []string{"c1", "c2", "c3", "c4", "b1", "b2", "r1"},
		competitors: []string{"c1", "c2", "c3", "c4"},
	}
	pe, stage := rankedPE(50)
	// metricsDBURL is empty on purpose: reaching a metric read at all would mean the guardrail
	// counted seven agents and let the boundary through.
	kept, cut, err := rolesController(store, "").computeCut(context.Background(), pe, stage)
	if err != nil {
		t.Fatalf("computeCut err = %v, want nil", err)
	}
	if len(cut) != 0 {
		t.Fatalf("cut = %v, want %v", cut, []string{})
	}
	assertCut(t, kept, "c1", "c2", "c3", "c4")
}
