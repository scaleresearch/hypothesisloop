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

// DefaultPlanning is the planning allowance of a deployment that configures none.
var DefaultPlanning = Planning{
	AllowanceAcceleratorHours: 0.5, Limit: 30 * time.Minute, ProposedFor: 7 * 24 * time.Hour,
	MaxConcurrentPerOrg: 2, MaxPerDayPerOrg: 10,
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

// CreateDraft creates a platform experiment in status draft from one sentence. It has the
// planning allowance for a budget and the planning limit for an end, and nothing else of a run:
// the planner proposes the rest and a person accepts it.
func (s *PlatformExperimentsService) CreateDraft(ctx context.Context, projectID, goal string, by domain.Actor, planning Planning, maxHosted int) (*domain.PlatformExperiment, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, invalidPlatformExperiment(errors.New("goal is required: one sentence saying what you want to find out"))
	}
	if len(goal) > domain.MaxGoalBytes {
		return nil, invalidPlatformExperiment(fmt.Errorf("goal is %d bytes and at most %d: say it in a sentence or two, the planner works out the rest", len(goal), domain.MaxGoalBytes))
	}
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{
		ID:                     "pe-" + uuid.New().String()[:8],
		ProjectID:              projectID,
		Name:                   draftName(goal),
		Goal:                   goal,
		CreatedBy:              &by,
		BudgetAcceleratorHours: planning.AllowanceAcceleratorHours,
		MaxAgents:              1,
		Metrics:                []domain.MetricDefinition{},
		ReportIntervalSeconds:  30,
		EndsAt:                 now.Add(planning.Limit),
		Status:                 domain.PlatformExpDraft,
		Stages:                 s.cfg.DefaultStages,
		CurrentStage:           1,
		HypothesisSubmitPolicy: domain.SubmitterPolicyMixed,
		JobSubmitPolicy:        domain.SubmitterPolicyMixed,
		DefaultQuotaTier:       domain.QuotaTierGuaranteed,
		MaxHosted:              maxHosted,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.CreateDraft: %w", err)
	}
	s.logger.Info("draft created", zap.String("id", pe.ID))
	return pe, nil
}

// WithPlanning sets what a draft may spend and how many an org may have.
func (h *PlatformExperimentsHandler) WithPlanning(planning Planning) *PlatformExperimentsHandler {
	h.planning = planning
	return h
}

// RegisterPlanningHuma registers the way from one sentence to a running platform experiment:
// a draft, the planner's proposal, and the person's acceptance.
func RegisterPlanningHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-draft", Method: "POST", Path: "/platform-experiments/drafts",
		Summary: "Start planning a platform experiment from one sentence", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Creates a platform experiment in status draft from goal, one sentence saying what the caller wants to find out. " +
			"A draft has the planning allowance for a budget, no metrics, and nobody joins it: the platform plans it and proposes the metrics, " +
			"the brief, the hardware, the budget and the agents, and it opens only when a person accepts that proposal. Until then nothing but the " +
			"allowance can be spent. Its name is the goal cut short until the proposal names it. POST /platform-experiments creates one whole instead, " +
			"and states what a run costs itself. Closing a draft (POST /platform-experiments/{id}/close) abandons it.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ProjectID string `json:"project_id"`
			Goal      string `json:"goal" doc:"What you want to find out, in a sentence or two. At most 2000 bytes."`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if in.Body.ProjectID == "" {
			return nil, huma.Error400BadRequest("project_id is required")
		}
		if err := access.Project(ctx, in.Body.ProjectID); err != nil {
			return nil, err
		}
		caller, _ := auth.FromContext(ctx)
		pe, err := h.svc.CreateDraft(ctx, in.Body.ProjectID, in.Body.Goal, caller.Actor(), h.planning, h.launch.MaxHosted)
		if errors.Is(err, ErrInvalidPlatformExperiment) {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err != nil {
			return nil, h.refusal("create draft", err)
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})
}
