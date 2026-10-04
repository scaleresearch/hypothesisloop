package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Planning bounds a draft, which the platform pays for: how long it is planned and waits.
type Planning struct {
	// Limit is how long a planner has to propose, from the goal or from a revision.
	Limit time.Duration
	// ProposedFor is how long a proposal waits for the person before the draft is closed.
	ProposedFor time.Duration
	// PlannerKept is how long a planner's machine is kept after its answer, for a revision asked
	// for in that time to reach the planner that is still there.
	PlannerKept time.Duration
	// AutoAccept is how long a proposal waits before the platform accepts it as proposed, unless
	// a person accepts it sooner, holds it, revises it or closes it. 0 leaves it to a person.
	AutoAccept time.Duration
}

// draftNameLength bounds the name a draft takes from its goal until the proposal names it.
const draftNameLength = 80

// draftName is a goal cut to a name at a word.
func draftName(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	if utf8.RuneCountInString(goal) <= draftNameLength {
		return goal
	}
	runes := []rune(goal)[:draftNameLength]
	if cut := strings.LastIndexByte(string(runes), ' '); cut > draftNameLength/2 {
		return string(runes)[:cut] + "…"
	}
	return string(runes) + "…"
}

// CreateDraft creates a platform experiment in status draft from one sentence, with its planner:
// an agent the platform runs, of role planner. The draft has the planning limit for an end and nothing of a run, no budget among it:
// the planner proposes the rest and a person accepts it. A repository to be created is created
// after this returns, by the repository pass it wakes (ProvideRepositories).
func (s *PlatformExperimentsService) CreateDraft(ctx context.Context, projectID, goal, continues string, by domain.Actor, planning Planning, launch AgentLaunch, repository RepositoryChoice) (*domain.PlatformExperiment, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, refuseUpdate(422, "invalid_goal", "goal is required: one sentence saying what you want to find out")
	}
	if len(goal) > domain.MaxGoalBytes {
		return nil, refuseUpdate(422, "invalid_goal", "goal is %d bytes and at most %d: say it in a sentence or two, the planner works out the rest", len(goal), domain.MaxGoalBytes)
	}
	if launch.Launcher == "" || launch.DefaultModel == "" {
		return nil, errors.New("platform_experiments.CreateDraft: no agent launch settings, and a draft is planned by an agent the platform runs")
	}
	if maxProposedResearchers(launch.MaxHosted) < 1 {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: the platform runs %d machines for one platform experiment (agents.max_hosted_per_platform_experiment), and an accepted draft takes three at least: its coordinator, its baseline agent and one researcher", launch.MaxHosted)
	}
	if err := s.requireLauncher(ctx, launch.Launcher); err != nil {
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
	settings := s.withSettings(CreatePlatformExperimentRequest{})
	pe := &domain.PlatformExperiment{
		ID:                    id,
		ProjectID:             projectID,
		Name:                  draftName(goal),
		Goal:                  goal,
		Continues:             continues,
		Actor:                 &by,
		MaxAgents:             maxProposedResearchers(launch.MaxHosted),
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
	planner := &domain.Agent{ID: plannerID, Name: "Planner", PerformanceScore: 0.5, CreatedAt: now}
	signup := domain.AgentSignup{
		AgentID: plannerID, Role: domain.SignupRolePlanner, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, Launcher: launch.Launcher, Flavor: domain.PlannerFlavor, Model: launch.DefaultModel,
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

// RegisterPlanningHuma registers the way from one sentence to a running platform experiment:
// a draft, the planner's proposal, and the person's acceptance.
func RegisterPlanningHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access
	registerProposalHuma(doc, h)

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-draft", Method: "POST", Path: "/platform-experiments/drafts",
		Summary: "Start planning a platform experiment from one sentence", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Creates a platform experiment in status draft from goal, one sentence saying what the caller wants to find out. " +
			"A draft has no budget, no metrics, and one agent, its planner, which the platform runs (role planner, in the roster like any agent) " +
			"and nobody else joins: the planner works out how to measure the goal and proposes the metrics, " +
			"the brief, the hardware, the budget and the agents, and it opens only when that proposal is accepted: by a person, or by the platform as proposed at accepts_at. Until then its max_agents is the most researchers a proposal may name: max_hosted less the coordinator and the baseline agent an accepted draft is given. A draft takes no job, so nothing is measured " +
			"and no compute is spent until then. Its name is the goal cut short until the proposal names it. POST /platform-experiments creates one whole instead, " +
			"and states what a run costs itself. Closing a draft (POST /platform-experiments/{id}/close) abandons it. " +
			"Its planner and later its agents keep their code in repository: the one the request names, checked as on POST /platform-experiments, or a private one the platform creates on the GitHub account the org connected. " +
			"repository_account says which account when the org connected several. 422 github_not_connected, repository_account_required and installation_not_connected as on POST /platform-experiments. " +
			"A repository the platform creates is created after this answers: the draft is answered without repository and with status_reason repository_creating, and its planner starts once the repository is there. " +
			"When GitHub does not create it the draft is closed with close_reason repository_not_created, and its status_reason message says what GitHub answered. " +
			"On a deployment whose git host is plain, repository is a name under the host's root and is required: 422 repository_required without it, repository_not_reachable when it does not answer git. " +
			"422 invalid_goal for a goal that is empty or over the length, invalid_continues for a continues that names no closed platform experiment of the project and repository. 503 launcher_not_registered when the deployment has no launcher to run the planner.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ProjectID         string `json:"project_id"`
			Goal              string `json:"goal" doc:"What you want to find out, in a sentence or two. At most 2000 bytes."`
			Repository        string `json:"repository,omitempty" required:"false" doc:"owner/name on GitHub, covered by an installation the org connected. Left out, the platform creates a private repository on the GitHub account the org connected."`
			RepositoryAccount string `json:"repository_account,omitempty" required:"false" doc:"The connected GitHub account the new repository is created on. Required only when the org connected several."`
			Continues         string `json:"continues,omitempty" required:"false" doc:"The id of a closed platform experiment of the same project to carry on, which keeps its code in the repository this one names. Its agents and its coordinator read that run as their own, its summary, standings, jobs and hypotheses, and its baseline starts from the commit of that run's baseline job, so both are measured by the same harness. Left out, it starts fresh. 422 invalid_continues for one that is no closed platform experiment of the project, or keeps its code in another repository."`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if in.Body.ProjectID == "" {
			return nil, huma.Error400BadRequest("project_id is required")
		}
		if err := access.Project(ctx, in.Body.ProjectID); err != nil {
			return nil, err
		}
		named, err := h.repositoryOf(ctx, in.Body.ProjectID, in.Body.Repository, in.Body.RepositoryAccount)
		if err != nil {
			return nil, err
		}
		repository, err := h.chooseRepository(ctx, in.Body.ProjectID, named, in.Body.RepositoryAccount)
		if err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		pe, err := h.svc.CreateDraft(ctx, in.Body.ProjectID, in.Body.Goal, in.Body.Continues, caller.Actor(), h.planning, h.launch, repository)
		var answered huma.StatusError
		if errors.As(err, &answered) {
			return nil, err
		}
		if err != nil {
			return nil, h.refusal("create draft", err)
		}
		return h.Answer(ctx, pe)
	})
}
