package domain

import (
	"testing"
	"time"
)

func TestValidateStages(t *testing.T) {
	cases := []struct {
		name    string
		stages  []Stage
		wantErr bool
	}{
		{"two stages", []Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}}, false},
		{"single stage no cut", []Stage{{LengthPct: 100, EvictPct: 0}}, false},
		{"three stages", []Stage{{LengthPct: 20, EvictPct: 25}, {LengthPct: 30, EvictPct: 25}, {LengthPct: 50, EvictPct: 0}}, false},
		{"max stages", []Stage{{LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 30, EvictPct: 0}}, false},
		{"empty", nil, true},
		{"too many stages", []Stage{{LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 10, EvictPct: 5}, {LengthPct: 20, EvictPct: 0}}, true},
		{"lengths under 100", []Stage{{LengthPct: 40, EvictPct: 25}, {LengthPct: 50, EvictPct: 0}}, true},
		{"lengths over 100", []Stage{{LengthPct: 40, EvictPct: 25}, {LengthPct: 70, EvictPct: 0}}, true},
		{"zero-length stage", []Stage{{LengthPct: 0, EvictPct: 25}, {LengthPct: 100, EvictPct: 0}}, true},
		{"negative length", []Stage{{LengthPct: -20, EvictPct: 0}, {LengthPct: 120, EvictPct: 0}}, true},
		{"negative evict", []Stage{{LengthPct: 40, EvictPct: -1}, {LengthPct: 60, EvictPct: 0}}, true},
		{"evict 100 impossible", []Stage{{LengthPct: 40, EvictPct: 100}, {LengthPct: 60, EvictPct: 0}}, true},
		{"cut on final stage", []Stage{{LengthPct: 40, EvictPct: 25}, {LengthPct: 60, EvictPct: 10}}, true},
		{"single stage with cut", []Stage{{LengthPct: 100, EvictPct: 50}}, true},
		{"job length cap", []Stage{{LengthPct: 40, EvictPct: 25, MaxJobHours: 0.5}, {LengthPct: 60, EvictPct: 0}}, false},
		{"negative job length cap", []Stage{{LengthPct: 40, EvictPct: 25, MaxJobHours: -1}, {LengthPct: 60, EvictPct: 0}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStages(tc.stages)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Percentages written as thirds don't sum to exactly 100 in float64; the tolerance must accept
// them, since an operator writing 33.33/33.33/33.34 means 100.
func TestValidateStagesFloatSum(t *testing.T) {
	if err := ValidateStages([]Stage{{LengthPct: 33.33, EvictPct: 25}, {LengthPct: 33.33, EvictPct: 25}, {LengthPct: 33.34, EvictPct: 0}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBoundaryProgress(t *testing.T) {
	stages := []Stage{{LengthPct: 20, EvictPct: 25}, {LengthPct: 30, EvictPct: 25}, {LengthPct: 50, EvictPct: 0}}
	for _, tc := range []struct {
		stageIndex int
		want       float64
	}{{0, 0}, {1, 0.20}, {2, 0.50}, {3, 1.0}} {
		if got := BoundaryProgress(stages, tc.stageIndex); got != tc.want {
			t.Errorf("BoundaryProgress(%d) = %v, want %v", tc.stageIndex, got, tc.want)
		}
	}
}

// When a stage ends and what its end gives were worked out apart by the submit refusal, the
// quota read and the boundary itself.
func TestAStageBoundaryIsWhenItFallsAndWhatItGivesEachSurvivor(t *testing.T) {
	started := time.Date(2026, 10, 6, 21, 0, 0, 0, time.UTC)
	pe := &PlatformExperiment{StartedAt: started, EndsAt: started.Add(45 * time.Minute),
		Stages: []Stage{{LengthPct: 40, EvictPct: 50}, {LengthPct: 60}}}
	if got, want := pe.StageEndsAt(1), started.Add(18*time.Minute); !got.Equal(want) {
		t.Errorf("stage 1 ends at %s, want %s: 40%% of 45 minutes", got, want)
	}
	if got := pe.StageEndsAt(2); !got.Equal(pe.EndsAt) {
		t.Errorf("the last stage ends at %s, want the run's end", got)
	}
	if got := (&PlatformExperiment{Stages: pe.Stages}).StageEndsAt(1); !got.IsZero() {
		t.Errorf("before the clock started: stage 1 ends at %s, want no moment", got)
	}

	// 60% of 6 hours over 2 survivors, and with it the 4 hours one cut agent left of its 10.
	if got := StageCredit(6, pe.Stages[1].ReleaseFrac(), nil, nil, 2); got != 1.8 {
		t.Errorf("credit = %v, want 1.8", got)
	}
	allocated, used := map[string]float64{"cut1": 10, "cut2": 10}, map[string]float64{"cut1": 6, "cut2": 12}
	if got := StageCredit(6, pe.Stages[1].ReleaseFrac(), allocated, used, 2); got != 3.8 {
		t.Errorf("credit with a cut agent's unspent hours = %v, want 3.8: an agent that overspent takes nothing back", got)
	}
}

func TestStageProgress(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Hour)

	// Budget ahead of the clock.
	if got := StageProgress(60, 100, start, end, start.Add(time.Hour)); got != 0.6 {
		t.Errorf("budget-driven progress = %v, want 0.6", got)
	}
	// Clock ahead of the budget — an idle experiment still advances.
	if got := StageProgress(0, 100, start, end, start.Add(8*time.Hour)); got != 0.8 {
		t.Errorf("clock-driven progress = %v, want 0.8", got)
	}
	// A platform experiment with no accelerator budget still advances on the clock alone.
	if got := StageProgress(0, 0, start, end, start.Add(5*time.Hour)); got != 0.5 {
		t.Errorf("no-budget progress = %v, want 0.5", got)
	}
	// Past ends_at, or over budget, the experiment is through and no further.
	if got := StageProgress(0, 100, start, end, start.Add(48*time.Hour)); got != 1 {
		t.Errorf("progress past ends_at = %v, want 1", got)
	}
	if got := StageProgress(150, 100, start, end, start.Add(time.Hour)); got != 1 {
		t.Errorf("over-budget progress = %v, want 1", got)
	}
}

// Progress must never go backwards across a restart: re-deriving it from the same stored state
// yields the same stage, so a boundary once crossed stays crossed.
func TestStageProgressMonotonic(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Hour)
	prev := 0.0
	for h := 0; h <= 10; h++ {
		// Consumption only ever grows, and so does elapsed time.
		got := StageProgress(float64(h*5), 100, start, end, start.Add(time.Duration(h)*time.Hour))
		if got < prev {
			t.Fatalf("progress went backwards at hour %d: %v < %v", h, got, prev)
		}
		prev = got
	}
}

// A platform experiment created with the zero starts_at must not read as ~100% elapsed.
func TestStageProgressIgnoresZeroBounds(t *testing.T) {
	real := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if got := StageProgress(10, 100, time.Time{}, real, real); got != 0.1 {
		t.Errorf("zero starts_at progress = %v, want 0.1 (budget only)", got)
	}
	if got := StageProgress(10, 100, real, time.Time{}, real); got != 0.1 {
		t.Errorf("zero ends_at progress = %v, want 0.1 (budget only)", got)
	}
	if got := StageProgress(10, 100, time.Time{}, time.Time{}, real); got != 0.1 {
		t.Errorf("both-zero progress = %v, want 0.1 (budget only)", got)
	}
}

// The job-length cap in force is the current stage's, and an out-of-range CurrentStage (a
// finished or not-yet-started ladder) means no cap rather than a panic.
func TestCurrentMaxJobHours(t *testing.T) {
	pe := &PlatformExperiment{
		Stages:       []Stage{{LengthPct: 40, EvictPct: 25, MaxJobHours: 2}, {LengthPct: 60, EvictPct: 0}},
		CurrentStage: 1,
	}
	if got := pe.CurrentMaxJobHours(); got != 2 {
		t.Errorf("stage 1 cap = %v, want 2", got)
	}
	pe.CurrentStage = 2
	if got := pe.CurrentMaxJobHours(); got != 0 {
		t.Errorf("stage 2 cap = %v, want 0 (unlimited)", got)
	}
	for _, idx := range []int{0, 3} {
		pe.CurrentStage = idx
		if got := pe.CurrentMaxJobHours(); got != 0 {
			t.Errorf("out-of-range stage %d cap = %v, want 0", idx, got)
		}
	}
}

func TestRunStagesIsOneStageWhereNoBoundaryCanCut(t *testing.T) {
	ladder := []Stage{{LengthPct: 40, EvictPct: 75, MaxJobHours: 1}, {LengthPct: 60, MaxJobHours: 6}}
	for researchers := 0; researchers < MinResearchersForCut; researchers++ {
		got := RunStages(ladder, researchers)
		if len(got) != 1 || got[0] != (Stage{LengthPct: 100, MaxJobHours: 6}) {
			t.Errorf("%d researchers: stages %+v, want one stage of the whole run with the last stage's job cap", researchers, got)
		}
		if err := ValidateStages(got); err != nil {
			t.Errorf("%d researchers: %v", researchers, err)
		}
	}
	if got := RunStages(ladder, MinResearchersForCut); len(got) != 2 || got[0] != ladder[0] || got[1] != ladder[1] {
		t.Errorf("%d researchers: stages %+v, want the ladder as declared", MinResearchersForCut, got)
	}
}
