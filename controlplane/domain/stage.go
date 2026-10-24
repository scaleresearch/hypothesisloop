package domain

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

var ErrInvalidStages = experimentspec.ErrInvalidStages

const MaxStages = experimentspec.MaxStages

// Stage is one rung of the elimination ladder (experimentspec.Stage).
type Stage = experimentspec.Stage

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

func ValidateStages(stages []Stage) error { return experimentspec.ValidateStages(stages) }

const MinExecutorsForCut = experimentspec.MinExecutorsForCut

// RunStages is the ladder a run of executors is started with. A field no boundary
// can cut from has one stage, so each executor holds its whole share from the start: staged,
// the ladder only withheld budget and selected nobody. The one stage keeps the job-length cap of
// the ladder's last, the stage that follows every cut. The field only shrinks after the start,
// so what is decided here holds for the whole run.
func RunStages(stages []Stage, executors int) []Stage {
	if executors >= MinExecutorsForCut || len(stages) == 0 {
		return stages
	}
	return []Stage{{LengthPct: 100, MaxJobHours: stages[len(stages)-1].MaxJobHours}}
}

// StageProgress is max(budget consumed, time elapsed of the run) as a fraction, at most 1 — the single monotonic
// clock driving every boundary, so whichever runs out first advances the ladder. consumedAccH
// must exclude reservations, or a large queued job could trip a boundary.
func StageProgress(consumedAccH, budgetAccH float64, startedAt, endsAt, now time.Time) float64 {
	var budgetProgress float64
	if budgetAccH > 0 {
		budgetProgress = consumedAccH / budgetAccH
	}
	// Both ends must be real for the clock to mean anything: one that has not started has neither.
	var timeProgress float64
	if window := endsAt.Sub(startedAt); window > 0 && !startedAt.IsZero() && !endsAt.IsZero() {
		timeProgress = now.Sub(startedAt).Seconds() / window.Seconds()
	}
	return min(1, max(timeProgress, budgetProgress))
}

// CurrentMaxJobHours is the job-length cap in force right now: the MaxJobHours of the stage
// currently running, 0 meaning unlimited. The cap follows the ladder rather than being frozen
// onto a job at submission — a job outliving the stage that admitted it is exactly the
// long-run-during-exploration case the cap exists to prevent.
func (p *PlatformExperiment) CurrentMaxJobHours() float64 {
	stage, ok := p.CurrentStageDef()
	if !ok {
		return 0
	}
	return stage.MaxJobHours
}

// CurrentStageDef is the stage CurrentStage names, ok=false when the row's current_stage does not
// address one. The bounds check lives here rather than at each call site: current_stage is a
// stored integer, and an out-of-range one used to panic the reconcile loop on every tick.
func (p *PlatformExperiment) CurrentStageDef() (Stage, bool) {
	if p.CurrentStage < 1 || p.CurrentStage > len(p.Stages) {
		return Stage{}, false
	}
	return p.Stages[p.CurrentStage-1], true
}

// StageEndsAt is the latest moment stage index (1-based) of the run ends, by the clock: the
// boundary's share of the run past the run's start. It ends sooner once the same share of the
// budget is spent (StageProgress). Zero while the run's clock has not started.
func (p *PlatformExperiment) StageEndsAt(stageIndex int) time.Time {
	if p.StartedAt.IsZero() {
		return time.Time{}
	}
	return p.StartedAt.Add(time.Duration(BoundaryProgress(p.Stages, stageIndex) * float64(p.EndsAt.Sub(p.StartedAt)))).UTC()
}

// StageCredit is what each of survivors is given when a stage starts at a boundary: the stage's
// part of budget, releaseFrac of it, plus what the agents cut there left unspent of what they
// were allocated, split evenly. An agent that overspent contributes nothing rather than a
// negative. The boundary credits it and a quota read promises it, from here alone.
func StageCredit(budget, releaseFrac float64, allocatedByCut, usedByCut map[string]float64, survivors int) float64 {
	release := budget * releaseFrac
	for agentID, allocated := range allocatedByCut {
		if unspent := allocated - usedByCut[agentID]; unspent > 0 {
			release += unspent
		}
	}
	return RoundHours(release / float64(survivors))
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
