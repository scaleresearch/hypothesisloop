package platformexperiments

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"go.uber.org/zap"
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
func (s *Service) nextStage(ctx context.Context, pe *domain.PlatformExperiment) (*nextStage, error) {
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

// Stages is where the platform experiment id stands on its ladder: the boundaries crossed, the
// agents still in and those cut.
func (s *Service) Stages(ctx context.Context, id string) (*StagesStatus, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get stages", err, zap.String("id", id))
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	cuts, err := s.store.ListCutAgents(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get stages", err, zap.String("id", id))
	}
	if cuts == nil {
		cuts = []domain.AgentCut{}
	}
	cutSet := make(map[string]bool, len(cuts))
	for _, c := range cuts {
		cutSet[c.AgentID] = true
	}
	advances, err := s.store.ListStageAdvances(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get stages", err, zap.String("id", id))
	}
	if advances == nil {
		advances = []domain.StageAdvance{}
	}
	allSignups, err := s.store.ListParticipants(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get stages", err, zap.String("id", id))
	}
	activeAgents := []string{}
	for _, a := range allSignups {
		if !cutSet[a] {
			activeAgents = append(activeAgents, a)
		}
	}
	progress, err := s.stageProgress(ctx, pe)
	if err != nil {
		return nil, auth.Internal(s.logger, "get stages", err, zap.String("id", id))
	}
	return &StagesStatus{
		Stages:               pe.Stages,
		CurrentStage:         pe.CurrentStage,
		Progress:             progress,
		NextBoundaryProgress: domain.BoundaryProgress(pe.Stages, pe.CurrentStage),
		BoundariesProgress:   boundariesProgress(pe.Stages),
		Advances:             advances,
		ActiveAgents:         activeAgents,
		CutAgents:            cuts,
	}, nil
}

func boundariesProgress(stages []domain.Stage) []float64 {
	out := []float64{}
	for i := 1; i < len(stages); i++ {
		out = append(out, domain.BoundaryProgress(stages, i))
	}
	return out
}

// StagesStatus is the response body for GET /platform-experiments/{id}/stages.
// Boundaries are published ahead so agents can plan. The cut line is not here; the standings
// are public at GET /platform-experiments/{id}/results.
type StagesStatus struct {
	Stages       []domain.Stage `json:"stages"`
	CurrentStage int            `json:"current_stage"`
	// Progress is max(budget_consumed / total_budget, elapsed / duration) — the single clock
	// driving every boundary.
	Progress             float64   `json:"progress"`
	NextBoundaryProgress float64   `json:"next_boundary_progress"`
	BoundariesProgress   []float64 `json:"boundaries_progress" doc:"The progress at which each stage but the last ends, in order."`
	// Advances records each boundary already crossed and when.
	Advances     []domain.StageAdvance `json:"advances"`
	ActiveAgents []string              `json:"active_agents"`
	CutAgents    []domain.AgentCut     `json:"cut_agents"`
}
