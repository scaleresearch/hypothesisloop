package experimentspec

import (
	"errors"
	"fmt"
)

// ErrInvalidStages wraps every ValidateStages failure so callers can map a malformed ladder to
// a client error instead of a server fault.
var ErrInvalidStages = errors.New("stages")

// MaxStages bounds a ladder. Eight is far more than any experiment needs and keeps the
// per-boundary bookkeeping (one advance row, one cut set) trivially small.
const MaxStages = 8

// MinResearchersForCut is the field size below which a boundary cuts nobody: a ladder that
// eliminates its way down to one agent has stopped comparing anything.
const MinResearchersForCut = 5

// Stage is one rung of the elimination ladder: LengthPct is its share of the experiment (all
// stages sum to 100), EvictPct the share of surviving agents cut when it ends.
type Stage struct {
	LengthPct float64 `json:"length_pct" yaml:"length_pct"`
	EvictPct  float64 `json:"evict_pct" yaml:"evict_pct"`
	// MaxJobHours caps how long a single job may run while this stage is current. Zero means
	// unlimited. A short cap on early stages pushes agents to explore with many small runs and
	// only commit to long ones once a stage without a cap is reached.
	MaxJobHours float64 `json:"max_job_hours,omitempty" yaml:"max_job_hours"`
}

// ReleaseFrac is the part of the run's budget the stage releases when it starts: its share of
// the run's length.
func (s Stage) ReleaseFrac() float64 {
	return s.LengthPct / 100
}

// ValidateStages enforces the stage ladder at creation, so nothing downstream defends against a
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
		if s.MaxJobHours < 0 {
			return fmt.Errorf("%w: stage %d max_job_hours must be >= 0 (0 = unlimited), got %g", ErrInvalidStages, i+1, s.MaxJobHours)
		}
		total += s.LengthPct
	}
	// Float sum of human-written percentages: a tolerance and never an exact compare.
	if total < 99.999 || total > 100.001 {
		return fmt.Errorf("%w: length_pct must sum to 100, got %g", ErrInvalidStages, total)
	}
	// Nothing follows the final stage, so a cut there would only strip agents at the moment
	// the experiment ends: it changes no outcome and no budget.
	if last := stages[len(stages)-1]; last.EvictPct != 0 {
		return fmt.Errorf("%w: final stage must have evict_pct 0, got %g", ErrInvalidStages, last.EvictPct)
	}
	return nil
}
