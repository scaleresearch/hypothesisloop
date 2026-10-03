package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
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
	if got, want := domain.StageCredit(dim.Budget, dim.ReleaseFrac, allocated, dim.UsedByAgent, 1), 64.0; got != want {
		t.Fatalf("release = %v, want %v", got, want)
	}
}

// A dimension the platform experiment does not budget releases nothing.
func TestStageReleaseSkipsUntrackedDimension(t *testing.T) {
	dim := db.StageRedistribution{ResourceType: domain.ResourceAcceleratorHours, Budget: 0, ReleaseFrac: 0.6}
	if got := domain.StageCredit(dim.Budget, dim.ReleaseFrac, nil, nil, 1); got != 0 {
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
	completedBy []string
	inFlightBy  []string
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

func (f *rolesStagesStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

func (f *rolesStagesStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	agents := make(map[string]db.AgentJobs)
	for _, agentID := range f.completedBy {
		jobs := agents[agentID]
		jobs.Completed = true
		agents[agentID] = jobs
	}
	for _, agentID := range f.inFlightBy {
		jobs := agents[agentID]
		jobs.InFlight = true
		agents[agentID] = jobs
	}
	return agents, nil
}

func (f *rolesStagesStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, fmt.Errorf("unexpected role read: %s", role)
	}
	return f.competitors, nil
}

// bestPerAgent is the per-agent standing the ladder cuts on, so a cut can be computed against
// real values rather than a stubbed ranking.
func bestPerAgent(values map[string]float64) fakeObserved {
	best := make(map[string]metricsdb.AgentBest, len(values))
	for agentID, v := range values {
		best[agentID] = metricsdb.AgentBest{Value: v, ExperimentID: "job-" + agentID}
	}
	return fakeObserved{best: best}
}

// rolesController takes an ObservedState rather than a URL, so a test that must prove no metric
// was read can pass nil: reaching a metric read then panics.
func rolesController(store *rolesStagesStore, observed ObservedState) *Controller {
	return &Controller{stagesStore: store, observed: observed, logger: zap.NewNop()}
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
	got, err := rolesController(store, nil).currentSurvivors(context.Background(), &domain.PlatformExperiment{ID: "pe-roles"})
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
	observed := bestPerAgent(map[string]float64{
		"c1": 50, "c2": 40, "c3": 30, "c4": 20, "c5": 10, "baseline": 1, "reviewer": 2,
	})
	store := &rolesStagesStore{
		quotaAgents: []string{"c1", "c2", "c3", "c4", "c5", "baseline", "reviewer"},
		competitors: []string{"c1", "c2", "c3", "c4", "c5"},
		completedBy: []string{"c1", "c2", "c3", "c4", "c5", "baseline", "reviewer"},
	}
	pe, stage := rankedPE(30) // 30% of 5 competitors floors to 1; of all 7 it would floor to 2.
	kept, cut, err := rolesController(store, observed).computeCut(context.Background(), pe, stage)
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
	// No metrics store on purpose: reaching a metric read at all would mean the guardrail counted
	// seven agents and let the boundary through.
	kept, cut, err := rolesController(store, nil).computeCut(context.Background(), pe, stage)
	if err != nil {
		t.Fatalf("computeCut err = %v, want nil", err)
	}
	if len(cut) != 0 {
		t.Fatalf("cut = %v, want %v", cut, []string{})
	}
	assertCut(t, kept, "c1", "c2", "c3", "c4")
}

// With no ranking value for anyone standing, the boundary waits only for what can still produce
// one: the metrics store coming back, or a job in flight in a field where none has completed.
// Everything else resolves at once with the field whole.
func TestBoundaryWithNoRankingValueWaitsOnlyForWhatCanStillRank(t *testing.T) {
	field := []string{"c1", "c2", "c3", "c4", "c5"}
	cases := []struct {
		name        string
		completedBy []string
		inFlightBy  []string
		observed    fakeObserved
		// wantPending is the postponement for a job in flight, wantStoreErr the failed pass that
		// retries the read.
		wantPending  bool
		wantStoreErr bool
	}{
		{
			name: "every job was stopped short: advance",
			// A non-competitor's jobs give the survivors nothing to be ranked on or to wait for.
			completedBy: []string{"baseline"},
			inFlightBy:  []string{"baseline"},
		},
		{
			name:        "nobody completed and a job is in flight: postpone",
			inFlightBy:  []string{"c2"},
			wantPending: true,
		},
		{
			name:        "completed jobs reported no ranking value: advance",
			completedBy: []string{"c3"},
		},
		{
			name:        "completed without a value beside a job in flight: advance",
			completedBy: []string{"c3"},
			inFlightBy:  []string{"c4"},
		},
		{
			name:         "the metrics store fails with nothing left to rank: retry",
			observed:     fakeObserved{failOn: "BestPerAgentOnMetric"},
			wantStoreErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &rolesStagesStore{
				quotaAgents: append([]string{"baseline"}, field...),
				competitors: field,
				completedBy: tc.completedBy,
				inFlightBy:  tc.inFlightBy,
			}
			pe, stage := rankedPE(50)
			kept, cut, err := rolesController(store, tc.observed).computeCut(context.Background(), pe, stage)
			if tc.wantPending || tc.wantStoreErr {
				if err == nil || errors.Is(err, ErrStageRankingPending) != tc.wantPending {
					t.Fatalf("computeCut err = %v, want pending=%v store error=%v", err, tc.wantPending, tc.wantStoreErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("computeCut err = %v, want nil", err)
			}
			if len(cut) != 0 {
				t.Fatalf("cut = %v, want %v", cut, []string{})
			}
			assertCut(t, kept, field...)
		})
	}
}

// A ranking value settles the boundary whatever else is still running: the field is cut as it
// stands, and the job in flight is not waited for.
func TestBoundaryWithARankingValueCutsWhileOtherJobsAreInFlight(t *testing.T) {
	observed := bestPerAgent(map[string]float64{"c1": 50, "c2": 40})
	store := &rolesStagesStore{
		quotaAgents: []string{"c1", "c2", "c3", "c4", "c5"},
		competitors: []string{"c1", "c2", "c3", "c4", "c5"},
		completedBy: []string{"c1", "c2"},
		inFlightBy:  []string{"c3", "c4", "c5"},
	}
	pe, stage := rankedPE(60)
	kept, cut, err := rolesController(store, observed).computeCut(context.Background(), pe, stage)
	if err != nil {
		t.Fatalf("computeCut err = %v, want nil", err)
	}
	assertCut(t, cut, "c3", "c4", "c5")
	assertCut(t, kept, "c1", "c2")
}
