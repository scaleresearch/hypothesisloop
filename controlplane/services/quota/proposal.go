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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Proposal is what a draft's planner proposes: the experiment as its file would state it, and
// beside it what a run costs, which is only ever proposed. A person states the budget, the
// duration and the agents again when accepting, by the rule that keeps them out of the
// experiment file: an agent's token wrote this.
type Proposal struct {
	// Experiment is shaped exactly like .hypothesisloop/experiment.yaml: name, description (the
	// brief every agent works from), metrics, report_interval_seconds, stages and the policies.
	Experiment json.RawMessage `json:"experiment" doc:"The experiment as .hypothesisloop/experiment.yaml would state it: name, description, metrics, report_interval_seconds, and optionally stages, hypothesis_submit_policy, job_submit_policy, default_quota_tier, max_concurrent_accelerators."`

	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"Proposed budget of the run."`
	DurationHours          float64         `json:"duration_hours" doc:"Proposed length of the run, from its start."`
	Agents                 []ProposedAgent `json:"agents" doc:"The researchers proposed, one entry each."`

	// BaselineJobID is the planner's completed job that measured the baseline. Its numbers are
	// read from the metrics store when the proposal is shown, never copied here.
	BaselineJobID string `json:"baseline_job_id,omitempty"`
	// AcceleratorType is the catalog type the jobs are sized for. Empty for a run on CPU alone.
	AcceleratorType string `json:"accelerator_type,omitempty"`

	MetricReason string   `json:"metric_reason" doc:"Why the ranked metric is the goal itself, in plain words."`
	Measurement  string   `json:"measurement" doc:"How it is measured, in plain words: the harness, the data it never trains on, the repeats."`
	OutOfScope   []string `json:"out_of_scope,omitempty"`
	Risks        []string `json:"risks,omitempty"`
	Questions    []string `json:"questions,omitempty" doc:"What the person should answer before starting, when the goal left it open."`
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
	return &reasonError{status: http.StatusUnprocessableEntity, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// checkProposal is the mechanical check of a proposal: everything that can be decided from the
// proposal and the platform's config. It returns the experiment the proposal states, decoded.
func (h *PlatformExperimentsHandler) checkProposal(p Proposal, maxHosted int) (CreatePlatformExperimentRequest, error) {
	experiment, err := decodeExperimentBase(p.Experiment)
	if err != nil {
		return experiment, refusedProposal("invalid_experiment", "experiment: %s", err)
	}
	if strings.TrimSpace(experiment.Name) == "" || strings.TrimSpace(experiment.Description) == "" {
		return experiment, refusedProposal("invalid_experiment", "experiment: name and description are required: the description is the brief every researcher works from")
	}
	if err := domain.ValidateMetricDefinitions(experiment.Metrics); err != nil {
		return experiment, refusedProposal("invalid_metrics", "experiment.metrics: %s", err)
	}
	if err := domain.RequireRankingMetric(experiment.Metrics); err != nil {
		return experiment, refusedProposal("invalid_metrics", "experiment.metrics: %s", err)
	}
	if experiment.ReportIntervalSeconds <= 0 {
		return experiment, refusedProposal("invalid_experiment", "experiment.report_interval_seconds must be positive: how often the harness really reports")
	}
	if len(experiment.Stages) > 0 {
		if err := domain.ValidateStages(experiment.Stages); err != nil {
			return experiment, refusedProposal("invalid_stages", "experiment.stages: %s", err)
		}
	}
	for name, policy := range map[string]string{"hypothesis_submit_policy": experiment.HypothesisSubmitPolicy, "job_submit_policy": experiment.JobSubmitPolicy} {
		if _, err := domain.ParseSubmitterPolicy(policy); err != nil {
			return experiment, refusedProposal("invalid_experiment", "experiment.%s: %s", name, err)
		}
	}
	if experiment.DefaultQuotaTier != "" && !domain.ValidQuotaTierOverride(experiment.DefaultQuotaTier) {
		return experiment, refusedProposal("invalid_experiment", "experiment.default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if experiment.MaxConcurrentAccelerators != nil && *experiment.MaxConcurrentAccelerators <= 0 {
		return experiment, refusedProposal("invalid_experiment", "experiment.max_concurrent_accelerators must be positive")
	}
	if p.BudgetAcceleratorHours <= 0 || p.DurationHours <= 0 {
		return experiment, refusedProposal("invalid_cost", "budget_accelerator_hours and duration_hours must both be positive: what the run you propose would spend and how long it would take")
	}
	// The coordinator the platform runs for an accepted draft is one of its machines.
	if len(p.Agents) == 0 || len(p.Agents) > maxHosted-1 {
		return experiment, refusedProposal("invalid_agents", "agents: propose between 1 and %d researchers, got %d", maxHosted-1, len(p.Agents))
	}
	for i, agent := range p.Agents {
		if _, _, err := domain.ResolveAgentLaunch(agent.Flavor, agent.Model, h.launch.DefaultModel, agent.Hyperparameters); err != nil {
			return experiment, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
	}
	if p.AcceleratorType != "" && !h.knownAcceleratorType(p.AcceleratorType) {
		return experiment, refusedProposal("unknown_accelerator_type", "accelerator_type %q is not in the resource catalog: copy one from `hl capacity`, or leave it out for a run on CPU alone", p.AcceleratorType)
	}
	for name, text := range map[string]string{"metric_reason": p.MetricReason, "measurement": p.Measurement} {
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

func (h *PlatformExperimentsHandler) knownAcceleratorType(name string) bool {
	for _, known := range h.catalog.AcceleratorTypes {
		if known.Name == name {
			return true
		}
	}
	return false
}

// requirePlanner admits only the planner of the draft id: the agent the platform signed up to
// plan it, with its own token.
func (h *PlatformExperimentsHandler) requirePlanner(ctx context.Context, id string) (string, error) {
	caller, _ := auth.FromContext(ctx)
	if caller.Kind != auth.KindAgent || caller.PlatformExperimentID != id {
		return "", huma.Error403Forbidden("a proposal is written by the draft's planner, with its own token")
	}
	signup, err := h.svc.store.GetAgentSignup(ctx, id, caller.AgentID)
	if err != nil {
		return "", h.refusal("read planner", err)
	}
	if signup == nil || signup.Flavor != domain.PlannerFlavor {
		return "", huma.Error403Forbidden("a proposal is written by the draft's planner, with its own token")
	}
	return caller.AgentID, nil
}

func registerProposalHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-proposal", Method: "PUT", Path: "/platform-experiments/{id}/proposal",
		Summary: "Propose the platform experiment a draft becomes", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after). " +
			"experiment is the experiment as its file would state it; budget_accelerator_hours, duration_hours and agents are proposed, and the person who accepts states them. " +
			"The proposal is checked before it is stored and refused with 422 and a reason the planner can act on: invalid_experiment, invalid_metrics, invalid_stages, " +
			"invalid_cost, invalid_agents, unknown_accelerator_type, missing_explanation, too_long. Stored, it replaces the one before, marks the draft proposed " +
			"(proposed_at), and the planner's machine is released: GET /platform-experiments/{id} shows the proposal, and the draft waits for a person to accept it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body Proposal
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if _, err := h.requirePlanner(ctx, in.ID); err != nil {
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
		if _, err := h.checkProposal(in.Body, pe.MaxHosted); err != nil {
			return nil, err
		}
		stored, err := json.Marshal(in.Body)
		if err != nil {
			return nil, h.refusal("encode proposal", err)
		}
		err = h.svc.store.SetProposal(ctx, in.ID, stored, time.Now().UTC().Add(h.planning.ProposedFor))
		if errors.Is(err, db.ErrNotDraft) {
			return nil, notADraft(pe)
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

func notADraft(pe *domain.PlatformExperiment) error {
	return &reasonError{status: http.StatusConflict, Reason: "not_a_draft", Message: "platform experiment " + pe.ID + " is " + string(pe.Status) + ": only a draft is proposed, revised or accepted"}
}
