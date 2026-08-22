package controller

import (
	"sort"
	"testing"

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
