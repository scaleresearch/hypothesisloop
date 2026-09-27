package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Proposal is what a draft's planner proposes: the experiment as its file would state it, and
// beside it what a run costs, which is only ever proposed. A person states the budget, the
// duration and the agents again when accepting, by the rule that keeps them out of the
// experiment file: an agent's token wrote this. Nothing has run when it is written: the
// baseline is measured after a person accepts, before any researcher starts.
type Proposal struct {
	// Experiment is shaped exactly like .hypothesisloop/experiment.yaml: name, description (the
	// brief every agent works from), metrics, report_interval_seconds, stages and the policies.
	// It is checked by validate, as the body of a create is.
	Experiment json.RawMessage `json:"experiment" doc:"The experiment as .hypothesisloop/experiment.yaml would state it: name, description, metrics, and optionally report_interval_seconds, stages and default_quota_tier, which left out are the platform's settings, and max_concurrent_accelerators."`

	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"Proposed budget of the run: a cap, estimated before anything ran."`
	DurationHours          float64         `json:"duration_hours" doc:"Proposed length of the run, from its start."`
	Agents                 []ProposedAgent `json:"agents" doc:"The researchers proposed, one entry each."`

	AcceleratorType domain.AcceleratorType `json:"accelerator_type" doc:"The type the run's jobs are sized for, as GET /resource-catalog/capacity lists it for a cluster this org's jobs can be placed on: an accelerator, or the CPU type for work that needs none, whose budget is then CPU core-hours."`

	MetricReason string   `json:"metric_reason" doc:"Why the ranked metric is the goal itself, in plain words."`
	Measurement  string   `json:"measurement" doc:"How it is measured, in plain words: the harness, the data it never trains on, the repeats."`
	SizingReason string   `json:"sizing_reason" doc:"Why this budget, this long and this many researchers are the least that can reach the goal, in plain words, as an estimate: the trial length assumed, what an hour on the type costs, and the trials counted."`
	OutOfScope   []string `json:"out_of_scope,omitempty"`
	Risks        []string `json:"risks,omitempty"`
	Questions    []string `json:"questions,omitempty" doc:"What the person should answer before starting, when the goal left it open: among them any bound of a gate that depends on a number nobody has measured yet."`
}

// ProposedAgent is one researcher of a proposal, as POST /platform-experiments/{id}/agents takes it.
type ProposedAgent struct {
	Flavor          string         `json:"flavor,omitempty"`
	Model           string         `json:"model,omitempty"`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty"`
}

// maxProposalTextBytes bounds each plain-words field of a proposal, and each entry of its lists.
const maxProposalTextBytes = 4000

// refusedProposal is a proposal the planner can fix: reason names what is wrong.
func refusedProposal(reason, format string, args ...any) error {
	return api.Refuse(http.StatusUnprocessableEntity, reason, fmt.Sprintf(format, args...))
}

// checkProposal is the mechanical check of a proposal: everything that can be decided from the
// proposal and the platform's config. It returns the experiment the proposal states, as validate
// answers it: with its cost, its length from now and one place for each researcher.
func (h *PlatformExperimentsHandler) checkProposal(p Proposal, maxHosted int, now time.Time) (CreatePlatformExperimentRequest, error) {
	experiment, err := decodeExperimentBase(p.Experiment)
	if err != nil {
		return experiment, refusedProposal("invalid_experiment", "experiment: %s", err)
	}
	// The coordinator the platform runs for an accepted draft is one of its machines.
	if len(p.Agents) == 0 || len(p.Agents) > maxHosted-1 {
		return experiment, refusedProposal("invalid_agents", "agents: propose between 1 and %d researchers, got %d", maxHosted-1, len(p.Agents))
	}
	// An accepted draft starts when it is accepted, whatever its experiment says of a start.
	experiment.StartsAt = time.Time{}
	experiment.BudgetAcceleratorHours, experiment.DurationHours, experiment.MaxAgents = p.BudgetAcceleratorHours, p.DurationHours, len(p.Agents)
	if experiment, err = h.svc.validate(experiment, now); err != nil {
		return experiment, err
	}
	for i, agent := range p.Agents {
		if _, _, err := domain.ResolveAgentLaunch(agent.Flavor, agent.Model, h.launch.DefaultModel, agent.Hyperparameters); err != nil {
			return experiment, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
	}
	for name, text := range map[string]string{"metric_reason": p.MetricReason, "measurement": p.Measurement, "sizing_reason": p.SizingReason} {
		if strings.TrimSpace(text) == "" {
			return experiment, refusedProposal("missing_explanation", "%s is required: the person who confirms reads it in place of the code", name)
		}
		if len(text) > maxProposalTextBytes {
			return experiment, refusedProposal("too_long", "%s is %d bytes and at most %d", name, len(text), maxProposalTextBytes)
		}
	}
	for name, list := range map[string][]string{"out_of_scope": p.OutOfScope, "risks": p.Risks, "questions": p.Questions} {
		for _, entry := range list {
			if strings.TrimSpace(entry) == "" || len(entry) > maxProposalTextBytes {
				return experiment, refusedProposal("too_long", "%s: each entry is a sentence of at most %d bytes", name, maxProposalTextBytes)
			}
		}
	}
	return experiment, nil
}

// requirePlanner admits only the planner of the draft id: the agent the platform signed up to
// plan it, with its own token.
func (h *PlatformExperimentsHandler) requirePlanner(ctx context.Context, id string) error {
	caller, _ := auth.FromContext(ctx)
	if caller.Kind != auth.KindAgent || caller.PlatformExperimentID != id {
		return huma.Error403Forbidden("a proposal is written by the draft's planner, with its own token")
	}
	signup, err := h.svc.store.GetAgentSignup(ctx, id, caller.AgentID)
	if err != nil {
		return h.refusal("read planner", err)
	}
	if signup == nil || signup.Flavor != domain.PlannerFlavor {
		return huma.Error403Forbidden("a proposal is written by the draft's planner, with its own token")
	}
	return nil
}

func registerProposalHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access
	registerDeclineHuma(doc, h)
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "revise-draft", Method: "POST", Path: "/platform-experiments/{id}/revise",
		Summary: "Ask a draft's planner to change its proposal, or to try again after it declined", Tags: []string{"platform-experiments"},
		Description: "Takes one sentence or two saying what to change (\"it must fit a 4090\", \"measure latency too\"). The request is recorded with whose credential made it and shown in the draft's revisions; " +
			"the proposal stays readable, proposed_at is cleared, and the platform starts the planner again with what it knew, with the planning limit to propose anew. " +
			"A draft its planner declined is revised the same way: say what the planner lacked, and its reason stays readable until it answers again. " +
			"409 invalid_transition for anything but a draft its planner has answered: while the planner still works there is nothing to revise yet.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Message string `json:"message" doc:"What to change, in plain words."`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		message := strings.TrimSpace(in.Body.Message)
		if message == "" || len(message) > maxRevisionBytes {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("message is required and at most %d bytes: say what to change in a sentence or two", maxRevisionBytes))
		}
		caller, _ := auth.FromContext(ctx)
		switch err := h.svc.store.Revise(ctx, in.ID, message, caller.Actor(), time.Now().UTC().Add(h.planning.Limit)); {
		case errors.Is(err, db.ErrNotDraft):
			return nil, invalidTransition("platform experiment %s is not a draft: only a draft's proposal is revised", in.ID)
		case errors.Is(err, db.ErrNothingProposed):
			return nil, invalidTransition("draft %s has nothing to revise yet: its planner is still working", in.ID)
		case err != nil:
			return nil, h.refusal("revise", err)
		}
		h.logger.Info("draft revision asked", zap.String("id", in.ID))
		pe, err := h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil || pe == nil {
			return nil, h.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
		}
		if err := h.nameActors(ctx, pe); err != nil {
			return nil, h.refusal("name actors", err)
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "accept-draft", Method: "POST", Path: "/platform-experiments/{id}/accept",
		Summary: "Accept a draft's proposal", Tags: []string{"platform-experiments"},
		Description: "Makes a proposed draft an open platform experiment in one write: its name, description, metrics, report interval, stages and policies are the proposal's; " +
			"its budget, its length and its researchers are the ones this body states, whatever the proposal proposed, because an agent wrote the proposal. " +
			"The budget caps everything the platform experiment spends, the baseline included: what measuring it cost is taken from the budget, the rest is shared among the researchers, and an agent's burst hours run on idle capacity and come out of its share, never on top of it. " +
			"The platform hosts its coordinator and each researcher, and starts_at is now. Nothing has run yet: the coordinator has the baseline measured and declares it (PATCH /platform-experiments/{id}, baseline_job_id), " +
			"no researcher's machine is started before, and the platform starts the experiment once the baseline is declared. " +
			"409 invalid_transition for anything but a draft with a proposal; 422 with the proposal's reasons when the stated cost or agents do not pass its check; " +
			"503 launcher_not_registered when nothing could run it. A draft's own planner cannot accept it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body AcceptRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := h.accept(ctx, in.ID, in.Body)
		if err != nil {
			return nil, err
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-proposal", Method: "PUT", Path: "/platform-experiments/{id}/proposal",
		Summary: "Propose the platform experiment a draft becomes", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after). " +
			"experiment is the experiment as its file would state it; budget_accelerator_hours, duration_hours and agents are proposed, and the person who accepts states them. " +
			"A proposal defines the experiment and runs nothing: its brief's BASELINE block states the control configuration and that nothing is measured yet, its numbers are estimates, " +
			"and the baseline is measured once a person accepted, before any researcher starts (PATCH /platform-experiments/{id}, baseline_job_id). " +
			"The proposal is checked before it is stored, its experiment and its cost by the rules of POST /platform-experiments, and refused with 422 and a reason the planner can act on: invalid_experiment, invalid_metrics, invalid_stages, " +
			"invalid_cost, invalid_agents, invalid_length, run_too_long (duration_hours over the platform's quota.max_platform_experiment_days), missing_explanation, too_long, " +
			"and accelerator_unreachable for an accelerator_type no cluster this org's jobs can be placed on has now. Stored, it replaces the one before, marks the draft proposed " +
			"(proposed_at), and the planner's machine is released: GET /platform-experiments/{id} shows the proposal, and the draft waits for a person to accept it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body Proposal
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := h.requirePlanner(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, h.refusal("read draft", err)
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		if pe.Status != domain.PlatformExpDraft {
			return nil, notADraft(pe)
		}
		if _, err := h.checkProposal(in.Body, pe.MaxHosted, time.Now().UTC()); err != nil {
			return nil, err
		}
		caller, _ := auth.FromContext(ctx)
		reachable, err := h.reachable(ctx, caller.OrgIDs[0])
		if err != nil {
			return nil, h.refusal("read capacity", err)
		}
		if !reachable[string(in.Body.AcceleratorType)] {
			return nil, refusedProposal("accelerator_unreachable", "accelerator_type %q is on no cluster this org's jobs can be placed on now: state a type `hl capacity` lists, copied verbatim", in.Body.AcceleratorType)
		}
		stored, err := json.Marshal(in.Body)
		if err != nil {
			return nil, h.refusal("encode proposal", err)
		}
		err = h.svc.store.SetProposal(ctx, in.ID, stored, time.Now().UTC().Add(h.planning.ProposedFor))
		if errors.Is(err, db.ErrNotDraft) {
			return nil, notADraft(pe)
		}
		if errors.Is(err, db.ErrPlanningOver) {
			return nil, planningOver(pe)
		}
		if err != nil {
			return nil, h.refusal("store proposal", err)
		}
		h.logger.Info("draft proposed", zap.String("id", in.ID))
		pe, err = h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil || pe == nil {
			return nil, h.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})
}

// AcceptRequest is what the person who accepts a draft states themselves: what the run costs,
// how long it lasts and who runs it. The proposal only proposed them, since an agent wrote it.
type AcceptRequest struct {
	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"The budget of the run: a cap on everything it spends, with what measuring the baseline costs and burst hours included."`
	DurationHours          float64         `json:"duration_hours" doc:"How long the run lasts from now."`
	Agents                 []ProposedAgent `json:"agents" doc:"The researchers the platform runs, one entry each."`
}

func invalidTransition(format string, args ...any) error {
	return api.Refuse(http.StatusConflict, "invalid_transition", fmt.Sprintf(format, args...))
}

// accept makes the draft id the open platform experiment its proposal describes, at the cost
// req states, with a hosted coordinator and its researchers, in one write.
func (h *PlatformExperimentsHandler) accept(ctx context.Context, id string, req AcceptRequest) (*domain.PlatformExperiment, error) {
	pe, err := h.svc.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, h.refusal("read draft", err)
	}
	if pe == nil {
		return nil, huma.Error404NotFound("platform experiment not found")
	}
	if pe.Status != domain.PlatformExpDraft {
		return nil, invalidTransition("platform experiment %s is %s: only a draft is accepted", id, pe.Status)
	}
	if pe.ProposedAt != nil && len(pe.Proposal) == 0 {
		return nil, invalidTransition("draft %s has no proposal to accept: its planner declined (declined_reason). Revise it with POST /platform-experiments/%s/revise, or close it", id, id)
	}
	if pe.ProposedAt == nil || len(pe.Proposal) == 0 {
		return nil, invalidTransition("draft %s has no proposal to accept yet: its planner is still working", id)
	}
	var proposal Proposal
	if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
		return nil, h.refusal("decode stored proposal", err)
	}
	// The check the proposal passed, again with the cost and the agents the person states.
	proposal.BudgetAcceleratorHours, proposal.DurationHours, proposal.Agents = req.BudgetAcceleratorHours, req.DurationHours, req.Agents
	now := time.Now().UTC()
	experiment, err := h.checkProposal(proposal, pe.MaxHosted, now)
	if err != nil {
		return nil, err
	}
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" {
		return nil, h.refusal("accept", errors.New("the handler has no agent launch settings"))
	}
	if err := h.svc.requireLauncher(ctx, h.launch.Launcher); err != nil {
		return nil, h.refusal("accept", err)
	}

	pe.Name, pe.Description, pe.Metrics, pe.ReportIntervalSeconds = experiment.Name, experiment.Description, experiment.Metrics, experiment.ReportIntervalSeconds
	pe.Stages, pe.DefaultQuotaTier = experiment.Stages, domain.QuotaTier(experiment.DefaultQuotaTier)
	pe.MaxConcurrentAccelerators = experiment.MaxConcurrentAccelerators
	pe.BudgetAcceleratorHours, pe.MaxAgents, pe.EndsAt = experiment.BudgetAcceleratorHours, experiment.MaxAgents, experiment.EndsAt
	pe.CoordinatorHosted, pe.Coordinator = true, &domain.HostedCoordinator{Launcher: h.launch.Launcher, Model: h.launch.DefaultModel}

	agents := make([]*domain.Agent, 0, len(req.Agents))
	signups := make([]domain.AgentSignup, 0, len(req.Agents))
	for i, asked := range req.Agents {
		flavor, model, err := domain.ResolveAgentLaunch(asked.Flavor, asked.Model, h.launch.DefaultModel, asked.Hyperparameters)
		if err != nil {
			return nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		agentID, err := domain.NewAgentID()
		if err != nil {
			return nil, h.refusal("accept", err)
		}
		agents = append(agents, &domain.Agent{ID: agentID, Name: fmt.Sprintf("Researcher %d", i+1), PerformanceScore: 0.5, CreatedAt: now})
		signups = append(signups, domain.AgentSignup{
			AgentID: agentID, Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: h.launch.Launcher,
			Flavor: flavor, Model: model, Hyperparameters: asked.Hyperparameters,
		})
	}
	switch err := h.svc.store.AcceptDraft(ctx, pe, agents, signups); {
	case errors.Is(err, db.ErrNotDraft):
		return nil, invalidTransition("platform experiment %s is no longer a draft", id)
	case errors.Is(err, db.ErrNothingProposed):
		return nil, invalidTransition("draft %s has no proposal to accept", id)
	case errors.Is(err, db.ErrMaxHostedReached):
		return nil, refusedProposal("invalid_agents", "%s", maxHostedReached(pe.MaxHosted))
	case err != nil:
		return nil, h.refusal("accept", err)
	}
	h.logger.Info("draft accepted", zap.String("id", id), zap.Int("researchers", len(signups)), zap.Float64("budget_accelerator_hours", pe.BudgetAcceleratorHours))
	accepted, err := h.svc.store.GetPlatformExperiment(ctx, id)
	if err != nil || accepted == nil {
		return nil, h.refusal("read accepted", errors.Join(err, errors.New("the platform experiment is gone")))
	}
	return accepted, nil
}

// nameActors names whoever created pe and whoever asked for its revisions, when it carries them.
func (h *PlatformExperimentsHandler) nameActors(ctx context.Context, pe *domain.PlatformExperiment) error {
	var actors []*domain.Actor
	if pe.Actor != nil {
		actors = append(actors, pe.Actor)
	}
	if pe.ClosedBy != nil {
		actors = append(actors, pe.ClosedBy)
	}
	for i := range pe.Revisions {
		actors = append(actors, &pe.Revisions[i].Actor)
	}
	if len(actors) == 0 {
		return nil
	}
	return h.svc.store.NameActors(ctx, actors)
}

// maxRevisionBytes bounds one request to change a proposal.
const maxRevisionBytes = domain.MaxGoalBytes

// maxDeclineBytes bounds the reason a planner gives for proposing nothing.
const maxDeclineBytes = maxProposalTextBytes

func registerDeclineHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-decline", Method: "PUT", Path: "/platform-experiments/{id}/decline",
		Summary: "Decline to propose, with the reason", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after), when the goal cannot be made into an experiment: " +
			"nothing measurable, or nothing this platform can run. reason is read by the person who wrote the goal, in plain words, with what would make it plannable. " +
			"Stored, it answers the goal as a proposal does: the draft carries declined_reason and proposed_at, a proposal made before is cleared so there is nothing to accept, " +
			"the planner's machine is released, and the person revises the draft or closes it. The next proposal clears the reason.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Reason string `json:"reason" doc:"Why no experiment is proposed, and what would make the goal plannable."`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := h.requirePlanner(ctx, in.ID); err != nil {
			return nil, err
		}
		reason := strings.TrimSpace(in.Body.Reason)
		if reason == "" || len(reason) > maxDeclineBytes {
			return nil, refusedProposal("missing_explanation", "reason is required and at most %d bytes: the person who wrote the goal reads it in place of a proposal", maxDeclineBytes)
		}
		pe, err := h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, h.refusal("read draft", err)
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		if pe.Status != domain.PlatformExpDraft {
			return nil, notADraft(pe)
		}
		err = h.svc.store.Decline(ctx, in.ID, reason, time.Now().UTC().Add(h.planning.ProposedFor))
		if errors.Is(err, db.ErrNotDraft) {
			return nil, notADraft(pe)
		}
		if errors.Is(err, db.ErrPlanningOver) {
			return nil, planningOver(pe)
		}
		if err != nil {
			return nil, h.refusal("store decline", err)
		}
		h.logger.Info("draft declined", zap.String("id", in.ID))
		pe, err = h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil || pe == nil {
			return nil, h.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})
}

func planningOver(pe *domain.PlatformExperiment) error {
	return api.Refuse(http.StatusConflict, "planning_over", "the planning limit of draft "+pe.ID+" passed at "+pe.ClosesAt().UTC().Format(time.RFC3339)+": it takes no proposal and no decline, and is closed")
}

func notADraft(pe *domain.PlatformExperiment) error {
	return api.Refuse(http.StatusConflict, "not_a_draft", "platform experiment "+pe.ID+" is "+string(pe.Status)+": only a draft is proposed, revised or accepted")
}
