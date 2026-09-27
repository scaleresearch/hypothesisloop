package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Planning is what one draft may spend before a person accepts its proposal, and how many
// drafts an org may have. The platform pays for planning.
type Planning struct {
	// AllowanceAcceleratorHours is the draft's whole budget: what the planner's baseline job
	// may cost.
	AllowanceAcceleratorHours float64
	// Limit is how long a planner has to propose, from the goal or from a revision.
	Limit time.Duration
	// ProposedFor is how long a proposal waits for the person before the draft is closed.
	ProposedFor time.Duration
	// MaxConcurrentPerOrg and MaxPerDayPerOrg bound an org's drafts being planned and the
	// drafts it creates in a day.
	MaxConcurrentPerOrg int
	MaxPerDayPerOrg     int
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
// an agent the platform runs, of role baseline, so that it is never ranked or cut, and of flavor
// planner. The draft has the planning allowance for a budget and the planning limit for an end,
// and nothing else of a run: the planner proposes the rest and a person accepts it.
func (s *PlatformExperimentsService) CreateDraft(ctx context.Context, projectID, goal string, by domain.Actor, planning Planning, launch AgentLaunch, prepare func(context.Context, *domain.PlatformExperiment) error) (*domain.PlatformExperiment, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, refuseUpdate(422, "invalid_goal", "goal is required: one sentence saying what you want to find out")
	}
	if len(goal) > domain.MaxGoalBytes {
		return nil, refuseUpdate(422, "invalid_goal", "goal is %d bytes and at most %d: say it in a sentence or two, the planner works out the rest", len(goal), domain.MaxGoalBytes)
	}
	if launch.Launcher == "" || launch.DefaultModel == "" || launch.MaxHosted <= 0 {
		return nil, errors.New("platform_experiments.CreateDraft: no agent launch settings, and a draft is planned by an agent the platform runs")
	}
	if err := s.requireLauncher(ctx, launch.Launcher); err != nil {
		return nil, err
	}
	plannerID, err := domain.NewAgentID()
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	now := time.Now().UTC()
	// A draft states nothing of a run yet, so what its planner's job runs under is the platform's.
	settings := s.withSettings(CreatePlatformExperimentRequest{})
	pe := &domain.PlatformExperiment{
		ID:                     "pe-" + uuid.New().String()[:8],
		ProjectID:              projectID,
		Name:                   draftName(goal),
		Goal:                   goal,
		Actor:                  &by,
		BudgetAcceleratorHours: planning.AllowanceAcceleratorHours,
		MaxAgents:              1,
		Metrics:                []domain.MetricDefinition{},
		ReportIntervalSeconds:  settings.ReportIntervalSeconds,
		PlanningEndsAt:         now.Add(planning.Limit),
		Status:                 domain.PlatformExpDraft,
		Stages:                 settings.Stages,
		CurrentStage:           1,
		DefaultQuotaTier:       domain.QuotaTier(settings.DefaultQuotaTier),
		MaxHosted:              launch.MaxHosted,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	planner := &domain.Agent{ID: plannerID, Name: "Planner", PerformanceScore: 0.5, CreatedAt: now}
	signup := domain.AgentSignup{
		AgentID: plannerID, Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, Launcher: launch.Launcher, Flavor: domain.PlannerFlavor, Model: launch.DefaultModel,
	}
	err = s.store.CreateDraft(ctx, pe, planner, signup, db.DraftLimits{Concurrent: planning.MaxConcurrentPerOrg, PerDay: planning.MaxPerDayPerOrg}, prepare)
	var over *db.PlanningLimitError
	if errors.As(err, &over) {
		if over.PerDay {
			return nil, refuseSignup(429, "planning_limit", "this org created %d drafts in the last 24 hours, which is the most the platform plans for one org in a day: accept or use the ones it has, or create the experiment yourself with POST /platform-experiments", over.Limit)
		}
		return nil, refuseSignup(429, "planning_limit", "this org has %d drafts open, which is the most the platform plans for one org at once: accept one, or close one with POST /platform-experiments/{id}/close", over.Limit)
	}
	var answered huma.StatusError
	if errors.As(err, &answered) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	pe.SignupCount = 1
	s.logger.Info("draft created", zap.String("id", pe.ID), zap.String("planner", plannerID))
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
			"A draft has the planning allowance for a budget, no metrics, and one agent, its planner, which the platform runs (flavor planner, role baseline, in the roster like any agent) " +
			"and nobody else joins: the planner works out how to measure the goal and proposes the metrics, " +
			"the brief, the hardware, the budget and the agents, and it opens only when a person accepts that proposal. Until then nothing but the " +
			"allowance can be spent. Its name is the goal cut short until the proposal names it. POST /platform-experiments creates one whole instead, " +
			"and states what a run costs itself. Closing a draft (POST /platform-experiments/{id}/close) abandons it. " +
			"Its planner and later its agents keep their code in repository: the one the request names, checked as on POST /platform-experiments, or a private one the platform creates on the GitHub account the org connected. " +
			"repository_account says which account when the org connected several. 422 github_not_connected, repository_account_required, repository_account_personal, repository_creation_not_permitted, 409 repository_exists and 502 repository_not_created as on POST /platform-experiments. " +
			"On a deployment whose git host is plain, repository is a name under the host's root and is required: 422 repository_required without it, repository_not_reachable when it does not answer git. " +
			"422 invalid_goal for a goal that is empty or over the length. 503 launcher_not_registered when the deployment has no launcher to run the planner. " +
			"429 planning_limit when the org has as many drafts open, or created as many in the last 24 hours, as the platform plans for one org.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ProjectID         string `json:"project_id"`
			Goal              string `json:"goal" doc:"What you want to find out, in a sentence or two. At most 2000 bytes."`
			Repository        string `json:"repository,omitempty" required:"false" doc:"owner/name on GitHub, covered by an installation the org connected. Left out, the platform creates a private repository on the GitHub account the org connected."`
			RepositoryAccount string `json:"repository_account,omitempty" required:"false" doc:"The connected GitHub account the new repository is created on. Required only when the org connected several."`
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
		caller, _ := auth.FromContext(ctx)
		pe, err := h.svc.CreateDraft(ctx, in.Body.ProjectID, in.Body.Goal, caller.Actor(), h.planning, h.launch, h.provideRepository(named, in.Body.RepositoryAccount))
		var answered huma.StatusError
		if errors.As(err, &answered) {
			return nil, err
		}
		if err != nil {
			return nil, h.refusal("create draft", err)
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})
}
