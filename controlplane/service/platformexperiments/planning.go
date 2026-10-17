package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Planning bounds a draft, which the platform pays for: how long it is planned and waits.
type Planning struct {
	// Limit is how long a planner has to propose, from the goal or from a changed one.
	Limit time.Duration
	// ProposedFor is how long a proposal waits for the person before the draft is closed.
	ProposedFor time.Duration
	// PlannerKept is how long a planner's machine is kept after its answer, for a goal changed in
	// that time to reach the planner that is still there.
	PlannerKept time.Duration
	// AutoAccept is how long a proposal waits before the platform accepts it as proposed, unless
	// a person accepts it sooner, holds it, changes its goal or closes it. 0 leaves it to a person.
	AutoAccept time.Duration
}

// checkGoal is goal as it is stored, or why it is refused.
func checkGoal(goal string) (string, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", refuseUpdate(422, "invalid_goal", "goal is required: one sentence saying what you want to find out")
	}
	if len(goal) > domain.MaxGoalBytes {
		return "", refuseUpdate(422, "invalid_goal", "goal is %d bytes and at most %d: say it in a sentence or two, the planner works out the rest", len(goal), domain.MaxGoalBytes)
	}
	return goal, nil
}

// DraftNameLength bounds the name a draft takes from its goal until the proposal names it.
const DraftNameLength = 80

// draftName is a goal cut to a name at a word.
func draftName(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	if utf8.RuneCountInString(goal) <= DraftNameLength {
		return goal
	}
	runes := []rune(goal)[:DraftNameLength]
	if cut := strings.LastIndexByte(string(runes), ' '); cut > DraftNameLength/2 {
		return string(runes)[:cut] + "…"
	}
	return string(runes) + "…"
}

// CreateDraft creates a platform experiment in status draft from one sentence, with its planner:
// an agent the platform runs, of role planner. The draft has the planning limit for an end and nothing of a run, no budget among it:
// the planner proposes the rest and a person accepts it. A repository to be created is created
// after this returns, by the repository pass it wakes (ProvideRepositories). repository and account
// say where it keeps its code, as on a create.
func (s *Service) CreateDraft(ctx context.Context, projectID, goal, continues, repository, account string, by domain.Actor) (*domain.PlatformExperiment, error) {
	named, err := s.repositoryOf(ctx, projectID, repository, account)
	if err != nil {
		return nil, err
	}
	choice, err := s.chooseRepository(ctx, projectID, named, account)
	if err != nil {
		return nil, err
	}
	pe, err := s.createDraft(ctx, projectID, goal, continues, by, choice)
	var answered interface{ GetStatus() int }
	if errors.As(err, &answered) {
		return nil, err
	}
	if err != nil {
		return nil, s.refusal("create draft", err)
	}
	return pe, nil
}

func (s *Service) createDraft(ctx context.Context, projectID, goal, continues string, by domain.Actor, repository RepositoryChoice) (*domain.PlatformExperiment, error) {
	planning, launch := s.planning, s.launch
	goal, err := checkGoal(goal)
	if err != nil {
		return nil, err
	}
	if launch.DefaultModel == "" {
		return nil, errors.New("platform_experiments.CreateDraft: no agent launch settings, and a draft is planned by an agent the platform runs")
	}
	if MaxProposedResearchers(launch.MaxHosted) < 1 {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: the platform runs %d machines for one platform experiment (agents.max_hosted_per_platform_experiment), and an accepted draft takes three at least: its coordinator, its baseline agent and one researcher", launch.MaxHosted)
	}
	if err := requireHosting(launch.Hosting); err != nil {
		return nil, err
	}
	if err := s.requireContinuable(ctx, projectID, repository.Named, continues); err != nil {
		return nil, err
	}
	plannerID, err := domain.NewID(domain.AgentIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	id, err := domain.NewID(domain.PlatformExperimentIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	now := time.Now().UTC()
	// A draft states nothing of a run yet, so its ladder and its policies are the platform's.
	settings := s.withSettings(experimentspec.Spec{})
	pe := &domain.PlatformExperiment{
		ID:                    id,
		ProjectID:             projectID,
		Name:                  draftName(goal),
		Goal:                  goal,
		GoalVersion:           1,
		Continues:             continues,
		Actor:                 &by,
		MaxAgents:             MaxProposedResearchers(launch.MaxHosted),
		Metrics:               []domain.MetricDefinition{},
		ReportIntervalSeconds: settings.ReportIntervalSeconds,
		PlanningEndsAt:        now.Add(planning.Limit),
		Status:                domain.PlatformExpDraft,
		Stages:                settings.Stages,
		CurrentStage:          1,
		DefaultQuotaTier:      domain.QuotaTier(settings.DefaultQuotaTier),
		MaxHosted:             launch.MaxHosted,
		Repository:            repository.Named,
		RepositoryURL:         repository.URL,
		RepositoryAccount:     repository.CreateOn,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	planner := &domain.Agent{ID: plannerID, Name: "Planner", CreatedAt: now}
	signup := domain.AgentSignup{
		AgentID: plannerID, Role: domain.SignupRolePlanner, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, AgentType: domain.PlannerFlavor, Model: launch.DefaultModel,
		Actor: &by,
	}
	if err := s.store.CreateDraft(ctx, pe, planner, signup); err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	pe.SignupCount = 1
	s.logger.Info("draft created", zap.String("id", pe.ID), zap.String("planner", plannerID))
	s.wakeRepositories(pe)
	return pe, nil
}
