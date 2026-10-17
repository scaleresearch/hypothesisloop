package platformexperiments

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

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// Proposal is what a draft's planner proposes: the experiment as its file would state it, and
// beside it what a run costs, which is only ever proposed. A person states the budget, the
// duration and the agents again when accepting, by the rule that keeps them out of the
// experiment file: an agent's token wrote this. One without questions the platform accepts as
// proposed after planning.auto_accept_seconds unless a person acts first; one with questions
// waits for a person to answer them. Nothing has run when it is written:
// the baseline is measured once it is accepted, before any researcher starts.
type Proposal struct {
	GoalVersion int `json:"goal_version" doc:"The goal_version of the draft the proposal was planned from. 409 goal_changed once the draft has another goal: read it and propose for it."`

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
	AgentType       string         `json:"agent_type,omitempty"`
	Model           string         `json:"model,omitempty"`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty"`
}

// MaxProposalTextBytes bounds each plain-words field of a proposal, and each entry of its lists.
const MaxProposalTextBytes = 4000

// refusedProposal is a proposal the planner can fix: reason names what is wrong.
func refusedProposal(reason, format string, args ...any) error {
	return refusal.Refuse(http.StatusUnprocessableEntity, reason, fmt.Sprintf(format, args...))
}

// MaxProposedResearchers is the most researchers a proposal may name, and a draft's max_agents
// until one is accepted: of the maxAgentSandboxes machines the platform runs for an accepted draft, one
// is its coordinator's and one its baseline agent's.
func MaxProposedResearchers(maxAgentSandboxes int) int {
	return maxAgentSandboxes - 2
}

// proposedPool is the researchers the agents of a proposal make: an entry for each agent type and
// model they run on, as each is resolved when the platform creates it (domain.ResolveAgentLaunch).
func proposedPool(catalog domain.AgentTypeCatalog, agents []ProposedAgent) (domain.ResearcherPool, error) {
	keys := make([]domain.PoolKey, 0, len(agents))
	for i, agent := range agents {
		choice, err := domain.ResolveAgentLaunch(catalog, domain.AgentRoleResearcher, agent.AgentType, agent.Model, agent.Hyperparameters)
		if err != nil {
			return nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		keys = append(keys, domain.PoolKey{AgentType: choice.AgentType, Model: choice.Model})
	}
	return domain.PoolOf(keys), nil
}

// poolAgents is a researcher of each place of pool, as sandboxedAgents creates them.
func poolAgents(pool domain.ResearcherPool) []ProposedAgent {
	agents := []ProposedAgent{}
	for _, e := range pool {
		for range e.Count {
			agents = append(agents, ProposedAgent{AgentType: e.AgentType, Model: e.Model, Hyperparameters: e.Hyperparameters})
		}
	}
	return agents
}

// checkProposal is the mechanical check of a proposal: everything that can be decided from the
// proposal and the platform's config. It refuses with every problem it finds, so a planner fixes
// them in one pass. It returns the experiment the proposal states, as validate answers it: with
// its cost, its length and one place for each researcher. It leaves p as it is stored: every text
// of it without the whitespace around it, and the CPU type where it states no accelerator_type.
func (s *Service) checkProposal(ctx context.Context, p *Proposal, draft *domain.PlatformExperiment) (CreatePlatformExperimentRequest, error) {
	var problems refusal.Problems
	refuse := func(reason, format string, args ...any) {
		problems = append(problems, refusal.Refuse(http.StatusUnprocessableEntity, reason, fmt.Sprintf(format, args...)))
	}
	catalog, err := s.store.AgentTypeCatalog(ctx, draft.ID)
	if err != nil {
		return CreatePlatformExperimentRequest{}, err
	}
	spec, err := experimentspec.Parse(p.Experiment)
	experiment := CreatePlatformExperimentRequest{Spec: spec}
	decoded := err == nil
	if !decoded {
		refuse("invalid_experiment", "experiment: %s", err)
	}
	if most := MaxProposedResearchers(draft.MaxAgentSandboxes); len(p.Agents) == 0 || len(p.Agents) > most {
		refuse("invalid_agents", "agents: propose between 1 and %d researchers (the draft's max_agents), got %d", most, len(p.Agents))
	}
	least, limit := max(1, int(s.cfg.MinBaseline.Minutes())), int(s.cfg.BaselineLimit.Minutes())
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
		experiment, invalid = s.validate(experiment, catalog)
		if err := problems.Add(invalid); err != nil {
			return experiment, err
		}
		if invalid == nil {
			if err := problems.Add(s.requireContinuedMetrics(ctx, draft.Continues, experiment.Metrics)); err != nil {
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
	clusters, err := s.requireReachable(ctx, draft.ProjectID, &p.AcceleratorType)
	if err := problems.Add(err); err != nil {
		return experiment, err
	}
	if err == nil {
		if err := problems.Add(requireImage(s.jobImages, clusters, p.Image)); err != nil {
			return experiment, err
		}
	}
	p.CPU, p.Memory, p.Storage = strings.TrimSpace(p.CPU), strings.TrimSpace(p.Memory), strings.TrimSpace(p.Storage)
	if err := domain.CheckNodeRequest(p.CPU, p.Memory, p.Storage); err != nil {
		refuse("invalid_resources", "%s", err)
	}
	for i, agent := range p.Agents {
		if _, err := domain.ResolveAgentLaunch(catalog, domain.AgentRoleResearcher, agent.AgentType, agent.Model, agent.Hyperparameters); err != nil {
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
		if len(*text) > MaxProposalTextBytes {
			refuse("too_long", "%s is %d bytes and at most %d", name, len(*text), MaxProposalTextBytes)
		}
	}
	if p.RepoMap = strings.TrimSpace(p.RepoMap); len(p.RepoMap) > MaxProposalTextBytes {
		refuse("too_long", "repo_map is %d bytes and at most %d: a line per file or directory that matters", len(p.RepoMap), MaxProposalTextBytes)
	}
	for _, field := range []struct {
		name string
		list []string
	}{{"out_of_scope", p.OutOfScope}, {"risks", p.Risks}} {
		for i, entry := range field.list {
			field.list[i] = strings.TrimSpace(entry)
			if field.list[i] == "" || len(field.list[i]) > MaxProposalTextBytes {
				refuse("too_long", "%s[%d]: each entry is a sentence of at most %d bytes", field.name, i, MaxProposalTextBytes)
			}
		}
	}
	for i := range p.Questions {
		q := &p.Questions[i]
		q.Question, q.Answer = strings.TrimSpace(q.Question), strings.TrimSpace(q.Answer)
		if q.Question == "" || q.Answer == "" || len(q.Question) > MaxProposalTextBytes || len(q.Answer) > MaxProposalTextBytes {
			refuse("too_long", "questions[%d]: question and answer are each a sentence of at most %d bytes: the decision, and the answer the proposal is planned on", i, MaxProposalTextBytes)
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

// ErrNotThePlanner refuses a proposal or a decline from anyone but the draft's planner.
var ErrNotThePlanner = errors.New("a proposal is written by the draft's planner, with its own token")

// requirePlanner admits only the planner of the draft id: the agent the platform signed up to
// plan it, with its own token.
func (s *Service) requirePlanner(ctx context.Context, id string, caller auth.Principal) error {
	if caller.Kind != auth.KindAgent || caller.PlatformExperimentID != id {
		return ErrNotThePlanner
	}
	signup, err := s.store.GetAgentSignup(ctx, id, caller.AgentID)
	if err != nil {
		return s.refusal("read planner", err)
	}
	if signup == nil || signup.Role != domain.SignupRolePlanner {
		return ErrNotThePlanner
	}
	return nil
}

// Accept makes the draft id the open platform experiment its proposal describes, at what req
// states, for by, who accepted it.
func (s *Service) Accept(ctx context.Context, id string, req AcceptRequest, by domain.Actor) (*domain.PlatformExperiment, error) {
	return s.accept(ctx, id, req, by, &by)
}

// Hold keeps the platform from accepting the proposal of the draft id by itself.
func (s *Service) Hold(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	switch err := s.store.HoldAccept(ctx, id); {
	case errors.Is(err, db.ErrNotDraft):
		return nil, invalidTransition("platform experiment %s is not a draft: only a draft's proposal is held", id)
	case errors.Is(err, db.ErrNotProposed):
		return nil, invalidTransition("draft %s has no proposal standing to hold", id)
	case err != nil:
		return nil, s.refusal("hold", err)
	}
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil || pe == nil {
		return nil, s.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
	}
	return pe, nil
}

// Propose stores p as the proposal of the draft id, written by its planner caller, or with check
// only checks it and answers the draft as it stands.
func (s *Service) Propose(ctx context.Context, id string, caller auth.Principal, p *Proposal, check bool) (*domain.PlatformExperiment, error) {
	if err := s.requirePlanner(ctx, id, caller); err != nil {
		return nil, err
	}
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, s.refusal("read draft", err)
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	if pe.Status != domain.PlatformExpDraft {
		return nil, notADraft(pe)
	}
	if _, err := s.checkProposal(ctx, p, pe); err != nil {
		return nil, s.refusal("check proposal", err)
	}
	if check {
		// A check writes nothing, so the store's refusal of a goal that changed is not reached.
		if p.GoalVersion != pe.GoalVersion {
			return nil, goalChanged(pe)
		}
		return pe, nil
	}
	stored, err := json.Marshal(p)
	if err != nil {
		return nil, s.refusal("encode proposal", err)
	}
	proposed, err := s.proposed(ctx, pe, p)
	if err != nil {
		return nil, s.refusal("store proposal", err)
	}
	if err := s.refusedAnswer(ctx, pe, s.store.SetProposal(ctx, id, p.GoalVersion, stored, proposed)); err != nil {
		return nil, s.refusal("store proposal", err)
	}
	s.logger.Info("draft proposed", zap.String("id", id))
	pe, err = s.store.GetPlatformExperiment(ctx, id)
	if err != nil || pe == nil {
		return nil, s.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
	}
	return pe, nil
}

// MaxDeclineBytes bounds the reason a planner gives for proposing nothing.
const MaxDeclineBytes = MaxProposalTextBytes

// Decline records that the planner caller of the draft id proposes nothing for goal goalVersion,
// and why.
func (s *Service) Decline(ctx context.Context, id string, caller auth.Principal, goalVersion int, reason string) (*domain.PlatformExperiment, error) {
	if err := s.requirePlanner(ctx, id, caller); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > MaxDeclineBytes {
		return nil, refusedProposal("missing_explanation", "reason is required and at most %d bytes: the person who wrote the goal reads it in place of a proposal", MaxDeclineBytes)
	}
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, s.refusal("read draft", err)
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	if err := s.refusedAnswer(ctx, pe, s.store.Decline(ctx, id, goalVersion, reason, time.Now().UTC().Add(s.planning.ProposedFor), s.planning.PlannerKept)); err != nil {
		return nil, s.refusal("store decline", err)
	}
	s.logger.Info("draft declined", zap.String("id", id))
	pe, err = s.store.GetPlatformExperiment(ctx, id)
	if err != nil || pe == nil {
		return nil, s.refusal("read draft", errors.Join(err, errors.New("the draft is gone")))
	}
	return pe, nil
}

// proposed is what proposal p of draft pe stored now writes beside it: until when it waits, when
// the platform accepts it by itself, and the agent that is to measure its baseline, signed up for
// the person who created the draft so that its machine starts while the proposal is read. One
// with questions is never accepted by itself, since only a person can answer them.
func (s *Service) proposed(ctx context.Context, pe *domain.PlatformExperiment, p *Proposal) (db.Proposed, error) {
	now := time.Now().UTC()
	proposed := db.Proposed{WaitUntil: now.Add(s.planning.ProposedFor), KeepPlanner: s.planning.PlannerKept}
	if s.planning.AutoAccept > 0 && len(p.Questions) == 0 {
		at := now.Add(s.planning.AutoAccept)
		proposed.AcceptsAt = &at
	}
	if !s.launch.AgentSandbox || s.launch.DefaultModel == "" {
		return proposed, nil
	}
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return proposed, err
	}
	agents, signups, err := sandboxedAgents(catalog, nil, by, now)
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
	Name                   string          `json:"name,omitempty" doc:"The name of the platform experiment, when the person changed the one proposed. One line, held to the rule of name on POST /platform-experiments. Left out, the proposal's name stands. Nothing else of the proposal's experiment is changed here but by answers: its metrics and the rest of its brief change by a change of the goal (PATCH /platform-experiments/{id} with goal), which the planner plans again."`
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
		if answer == "" || len(answer) > MaxProposalTextBytes {
			return nil, nil, refusedProposal("invalid_answers", "answers[%d]: answer %q in a sentence of at most %d bytes", i, q.Question, MaxProposalTextBytes)
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
	return refusal.Refuse(http.StatusConflict, "invalid_transition", fmt.Sprintf(format, args...))
}

// accept makes the draft id the open platform experiment its proposal describes, at the cost
// req states, with a sandboxed coordinator, its researchers and the agent that measures its
// baseline, in one write. by owns its agents; acceptedBy is the person who accepted, nil when the
// platform accepts it as proposed.
func (s *Service) accept(ctx context.Context, id string, req AcceptRequest, by domain.Actor, acceptedBy *domain.Actor) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, s.refusal("read draft", err)
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	if err := acceptable(pe); err != nil {
		return nil, err
	}
	var proposal Proposal
	if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
		return nil, s.refusal("decode stored proposal", err)
	}
	// The check the proposal passed, again with the cost and the agents the person states.
	proposal.BudgetAcceleratorHours, proposal.DurationHours, proposal.BaselineMinutes, proposal.Agents = req.BudgetAcceleratorHours, req.DurationHours, req.BaselineMinutes, req.Agents
	if req.Name != "" {
		if proposal.Experiment, err = named(proposal.Experiment, req.Name); err != nil {
			return nil, s.refusal("name the proposed experiment", err)
		}
	}
	var answers []string
	if proposal.Experiment, answers, err = answered(proposal.Experiment, proposal.Questions, req.Answers); err != nil {
		return nil, s.refusal("answer the proposal's questions", err)
	}
	now := time.Now().UTC()
	experiment, err := s.checkProposal(ctx, &proposal, pe)
	if err != nil {
		return nil, s.refusal("check proposal", err)
	}
	if s.launch.DefaultModel == "" || s.launch.CoordinatorModel == "" {
		return nil, s.refusal("accept", errors.New("the handler has no agent launch settings"))
	}
	if err := requireAgentSandbox(s.launch.AgentSandbox); err != nil {
		return nil, s.refusal("accept", err)
	}

	pe.Name, pe.Description, pe.Metrics, pe.ReportIntervalSeconds = experiment.Name, experiment.Description, experiment.Metrics, experiment.ReportIntervalSeconds
	pe.Stages, pe.DefaultQuotaTier = experiment.Stages, experiment.DefaultQuotaTier
	pe.MaxConcurrentAccelerators = experiment.MaxConcurrentAccelerators
	pe.BudgetAcceleratorHours, pe.MaxAgents, pe.Researchers, pe.DurationHours = experiment.BudgetAcceleratorHours, experiment.MaxAgents, experiment.Researchers, experiment.DurationHours
	pe.StartsAt, pe.BaselineDueAt = now, now.Add(time.Duration(req.BaselineMinutes)*time.Minute)
	pe.CoordinatorInAgentSandbox, pe.Coordinator = true, &domain.AgentSandboxCoordinator{}
	pe.AcceptedAt, pe.AcceptedBy, pe.AcceptedAnswers = &now, acceptedBy, answers

	catalog, err := s.store.AgentTypeCatalog(ctx, id)
	if err != nil {
		return nil, s.refusal("accept", err)
	}
	agents, signups, err := sandboxedAgents(catalog, req.Agents, by, now)
	if err != nil {
		return nil, s.refusal("accept", err)
	}
	// The baseline agent was signed up with the proposal, and its machine may be up already.
	signed, err := s.store.ListAgentSignups(ctx, id)
	if err != nil {
		return nil, s.refusal("accept", err)
	}
	if slices.ContainsFunc(signed, func(s domain.AgentSignup) bool { return s.Role == domain.SignupRoleBaseline }) {
		agents, signups = agents[:len(agents)-1], signups[:len(signups)-1]
	}
	switch err := s.store.AcceptDraft(ctx, pe, agents, signups); {
	case errors.Is(err, db.ErrNotDraft):
		return nil, invalidTransition("platform experiment %s is no longer a draft", id)
	case errors.Is(err, db.ErrNothingProposed):
		return nil, invalidTransition("draft %s has no proposal to accept", id)
	case errors.Is(err, db.ErrMaxAgentSandboxesReached):
		return nil, refusedProposal("invalid_agents", "%s", maxAgentSandboxesReached(pe.MaxAgentSandboxes))
	case err != nil:
		return nil, s.refusal("accept", err)
	}
	s.logger.Info("draft accepted", zap.String("id", id), zap.Int("researchers", len(req.Agents)), zap.Float64("budget_accelerator_hours", pe.BudgetAcceleratorHours))
	accepted, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil || accepted == nil {
		return nil, s.refusal("read accepted", errors.Join(err, errors.New("the platform experiment is gone")))
	}
	return accepted, nil
}

// nameActors names whoever created pe, closed it, accepted its proposal and raised its baseline,
// when it carries them.
func (s *Service) nameActors(ctx context.Context, pe *domain.PlatformExperiment) error {
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
	for i := range pe.BaselineRaises {
		actors = append(actors, &pe.BaselineRaises[i].Actor)
	}
	if len(actors) == 0 {
		return nil
	}
	return s.store.NameActors(ctx, actors)
}

func planningOver(pe *domain.PlatformExperiment) error {
	return refusal.Refuse(http.StatusConflict, "planning_over", "the planning limit of draft "+pe.ID+" passed at "+pe.ClosesAt().UTC().Format(time.RFC3339)+": it takes no proposal and no decline, and is closed")
}

func notADraft(pe *domain.PlatformExperiment) error {
	return refusal.Refuse(http.StatusConflict, "not_a_draft", "platform experiment "+pe.ID+" is "+string(pe.Status)+": only a draft is proposed or accepted")
}

func goalChanged(pe *domain.PlatformExperiment) error {
	return refusal.Refuse(http.StatusConflict, "goal_changed", fmt.Sprintf("the goal of draft %s changed to version %d after the one you planned from. Read it with `hl platform-experiments get` and answer it", pe.ID, pe.GoalVersion))
}

// refusedAnswer is the refusal of the planner's answer to the draft pe that the store refused
// with err, naming the goal the draft has where it changed in the meantime.
func (s *Service) refusedAnswer(ctx context.Context, pe *domain.PlatformExperiment, err error) error {
	switch {
	case errors.Is(err, db.ErrNotDraft):
		return notADraft(pe)
	case errors.Is(err, db.ErrPlanningOver):
		return planningOver(pe)
	case errors.Is(err, db.ErrGoalChanged):
		now, err := s.stored(ctx, pe.ID)
		if err != nil {
			return err
		}
		return goalChanged(now)
	}
	return err
}

// AcceptDue accepts, as proposed, every draft whose accepts_at has passed, for the person who
// created it. One the check refuses now, its accelerator type gone from every cluster for one,
// is held: it waits for a person, who is told why on accepting it. One that cannot be accepted
// for a fault of the platform's is tried again on the next pass.
func (s *Service) AcceptDue(ctx context.Context) error {
	ids, err := s.store.DraftsToAccept(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		log := s.logger.With(zap.String("platform_experiment_id", id))
		err := s.acceptAsProposed(ctx, id)
		var refused *refusal.Refusal
		switch {
		case err == nil:
			log.Info("draft accepted as proposed")
		case errors.As(err, &refused) && refused.GetStatus() < http.StatusInternalServerError:
			log.Warn("draft not accepted as proposed: it waits for a person", zap.String("reason", refused.Reason), zap.String("message", refused.Message))
			if err := s.store.HoldAccept(ctx, id); err != nil && !errors.Is(err, db.ErrNotDraft) && !errors.Is(err, db.ErrNotProposed) {
				log.Error("hold the draft", zap.Error(err))
			}
		default:
			log.Error("accept the draft as proposed", zap.Error(err))
		}
	}
	return nil
}

// acceptAsProposed accepts the draft id at the cost, length and researchers its proposal states.
func (s *Service) acceptAsProposed(ctx context.Context, id string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
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
	_, err = s.accept(ctx, id, AcceptRequest{
		BudgetAcceleratorHours: proposal.BudgetAcceleratorHours, DurationHours: proposal.DurationHours,
		BaselineMinutes: proposal.BaselineMinutes, Agents: proposal.Agents,
	}, by, nil)
	return err
}

// StartAutoAccept runs AcceptDue every interval until ctx is cancelled, each pass bounded by
// passTimeout.
func (s *Service) StartAutoAccept(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			if err := s.AcceptDue(passCtx); err != nil {
				s.logger.Error("accept_due: scan", zap.Error(err))
			}
			cancel()
		}
	}
}
