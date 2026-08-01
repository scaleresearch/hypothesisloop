package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidStages wraps every ValidateStages failure so callers can map a malformed ladder to
// a client error instead of a server fault.
var ErrInvalidStages = errors.New("stages")

// MaxStages bounds a ladder. Eight is far more than any experiment needs and keeps the
// per-boundary bookkeeping (one advance row, one cut set) trivially small.
const MaxStages = 8

// Stage is one rung of the elimination ladder: LengthPct is its share of the experiment (all
// stages sum to 100), EvictPct the share of surviving agents cut when it ends. See
// docs/stages.md.
type Stage struct {
	LengthPct float64 `json:"length_pct" yaml:"length_pct"`
	EvictPct  float64 `json:"evict_pct" yaml:"evict_pct"`
}

// DefaultStages is the ladder used when a platform experiment supplies none.
var DefaultStages = []Stage{
	{LengthPct: 40, EvictPct: 75},
	{LengthPct: 60, EvictPct: 0},
}

// AgentCut records that an agent was cut at the end of stage StageIndex (1-based).
type AgentCut struct {
	AgentID    string `json:"agent_id"`
	StageIndex int    `json:"stage_index"`
}

// StageAdvance records that a boundary was crossed: the stage that ended and when.
type StageAdvance struct {
	StageIndex int       `json:"stage_index"`
	AdvancedAt time.Time `json:"advanced_at"`
}

// ValidateStages enforces docs/stages.md at creation, so nothing downstream defends against a
// malformed ladder.
func ValidateStages(stages []Stage) error {
	if len(stages) == 0 || len(stages) > MaxStages {
		return fmt.Errorf("%w: need 1..%d stages, got %d", ErrInvalidStages, MaxStages, len(stages))
	}
	var total float64
	for i, s := range stages {
		if s.LengthPct <= 0 {
			return fmt.Errorf("%w: stage %d length_pct must be > 0, got %g", ErrInvalidStages, i+1, s.LengthPct)
		}
		if s.EvictPct < 0 || s.EvictPct >= 100 {
			return fmt.Errorf("%w: stage %d evict_pct must be in [0, 100), got %g", ErrInvalidStages, i+1, s.EvictPct)
		}
		total += s.LengthPct
	}
	// Float sum of human-written percentages — a tolerance, not an exact compare.
	if total < 99.999 || total > 100.001 {
		return fmt.Errorf("%w: length_pct must sum to 100, got %g", ErrInvalidStages, total)
	}
	// Nothing follows the final stage, so a cut there would only strip agents at the moment
	// the experiment ends — it changes no outcome and no budget.
	if last := stages[len(stages)-1]; last.EvictPct != 0 {
		return fmt.Errorf("%w: final stage must have evict_pct 0, got %g", ErrInvalidStages, last.EvictPct)
	}
	return nil
}

// StageProgress is max(budget consumed, time elapsed) as a fraction — the single monotonic
// clock driving every boundary, so whichever runs out first advances the ladder. consumedAccH
// must exclude reservations, or a large queued job could trip a boundary.
func StageProgress(consumedAccH, budgetAccH float64, startsAt, endsAt, now time.Time) float64 {
	var budgetProgress float64
	if budgetAccH > 0 {
		budgetProgress = consumedAccH / budgetAccH
	}
	// Both ends must be real for the clock to mean anything. A platform experiment created with
	// the zero starts_at ("start immediately") and a real ends_at would otherwise measure elapsed
	// time from year 1 and read as ~100% done on its first tick.
	var timeProgress float64
	if window := endsAt.Sub(startsAt); window > 0 && !startsAt.IsZero() && !endsAt.IsZero() {
		timeProgress = now.Sub(startsAt).Seconds() / window.Seconds()
	}
	if timeProgress > budgetProgress {
		return timeProgress
	}
	return budgetProgress
}

// BoundaryProgress returns the progress fraction (0..1) at which stage index (1-based) ends.
// The final stage's boundary is 1.0 — the end of the experiment, where no cut can happen.
func BoundaryProgress(stages []Stage, stageIndex int) float64 {
	var cum float64
	for i := 0; i < stageIndex && i < len(stages); i++ {
		cum += stages[i].LengthPct
	}
	return cum / 100.0
}
