package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// nextStage is the boundary a running platform experiment has ahead: when the stage after the
// current one starts and what the start gives each researcher still standing.
type nextStage struct {
	startsAt time.Time
	credit   float64
	standing map[string]bool
}

// nextStageRelease is when the stage after pe's current one starts by the clock and what each of
// standing researchers is given of runBudget then, as the boundary releases it
// (controller.applyCut): the incoming stage's part of the budget, split evenly. ok is false when
// no boundary is ahead: pe is not running, its clock has not started, it is in its last stage, or
// nobody stands.
func nextStageRelease(pe *domain.PlatformExperiment, runBudget float64, standing int) (startsAt time.Time, credit float64, ok bool) {
	if pe.Status != domain.PlatformExpRunning || pe.CurrentStage < 1 || pe.CurrentStage >= len(pe.Stages) || standing == 0 {
		return time.Time{}, 0, false
	}
	if startsAt = pe.StageEndsAt(pe.CurrentStage); startsAt.IsZero() {
		return time.Time{}, 0, false
	}
	return startsAt, domain.StageCredit(runBudget, pe.Stages[pe.CurrentStage].ReleaseFrac(), nil, nil, standing), true
}

// nextStage reads the boundary ahead of pe, nil when there is none.
func (s *PlatformExperimentsService) nextStage(ctx context.Context, pe *domain.PlatformExperiment) (*nextStage, error) {
	if pe.Status != domain.PlatformExpRunning || pe.CurrentStage >= len(pe.Stages) {
		return nil, nil
	}
	competitors, err := s.store.ListSignupsByRole(ctx, pe.ID, domain.SignupRoleCompetitor)
	if err != nil {
		return nil, fmt.Errorf("next stage: %w", err)
	}
	cuts, err := s.store.ListCutAgents(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("next stage: %w", err)
	}
	standing := make(map[string]bool, len(competitors))
	for _, agentID := range competitors {
		standing[agentID] = true
	}
	for _, cut := range cuts {
		delete(standing, cut.AgentID)
	}
	runBudget, err := s.RunBudget(ctx, pe)
	if err != nil {
		return nil, fmt.Errorf("next stage: %w", err)
	}
	startsAt, credit, ok := nextStageRelease(pe, runBudget, len(standing))
	if !ok {
		return nil, nil
	}
	return &nextStage{startsAt: startsAt, credit: credit, standing: standing}, nil
}
