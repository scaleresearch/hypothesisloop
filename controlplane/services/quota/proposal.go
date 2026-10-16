package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Proposal is what a draft's planner proposes: the experiment as its file would state it, and
// beside it what a run costs, which is only ever proposed. A person states the budget, the
// duration and the agents again when accepting, by the rule that keeps them out of the
// experiment file: an agent's token wrote this. One without questions the platform accepts as
// proposed after planning.auto_accept_seconds unless a person acts first; one with questions
// waits for a person to answer them. Nothing has run when it is written:
// the baseline is measured once it is accepted, before any researcher starts.
type Proposal struct {
	// Experiment is shaped exactly like .hypothesisloop/experiment.yaml: name, description (the
	// brief every agent works from), metrics, report_interval_seconds, stages and the policies.
	// It is checked by validate, as the body of a create is.
	Experiment json.RawMessage `json:"experiment" doc:"The experiment as .hypothesisloop/experiment.yaml would state it: name, description, metrics, and optionally report_interval_seconds, stages and default_quota_tier, which left out are the platform's settings, and max_concurrent_accelerators."`

	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"Proposed budget of the run: a cap, estimated before anything ran."`
	DurationHours          float64         `json:"duration_hours" doc:"Proposed length of the run, counted from the moment it starts, once its baseline is declared: a time the goal states is this."`
	BaselineMinutes        int             `json:"baseline_minutes" doc:"The minutes the baseline agent needs, from the accept, to measure the baseline and leave its job: few where the repository holds a harness that runs unchanged, more where the harness and its environment are written from nothing. At least planning.min_baseline_minutes and at most planning.baseline_limit_minutes."`
	Agents                 []ProposedAgent `json:"agents" doc:"The researchers proposed, one entry each."`

	AcceleratorType domain.AcceleratorType `json:"accelerator_type,omitempty" doc:"The type the run's jobs are sized for, one of the accelerator_type strings GET /resource-catalog/capacity lists (hl capacity), copied verbatim. Left out for work that needs no accelerator: the run is then on CPU cores, hypothesisloop.dev/cpu=core, which is what is stored, and its budget is CPU core-hours. 422 accelerator_unreachable names every type that is listed now."`
	Image           string                 `json:"image" doc:"The image the run's jobs name as job.image, checked as a submit checks it: a job image the platform offers, by name, or the whole address of another, which a cluster that has accelerator_type runs."`
	// CPU, Memory and Storage are what one node of the run's jobs asks for, as job.cpu, job.memory
	// and job.storage state it, so that whoever writes a job copies them from here and from no prose.
	CPU     string `json:"cpu,omitempty" doc:"What the run's jobs state as job.cpu: cores for one node, a quantity such as 8 or 500m. Checked as a submit checks it: 422 invalid_resources for one that is no quantity or is negative, as for memory and storage."`
	Memory  string `json:"memory,omitempty" doc:"What the run's jobs state as job.memory: a quantity such as 32Gi. Checked as a submit checks it."`
	Storage string `json:"storage,omitempty" doc:"What the run's jobs state as job.storage: a quantity such as 100Gi. Checked as a submit checks it."`

	MetricReason string             `json:"metric_reason" doc:"Why the ranked metric is the goal itself, in plain words."`
	Measurement  string             `json:"measurement" doc:"How it is measured, in plain words: the harness, the data it never trains on, the repeats."`
	SizingReason string             `json:"sizing_reason" doc:"Why this budget, this long and this many researchers are the least that can reach the goal, in plain words, as an estimate: the trial length assumed, what an hour on the type costs, and the trials counted."`
	OutOfScope   []string           `json:"out_of_scope,omitempty"`
	Risks        []string           `json:"risks,omitempty"`
	Questions    []ProposalQuestion `json:"questions,omitempty" doc:"Each decision the goal left open that changes what counts, with the answer the proposal is planned on: among them any bound of a gate that depends on a number nobody has measured yet. A proposal with questions is never accepted by itself: a person confirms or changes each answer on accepting it, and the answers go into the brief."`
	// RepoMap is what the planner read of the repository, handed to the baseline agent so that it
	// does not read it all again.
	RepoMap string `json:"repo_map,omitempty" doc:"What the planner learned of the repository, for the baseline agent, which is given it when it starts: the files and directories that matter to the goal, the entry points and how the code builds and runs, the toolchain and dependency pins, a line each. Optional, at most 4000 bytes."`
}

// ProposalQuestion is one decision a goal left open, and the answer its planner planned on.
type ProposalQuestion struct {
	Question string `json:"question" doc:"The decision, in a sentence."`
	Answer   string `json:"answer" doc:"The answer the proposal is planned on, which the person confirms or changes."`
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

// maxProposedResearchers is the most researchers a proposal may name, and a draft's max_agents
// until one is accepted: of the maxHosted machines the platform runs for an accepted draft, one
// is its coordinator's and one its baseline agent's.
func maxProposedResearchers(maxHosted int) int {
	return maxHosted - 2
}

// proposedPool is the researchers the agents of a proposal make: an entry for each agent type and
// model they run on, as each is resolved when the platform creates it (domain.ResolveAgentLaunch).
func proposedPool(catalog domain.AgentTypeCatalog, agents []ProposedAgent) (domain.ResearcherPool, error) {
	keys := make([]domain.PoolKey, 0, len(agents))
	for i, agent := range agents {
		choice, err := domain.ResolveAgentLaunch(catalog, domain.AgentRoleResearcher, agent.Flavor, agent.Model, agent.Hyperparameters)
		if err != nil {
			return nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		keys = append(keys, domain.PoolKey{AgentType: choice.AgentType, Model: choice.Model})
	}
	return domain.PoolOf(keys), nil
}

// poolAgents is a researcher of each place of pool, as hostedAgents creates them.
func poolAgents(pool domain.ResearcherPool) []ProposedAgent {
	var agents []ProposedAgent
	for _, e := range pool {
		for range e.Count {
			agents = append(agents, ProposedAgent{Flavor: e.AgentType, Model: e.Model, Hyperparameters: e.Hyperparameters})
		}
	}
	return agents
}

// checkProposal is the mechanical check of a proposal: everything that can be decided from the
// proposal and the platform's config. It refuses with every problem it finds, so a planner fixes
// them in one pass. It returns the experiment the proposal states, as validate answers it: with
// its cost, its length and one place for each researcher. It leaves p as it is stored: every text
// of it without the whitespace around it, and the CPU type where it states no accelerator_type.
func (h *PlatformExperimentsHandler) checkProposal(ctx context.Context, p *Proposal, draft *domain.PlatformExperiment) (CreatePlatformExperimentRequest, error) {
	var problems api.Problems
	refuse := func(reason, format string, args ...any) {
		problems = append(problems, api.Refuse(http.StatusUnprocessableEntity, reason, fmt.Sprintf(format, args...)))
	}
	catalog, err := h.svc.store.AgentTypeCatalog(ctx, draft.ID)
	if err != nil {
		return CreatePlatformExperimentRequest{}, err
	}
	experiment, err := decodeExperimentBase(p.Experiment)
	decoded := err == nil
	if !decoded {
		refuse("invalid_experiment", "experiment: %s", err)
	}
	if most := maxProposedResearchers(draft.MaxHosted); len(p.Agents) == 0 || len(p.Agents) > most {
		refuse("invalid_agents", "agents: propose between 1 and %d researchers (the draft's max_agents), got %d", most, len(p.Agents))
	}
	least, limit := max(1, int(h.svc.cfg.MinBaseline.Minutes())), int(h.svc.cfg.BaselineLimit.Minutes())
	if p.BaselineMinutes < least || p.BaselineMinutes > limit {
		refuse("invalid_baseline_minutes", "baseline_minutes is %d: state the minutes measuring the baseline needs, from %d (planning.min_baseline_minutes) to %d (planning.baseline_limit_minutes). "+
			"The platform gives a baseline at least %d minutes because one written from nothing writes its harness, waits for the platform to build its environment and measures once in that time, and a shorter window closes the experiment before its run starts",
			p.BaselineMinutes, least, limit, least)
	}
	if decoded {
		// An accepted draft starts when it is accepted, whatever its experiment says of a start.
		experiment.StartsAt = time.Time{}
		experiment.BudgetAcceleratorHours, experiment.DurationHours, experiment.MaxAgents = p.BudgetAcceleratorHours, p.DurationHours, len(p.Agents)
		pool, unresolved := proposedPool(catalog, p.Agents)
		if err := problems.Add(unresolved); err != nil {
			return experiment, err
		}
		experiment.Researchers = pool
		var invalid error
		experiment, invalid = h.svc.validate(experiment, catalog)
		if err := problems.Add(invalid); err != nil {
			return experiment, err
		}
		if invalid == nil {
			if err := problems.Add(h.svc.requireContinuedMetrics(ctx, draft.Continues, experiment.Metrics)); err != nil {
				return experiment, err
			}
			if p.Experiment, err = withTexts(p.Experiment, experiment); err != nil {
				refuse("invalid_experiment", "experiment: %s", err)
			}
		}
	}
	p.AcceleratorType = domain.AcceleratorType(strings.TrimSpace(string(p.AcceleratorType)))
	if p.AcceleratorType == "" {
		p.AcceleratorType = domain.CPUType
	}
	clusters, err := h.svc.requireReachable(ctx, draft.ProjectID, &p.AcceleratorType)
	if err := problems.Add(err); err != nil {
		return experiment, err
	}
	if err == nil {
		if err := problems.Add(requireImage(h.jobImages, clusters, p.Image)); err != nil {
			return experiment, err
		}
	}
	p.CPU, p.Memory, p.Storage = strings.TrimSpace(p.CPU), strings.TrimSpace(p.Memory), strings.TrimSpace(p.Storage)
	if err := workload.CheckNodeRequest(p.CPU, p.Memory, p.Storage); err != nil {
		refuse("invalid_resources", "%s", err)
	}
	for i, agent := range p.Agents {
		if _, err := domain.ResolveAgentLaunch(catalog, domain.AgentRoleResearcher, agent.Flavor, agent.Model, agent.Hyperparameters); err != nil {
			refuse("invalid_agents", "agents[%d]: %s", i, err)
		}
	}
	explanations := map[string]*string{"metric_reason": &p.MetricReason, "measurement": &p.Measurement, "sizing_reason": &p.SizingReason}
	for _, name := range slices.Sorted(maps.Keys(explanations)) {
		text := explanations[name]
		*text = strings.TrimSpace(*text)
		if *text == "" {
			refuse("missing_explanation", "%s is required: the person who confirms reads it in place of the code", name)
		}
		if len(*text) > maxProposalTextBytes {
			refuse("too_long", "%s is %d bytes and at most %d", name, len(*text), maxProposalTextBytes)
		}
	}
	if p.RepoMap = strings.TrimSpace(p.RepoMap); len(p.RepoMap) > maxProposalTextBytes {
		refuse("too_long", "repo_map is %d bytes and at most %d: a line per file or directory that matters", len(p.RepoMap), maxProposalTextBytes)
	}
	for _, field := range []struct {
		name string
		list []string
	}{{"out_of_scope", p.OutOfScope}, {"risks", p.Risks}} {
		for i, entry := range field.list {
			field.list[i] = strings.TrimSpace(entry)
			if field.list[i] == "" || len(field.list[i]) > maxProposalTextBytes {
				refuse("too_long", "%s[%d]: each entry is a sentence of at most %d bytes", field.name, i, maxProposalTextBytes)
			}
		}
	}
	for i := range p.Questions {
		q := &p.Questions[i]
		q.Question, q.Answer = strings.TrimSpace(q.Question), strings.TrimSpace(q.Answer)
		if q.Question == "" || q.Answer == "" || len(q.Question) > maxProposalTextBytes || len(q.Answer) > maxProposalTextBytes {
			refuse("too_long", "questions[%d]: question and answer are each a sentence of at most %d bytes: the decision, and the answer the proposal is planned on", i, maxProposalTextBytes)
		}
	}
	return experiment, problems.Err()
}

// withTexts is the experiment of a proposal with its texts as validate answered them, so that
// the proposal a person reads states what accepting it stores.
func withTexts(experiment json.RawMessage, checked CreatePlatformExperimentRequest) (json.RawMessage, error) {
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(experiment))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("it is not a JSON object: %w", err)
	}
	fields["name"], fields["description"], fields["metrics"] = checked.Name, checked.Description, checked.Metrics
	if _, stated := fields["default_quota_tier"]; stated {
		fields["default_quota_tier"] = checked.DefaultQuotaTier
	}
	return json.Marshal(fields)
}

// requirePlanner admits only the planner of the draft id: the agent the platform signed up to
// plan it, with its own token.
func (h *PlatformExperimentsHandler) requirePlanner(ctx context.Context, id string) error {
	caller, err := h.caller(ctx)
	if err != nil {
		return err
	}
	if caller.Kind != auth.KindAgent || caller.PlatformExperimentID != id {
		return huma.Error403Forbidden("a proposal is written by the draft's planner, with its own token")
	}
	signup, err := h.svc.store.GetAgentSignup(ctx, id, caller.AgentID)
	if err != nil {
		return h.refusal("read planner", err)
	}
	if signup == nil || signup.Role != domain.SignupRolePlanner {
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
			"the proposal stays readable, proposed_at or declined_at is cleared with accepts_at, so nothing is accepted by itself, and the planner has the planning limit to propose anew. " +
			"The planner's machine is kept after its answer until the draft's planner_ends_at: a revision asked for before it reaches the planner that is still there, as its next update (platform_experiment.revision), and is answered with nothing to boot. After it the platform starts the planner again with what it knew. " +
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
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
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
		return h.Answer(ctx, pe)
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "accept-draft", Method: "POST", Path: "/platform-experiments/{id}/accept",
		Summary: "Accept a draft's proposal", Tags: []string{"platform-experiments"},
		Description: "Makes a proposed draft an open platform experiment in one write: its name, description, metrics, report interval, stages and policies are the proposal's; " +
			"its budget, its length, the minutes its baseline has and its researchers are the ones this body states, whatever the proposal proposed, because an agent wrote the proposal. " +
			"name, when the body states one, replaces the proposal's name and nothing else of it (422 invalid_experiment for one that is no name). " +
			"answers holds one answer for each of the proposal's questions, in their order, as the person confirmed or changed it: each question and its answer are added to the end of the brief, a line each, so every agent reads the decisions as made. 422 invalid_answers when it does not hold one non-empty answer for each question. " +
			"proposal stays on the platform experiment as it was proposed, for anyone who reads it, beside accepted_at, accepted_by (absent when the platform accepted it by itself) and accepted_answers. " +
			"The budget caps everything the platform experiment spends, the baseline included: what measuring it cost is taken from the budget, the rest is shared among the researchers, and an agent's burst hours run on idle capacity and come out of its share, never on top of it. " +
			"The platform hosts its coordinator, each researcher and one agent of role baseline, named by its role, which the proposal signed up so that its machine starts while the proposal is read, and which is created here for a proposal that signed none up. " + coordinatorWanted + " " +
			"starts_at is now and it is open until baseline_due_at, baseline_minutes from now, and one not running by then is closed with close_reason baseline_limit_reached. The run lasts duration_hours from the moment it starts, and measuring the baseline takes nothing from it. Nothing has run yet: the baseline agent measures the baseline, the platform declares the job it leaves when it finishes and starts the run, " +
			"and no researcher's machine is started before. Where that agent leaves no job, or several, the coordinator adds another or declares one (PATCH /platform-experiments/{id}, baseline_job_id), and status_reason says so. " +
			"The platform makes the same accept by itself at accepts_at, at the proposal's own budget, length, baseline_minutes and researchers, for the person who created the draft, unless a person accepted, held, revised or closed it first; one the check refuses then waits for a person. A proposal with questions has no accepts_at and waits for a person to answer them. " +
			"409 invalid_transition for anything but a draft with a proposal; 422 with the proposal's reasons when the stated cost, baseline_minutes or agents do not pass its check, " +
			"accelerator_unreachable among them when the proposal's accelerator_type is on no cluster the org's jobs can be placed on now, and no_cluster_for_image when none that has it runs the proposal's image any more; " +
			"503 hosting_not_configured when nothing could run it. A draft's own planner cannot accept it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body AcceptRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		by := caller.Actor()
		pe, err := h.accept(ctx, in.ID, in.Body, by, &by)
		if err != nil {
			return nil, err
		}
		return h.Answer(ctx, pe)
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "hold-draft", Method: "POST", Path: "/platform-experiments/{id}/hold",
		Summary: "Keep the platform from accepting a draft's proposal by itself", Tags: []string{"platform-experiments"},
		Description: "For a person who edits the proposal before accepting it: accepts_at is cleared, status_reason reads proposed, and the draft waits for the person to accept it (POST /platform-experiments/{id}/accept, with what they changed), revise it or close it. " +
			"The baseline agent the proposal signed up is let go of its machine until then, and is given one again with the accept. Holding a draft that is held changes nothing. " +
			"409 invalid_transition for anything but a draft with a proposal standing.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		switch err := h.svc.store.HoldAccept(ctx, in.ID); {
		case errors.Is(err, db.ErrNotDraft):
			return nil, invalidTransition("platform experiment %s is not a draft: only a draft's proposal is held", in.ID)
		case errors.Is(err, db.ErrNotProposed):
			return nil, invalidTransition("draft %s has no proposal standing to hold", in.ID)
		case err != nil:
			return nil, h.refusal("hold", err)
		}
		pe, err := h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil || pe == nil {
			return nil, h.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
		}
		return h.Answer(ctx, pe)
	})
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "put-proposal", Method: "PUT", Path: "/platform-experiments/{id}/proposal",
		Summary: "Propose the platform experiment a draft becomes", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after). " +
			"experiment is the experiment as its file would state it; budget_accelerator_hours, duration_hours, baseline_minutes and agents are proposed, and the person who accepts states them. " +
			"A proposal defines the experiment and runs nothing: its brief states the objective and the measure and nothing measured yet, its numbers are estimates, " +
			"and the baseline is measured once it is accepted, before any researcher starts, by the baseline agent the proposal signs up, whose machine starts with it. " +
			"The proposal is checked before it is stored, its experiment and its cost by the rules of POST /platform-experiments, and refused with 422 and a reason the planner can act on: invalid_experiment, invalid_metrics, invalid_stages, " +
			"invalid_cost, invalid_agents, invalid_length, run_too_long (duration_hours over the platform's quota.max_platform_experiment_days), invalid_baseline_minutes (under planning.min_baseline_minutes, or over planning.baseline_limit_minutes), missing_explanation, too_long, " +
			"accelerator_unreachable for an accelerator_type no cluster this org's jobs can be placed on has now, naming every type that is, and for image the two refusals a job submit gives: " +
			"unknown_job_image for one that is neither a job image the platform offers nor a whole registry address, and no_cluster_for_image for one no cluster that has the type runs. Stored, it replaces the one before, marks the draft proposed " +
			"(proposed_at), and the planner's machine is released: GET /platform-experiments/{id} shows the proposal, and the platform accepts it as proposed at accepts_at (planning.auto_accept_seconds from now) unless a person accepts it sooner, holds it, revises it or closes it. A proposal with questions, or one made with no auto-accept configured, has no accepts_at and waits for a person to accept it, and its baseline agent's machine starts with the accept. " +
			"A refusal lists every problem the check found, each in problems with its reason and message, the first also as the refusal's own reason. " +
			"With check=true the proposal is checked exactly so and nothing is stored: 422 with every problem, or 200 with the draft as it stands, so a planner tries variants with check and proposes only the one it means.",
	}, func(ctx context.Context, in *struct {
		ID    string `path:"id"`
		Check bool   `query:"check" doc:"Check the proposal as a proposal is checked and store nothing: the answer is the draft as it stands, or 422 with every problem."`
		Body  Proposal
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
		if _, err := h.checkProposal(ctx, &in.Body, pe); err != nil {
			return nil, h.refusal("check proposal", err)
		}
		if in.Check {
			return h.Answer(ctx, pe)
		}
		stored, err := json.Marshal(in.Body)
		if err != nil {
			return nil, h.refusal("encode proposal", err)
		}
		proposed, err := h.proposed(ctx, pe, &in.Body)
		if err != nil {
			return nil, h.refusal("store proposal", err)
		}
		err = h.svc.store.SetProposal(ctx, in.ID, stored, proposed)
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
		return h.Answer(ctx, pe)
	})
}

// proposed is what proposal p of draft pe stored now writes beside it: until when it waits, when
// the platform accepts it by itself, and the agent that is to measure its baseline, signed up for
// the person who created the draft so that its machine starts while the proposal is read. One
// with questions is never accepted by itself, since only a person can answer them.
func (h *PlatformExperimentsHandler) proposed(ctx context.Context, pe *domain.PlatformExperiment, p *Proposal) (db.Proposed, error) {
	now := time.Now().UTC()
	proposed := db.Proposed{WaitUntil: now.Add(h.planning.ProposedFor), KeepPlanner: h.planning.PlannerKept}
	if h.planning.AutoAccept > 0 && len(p.Questions) == 0 {
		at := now.Add(h.planning.AutoAccept)
		proposed.AcceptsAt = &at
	}
	if !h.launch.Hosting || h.launch.DefaultModel == "" {
		return proposed, nil
	}
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	catalog, err := h.svc.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return proposed, err
	}
	agents, signups, err := h.hostedAgents(catalog, nil, by, now)
	if err != nil {
		return proposed, err
	}
	proposed.Baseline, proposed.BaselineSignup = agents[0], signups[0]
	return proposed, nil
}

// AcceptRequest is what the person who accepts a draft states themselves: what the run costs,
// how long it and measuring its baseline last, and who runs it. The proposal only proposed them,
// since an agent wrote it.
type AcceptRequest struct {
	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"The budget of the run: a cap on everything it spends, with what measuring the baseline costs and burst hours included."`
	DurationHours          float64         `json:"duration_hours" doc:"How long the run lasts from the moment it starts, once its baseline is declared."`
	BaselineMinutes        int             `json:"baseline_minutes" doc:"The minutes measuring the baseline has from this accept, at least planning.min_baseline_minutes and at most planning.baseline_limit_minutes: baseline_due_at is that much later, and moves out by the time the platform spends building and starting the baseline's jobs."`
	Agents                 []ProposedAgent `json:"agents" doc:"The researchers the platform runs, one entry each."`
	Name                   string          `json:"name,omitempty" doc:"The name of the platform experiment, when the person changed the one proposed. One line, held to the rule of name on POST /platform-experiments. Left out, the proposal's name stands. Nothing else of the proposal's experiment is changed here but by answers: its metrics and the rest of its brief change by a revision."`
	Answers                []string        `json:"answers,omitempty" doc:"One answer for each of the proposal's questions, in their order: the answer the planner planned on, or the person's own. Each question and its answer are added to the end of the brief. Left out for a proposal without questions."`
}

// answered is the proposal's experiment with each question and its answer added to the end of
// its brief, a line each, so that every agent reads the decisions the person made, and the
// answers as the brief states them.
func answered(experiment json.RawMessage, questions []ProposalQuestion, answers []string) (json.RawMessage, []string, error) {
	if len(answers) != len(questions) {
		return nil, nil, refusedProposal("invalid_answers", "answers: the proposal has %d questions and the accept answers %d: answer each, in their order", len(questions), len(answers))
	}
	if len(questions) == 0 {
		return experiment, nil, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(experiment, &fields); err != nil {
		return nil, nil, err
	}
	var brief string
	if err := json.Unmarshal(fields["description"], &brief); err != nil {
		return nil, nil, err
	}
	lines := []string{strings.TrimRight(brief, "\n"), "", "Decisions the owner made before the run:"}
	given := make([]string, len(questions))
	for i, q := range questions {
		answer := strings.Join(strings.Fields(answers[i]), " ")
		if answer == "" || len(answer) > maxProposalTextBytes {
			return nil, nil, refusedProposal("invalid_answers", "answers[%d]: answer %q in a sentence of at most %d bytes", i, q.Question, maxProposalTextBytes)
		}
		given[i] = answer
		lines = append(lines, "- "+strings.Join(strings.Fields(q.Question), " ")+" Answer: "+answer)
	}
	var err error
	if fields["description"], err = json.Marshal(strings.Join(lines, "\n")); err != nil {
		return nil, nil, err
	}
	experiment, err = json.Marshal(fields)
	return experiment, given, err
}

// named is the proposal's experiment under the name the person who accepts gave it.
func named(experiment json.RawMessage, name string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(experiment, &fields); err != nil {
		return nil, err
	}
	var err error
	if fields["name"], err = json.Marshal(name); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func invalidTransition(format string, args ...any) error {
	return api.Refuse(http.StatusConflict, "invalid_transition", fmt.Sprintf(format, args...))
}

// accept makes the draft id the open platform experiment its proposal describes, at the cost
// req states, with a hosted coordinator, its researchers and the agent that measures its
// baseline, in one write. by owns its agents; acceptedBy is the person who accepted, nil when the
// platform accepts it as proposed.
func (h *PlatformExperimentsHandler) accept(ctx context.Context, id string, req AcceptRequest, by domain.Actor, acceptedBy *domain.Actor) (*domain.PlatformExperiment, error) {
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
	if pe.DeclinedAt != nil {
		return nil, invalidTransition("draft %s has no proposal to accept: its planner declined (declined_reason). Revise it with POST /platform-experiments/%s/revise, or close it", id, id)
	}
	if pe.ProposedAt == nil {
		return nil, invalidTransition("draft %s has no proposal to accept yet: its planner is still working", id)
	}
	var proposal Proposal
	if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
		return nil, h.refusal("decode stored proposal", err)
	}
	// The check the proposal passed, again with the cost and the agents the person states.
	proposal.BudgetAcceleratorHours, proposal.DurationHours, proposal.BaselineMinutes, proposal.Agents = req.BudgetAcceleratorHours, req.DurationHours, req.BaselineMinutes, req.Agents
	if req.Name != "" {
		if proposal.Experiment, err = named(proposal.Experiment, req.Name); err != nil {
			return nil, h.refusal("name the proposed experiment", err)
		}
	}
	var answers []string
	if proposal.Experiment, answers, err = answered(proposal.Experiment, proposal.Questions, req.Answers); err != nil {
		return nil, h.refusal("answer the proposal's questions", err)
	}
	now := time.Now().UTC()
	experiment, err := h.checkProposal(ctx, &proposal, pe)
	if err != nil {
		return nil, h.refusal("check proposal", err)
	}
	if h.launch.DefaultModel == "" || h.launch.CoordinatorModel == "" {
		return nil, h.refusal("accept", errors.New("the handler has no agent launch settings"))
	}
	if err := requireHosting(h.launch.Hosting); err != nil {
		return nil, h.refusal("accept", err)
	}

	pe.Name, pe.Description, pe.Metrics, pe.ReportIntervalSeconds = experiment.Name, experiment.Description, experiment.Metrics, experiment.ReportIntervalSeconds
	pe.Stages, pe.DefaultQuotaTier = experiment.Stages, domain.QuotaTier(experiment.DefaultQuotaTier)
	pe.MaxConcurrentAccelerators = experiment.MaxConcurrentAccelerators
	pe.BudgetAcceleratorHours, pe.MaxAgents, pe.Researchers, pe.DurationHours = experiment.BudgetAcceleratorHours, experiment.MaxAgents, experiment.Researchers, experiment.DurationHours
	pe.StartsAt, pe.BaselineDueAt = now, now.Add(time.Duration(req.BaselineMinutes)*time.Minute)
	pe.CoordinatorHosted, pe.Coordinator = true, &domain.HostedCoordinator{}
	pe.AcceptedAt, pe.AcceptedBy, pe.AcceptedAnswers = &now, acceptedBy, answers

	catalog, err := h.svc.store.AgentTypeCatalog(ctx, id)
	if err != nil {
		return nil, h.refusal("accept", err)
	}
	agents, signups, err := h.hostedAgents(catalog, req.Agents, by, now)
	if err != nil {
		return nil, h.refusal("accept", err)
	}
	// The baseline agent was signed up with the proposal, and its machine may be up already.
	signed, err := h.svc.store.ListAgentSignups(ctx, id)
	if err != nil {
		return nil, h.refusal("accept", err)
	}
	if slices.ContainsFunc(signed, func(s domain.AgentSignup) bool { return s.Role == domain.SignupRoleBaseline }) {
		agents, signups = agents[:len(agents)-1], signups[:len(signups)-1]
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
	h.logger.Info("draft accepted", zap.String("id", id), zap.Int("researchers", len(req.Agents)), zap.Float64("budget_accelerator_hours", pe.BudgetAcceleratorHours))
	accepted, err := h.svc.store.GetPlatformExperiment(ctx, id)
	if err != nil || accepted == nil {
		return nil, h.refusal("read accepted", errors.Join(err, errors.New("the platform experiment is gone")))
	}
	return accepted, nil
}

// nameActors names whoever created pe, closed it, accepted its proposal, asked for its revisions
// and raised its baseline, when it carries them.
func (h *PlatformExperimentsHandler) nameActors(ctx context.Context, pe *domain.PlatformExperiment) error {
	var actors []*domain.Actor
	if pe.Actor != nil {
		actors = append(actors, pe.Actor)
	}
	if pe.ClosedBy != nil {
		actors = append(actors, pe.ClosedBy)
	}
	if pe.AcceptedBy != nil {
		actors = append(actors, pe.AcceptedBy)
	}
	for i := range pe.Revisions {
		actors = append(actors, &pe.Revisions[i].Actor)
	}
	for i := range pe.BaselineRaises {
		actors = append(actors, &pe.BaselineRaises[i].Actor)
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
			"Stored, it answers the goal as a proposal does: the draft carries declined_reason and declined_at and no proposed_at, its status_reason reads declined, a proposal made before is cleared so there is nothing to accept, " +
			"the planner's machine is kept until planner_ends_at for a revision asked for soon and released then, and the person revises the draft or closes it. It waits for that until proposal_expires_at, as a proposal does, and the platform closes it then with close_reason proposal_expired. The next proposal clears the reason.",
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
		err = h.svc.store.Decline(ctx, in.ID, reason, time.Now().UTC().Add(h.planning.ProposedFor), h.planning.PlannerKept)
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
		return h.Answer(ctx, pe)
	})
}

func planningOver(pe *domain.PlatformExperiment) error {
	return api.Refuse(http.StatusConflict, "planning_over", "the planning limit of draft "+pe.ID+" passed at "+pe.ClosesAt().UTC().Format(time.RFC3339)+": it takes no proposal and no decline, and is closed")
}

func notADraft(pe *domain.PlatformExperiment) error {
	return api.Refuse(http.StatusConflict, "not_a_draft", "platform experiment "+pe.ID+" is "+string(pe.Status)+": only a draft is proposed, revised or accepted")
}

// AcceptDue accepts, as proposed, every draft whose accepts_at has passed, for the person who
// created it. One the check refuses now, its accelerator type gone from every cluster for one,
// is held: it waits for a person, who is told why on accepting it. One that cannot be accepted
// for a fault of the platform's is tried again on the next pass.
func (h *PlatformExperimentsHandler) AcceptDue(ctx context.Context) error {
	ids, err := h.svc.store.DraftsToAccept(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		log := h.logger.With(zap.String("platform_experiment_id", id))
		err := h.acceptAsProposed(ctx, id)
		var refused *api.Refusal
		switch {
		case err == nil:
			log.Info("draft accepted as proposed")
		case errors.As(err, &refused) && refused.GetStatus() < http.StatusInternalServerError:
			log.Warn("draft not accepted as proposed: it waits for a person", zap.String("reason", refused.Reason), zap.String("message", refused.Message))
			if err := h.svc.store.HoldAccept(ctx, id); err != nil && !errors.Is(err, db.ErrNotDraft) && !errors.Is(err, db.ErrNotProposed) {
				log.Error("hold the draft", zap.Error(err))
			}
		default:
			log.Error("accept the draft as proposed", zap.Error(err))
		}
	}
	return nil
}

// acceptAsProposed accepts the draft id at the cost, length and researchers its proposal states.
func (h *PlatformExperimentsHandler) acceptAsProposed(ctx context.Context, id string) error {
	pe, err := h.svc.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil || pe.Status != domain.PlatformExpDraft || pe.ProposedAt == nil {
		return nil
	}
	var proposal Proposal
	if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
		return fmt.Errorf("decode stored proposal: %w", err)
	}
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	_, err = h.accept(ctx, id, AcceptRequest{
		BudgetAcceleratorHours: proposal.BudgetAcceleratorHours, DurationHours: proposal.DurationHours,
		BaselineMinutes: proposal.BaselineMinutes, Agents: proposal.Agents,
	}, by, nil)
	return err
}

// StartAutoAccept runs AcceptDue every interval until ctx is cancelled, each pass bounded by
// passTimeout.
func (h *PlatformExperimentsHandler) StartAutoAccept(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			if err := h.AcceptDue(passCtx); err != nil {
				h.logger.Error("accept_due: scan", zap.Error(err))
			}
			cancel()
		}
	}
}
