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
	"github.com/scaleresearch/hypothesisloop/contract/jobspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// Proposal is what a draft's planner proposes: the experiment as its file would state it, and
// beside it what a run costs. It opens the draft as it is proposed, with the planner's answers
// to its questions as the assumptions the run starts on; a person changes the plan afterwards.
// Nothing has run when it is written: the baseline is measured once it is applied, before any
// executor starts.
type Proposal struct {
	GoalVersion int `json:"goal_version" doc:"The goal_version of the draft the proposal was planned from. 409 goal_changed once the draft has another goal: read it and propose for it."`

	// Experiment is shaped exactly like .hypothesisloop/experiment.yaml: name, description (the
	// text every agent works from), metrics, report_interval_seconds, stages and the policies.
	// It is checked by validate, as the body of a create is.
	Experiment json.RawMessage `json:"experiment" doc:"The experiment as .hypothesisloop/experiment.yaml would state it: name, description, metrics, and optionally report_interval_seconds, stages and default_quota_tier, which left out are the platform's settings, and max_concurrent_accelerators."`

	BudgetAcceleratorHours float64         `json:"budget_accelerator_hours" doc:"Proposed budget of the run: a cap, estimated before anything ran."`
	DurationHours          float64         `json:"duration_hours" doc:"Proposed length of the run, counted from the moment it starts, once its baseline is declared: a time the goal states is this."`
	BaselineMinutes        int             `json:"baseline_minutes" doc:"The minutes the baseline agent needs, from the proposal, to measure the baseline and leave its job: few where the repository holds a harness that runs unchanged, more where the harness and its environment are written from nothing. At least planning.min_baseline_minutes and at most planning.baseline_limit_minutes."`
	Agents                 []ProposedAgent `json:"agents" doc:"The team proposed, one entry for each agent: one of agent_type researcher is a researcher, which runs no job and steers the work through the hypothesis graph, and any other an executor, which runs the jobs. At least one executor."`

	AcceleratorType domain.AcceleratorType `json:"accelerator_type,omitempty" doc:"The type the run's jobs are sized for, one of the accelerator_type strings GET /resource-catalog/capacity lists (hl capacity), copied verbatim. Left out for work that needs no accelerator: the run is then on CPU cores, hypothesisloop.dev/cpu=core, which is what is stored, and its budget is CPU core-hours. 422 accelerator_unreachable names every type that is listed now."`
	AcceleratorKind domain.TypeKind        `json:"accelerator_kind" readOnly:"true" doc:"What accelerator_type counts, which the platform sets: accelerator for devices, cpu for CPU cores, whose budget is CPU core-hours."`

	// CPU, Memory and Storage are what one node of the run's jobs asks for, as job.cpu, job.memory
	// and job.storage state it, so that whoever writes a job copies them from here and from no prose.
	CPU     string `json:"cpu" doc:"What the run's jobs state as job.cpu: cores for one node, a quantity such as 8 or 500m. Checked as a submit checks it: 422 invalid_resources for one left out, stated as max, no quantity or negative, as for memory and storage."`
	Memory  string `json:"memory" doc:"What the run's jobs state as job.memory: a quantity such as 32Gi. Checked as a submit checks it."`
	Storage string `json:"storage" doc:"What the run's jobs state as job.storage: a quantity such as 100Gi. Checked as a submit checks it."`

	MetricReason string             `json:"metric_reason" doc:"Why the ranked metric is the goal itself, in plain words."`
	Measurement  string             `json:"measurement" doc:"How it is measured, in plain words: the harness, the data it never trains on, the repeats."`
	SizingReason string             `json:"sizing_reason" doc:"Why this budget, this long and this team are the least that can reach the goal, in plain words, as an estimate: the trial length assumed, what an hour on the type costs, and the trials counted."`
	OutOfScope   []string           `json:"out_of_scope,omitempty"`
	Risks        []string           `json:"risks,omitempty"`
	Questions    []ProposalQuestion `json:"questions,omitempty" doc:"Each decision the goal left open that changes what counts, with the answer the proposal is planned on: among them any bound of a gate that depends on a number nobody has measured yet. Each answer is an assumption the run starts on: it is added to the description when the proposal is applied, where a person changes it."`
}

// ProposalQuestion is one decision a goal left open, and the answer its planner planned on.
type ProposalQuestion struct {
	Question string `json:"question" doc:"The decision, in a sentence."`
	Answer   string `json:"answer" doc:"The answer the proposal is planned on, which the run assumes."`
}

// ProposedAgent is one agent of a proposal's team, as POST /platform-experiments/{id}/agents takes
// it: its agent type gives its role.
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

// MaxProposedAgents is the most agents a proposal's team may name, and a draft's max_agents
// until one applies: of the maxAgentSandboxes machines the platform runs for an open draft, one
// is its coordinator's and one its baseline agent's.
func MaxProposedAgents(maxAgentSandboxes int) int {
	return maxAgentSandboxes - 2
}

// proposedPool is the pool the agents of a proposal make: an entry for each agent type and model
// they run on, as each is resolved when the platform creates it (domain.ResolveAgentLaunch).
func proposedPool(catalog domain.AgentTypeCatalog, agents []ProposedAgent) (domain.AgentPool, error) {
	keys := make([]domain.PoolKey, 0, len(agents))
	for i, agent := range agents {
		choice, err := domain.ResolveAgentLaunch(catalog, domain.PoolRole(agent.AgentType), agent.AgentType, agent.Model, agent.Hyperparameters)
		if err != nil {
			return nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		keys = append(keys, domain.PoolKey{AgentType: choice.AgentType, Model: choice.Model})
	}
	return domain.PoolOf(keys), nil
}

// proposedExecutors counts the executors of agents: every one but a researcher.
func proposedExecutors(agents []ProposedAgent) int {
	n := 0
	for _, agent := range agents {
		if domain.PoolRole(agent.AgentType) == domain.AgentRoleExecutor {
			n++
		}
	}
	return n
}

// poolAgents is an agent of each place of pool, as sandboxedAgents creates them.
func poolAgents(pool domain.AgentPool) []ProposedAgent {
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
// its cost, its length and one place for each agent of the team. It leaves p as it is stored: every text
// of it without the whitespace around it, the CPU type where it states no accelerator_type, and
// the kind of its type.
func (s *Service) checkProposal(ctx context.Context, p *Proposal, draft *domain.PlatformExperiment) (CreatePlatformExperimentRequest, error) {
	var problems refusal.Problems
	refuse := func(reason, format string, args ...any) {
		problems = append(problems, refusal.Refuse(http.StatusUnprocessableEntity, reason, fmt.Sprintf(format, args...)))
	}
	catalog, err := s.store.AgentTypeCatalog(ctx, draft.ID)
	if err != nil {
		return CreatePlatformExperimentRequest{}, err
	}
	file, err := experimentspec.Parse(p.Experiment)
	if err == nil && file.Placement != (experimentspec.Placement{}) {
		err = errors.New("project_id, repository and branch are the draft's and no part of a proposal")
	}
	experiment := CreatePlatformExperimentRequest{Spec: file.Spec}
	decoded := err == nil
	if !decoded {
		refuse("invalid_experiment", "experiment: %s", err)
	}
	executors := proposedExecutors(p.Agents)
	if most := MaxProposedAgents(draft.MaxAgentSandboxes); executors == 0 || len(p.Agents) > most {
		refuse("invalid_agents", "agents: propose at least one executor and at most %d agents in all, researchers included: the machines the platform runs for the team beside its coordinator and baseline agent. Got %d executors of %d", most, executors, len(p.Agents))
	}
	least, limit := max(1, int(s.cfg.MinBaseline.Minutes())), int(s.cfg.BaselineLimit.Minutes())
	if p.BaselineMinutes < least || p.BaselineMinutes > limit {
		refuse("invalid_baseline_minutes", "baseline_minutes is %d: state the minutes measuring the baseline needs, from %d (planning.min_baseline_minutes) to %d (planning.baseline_limit_minutes). "+
			"The platform gives a baseline at least %d minutes because one written from nothing writes its harness, waits for the platform to build its environment and measures once in that time, and a shorter window closes the experiment before its run starts",
			p.BaselineMinutes, least, limit, least)
	}
	if decoded {
		// A draft starts when its proposal applies, whatever its experiment says of a start.
		experiment.StartsAt = time.Time{}
		experiment.BudgetAcceleratorHours, experiment.DurationHours, experiment.MaxAgents = p.BudgetAcceleratorHours, p.DurationHours, executors
		pool, unresolved := proposedPool(catalog, p.Agents)
		if err := problems.Add(unresolved); err != nil {
			return experiment, err
		}
		experiment.Agents = pool
		var invalid error
		experiment, invalid = s.validate(experiment)
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
	p.AcceleratorKind = p.AcceleratorType.Kind()
	if _, err := s.requireReachable(ctx, draft.ProjectID, &p.AcceleratorType); err != nil {
		if err := problems.Add(err); err != nil {
			return experiment, err
		}
	}
	p.CPU, p.Memory, p.Storage = strings.TrimSpace(p.CPU), strings.TrimSpace(p.Memory), strings.TrimSpace(p.Storage)
	if err := jobspec.CheckNode("", p.CPU, p.Memory, p.Storage); err != nil {
		refuse("invalid_resources", "%s", err)
	}
	for i, agent := range p.Agents {
		if _, err := domain.ResolveAgentLaunch(catalog, domain.PoolRole(agent.AgentType), agent.AgentType, agent.Model, agent.Hyperparameters); err != nil {
			refuse("invalid_agents", "agents[%d]: %s", i, err)
		} else if err := problems.Add(s.served.CheckChoice(ctx, fmt.Sprintf("agents[%d].model", i), agent.Model)); err != nil {
			return experiment, err
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
// the proposal a person reads states what applying it stores.
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

// Propose applies p as the proposal of the draft id, written by its planner caller, which opens
// it, or with check only checks it and answers the draft as it stands.
func (s *Service) Propose(ctx context.Context, id string, caller auth.Principal, p *Proposal, check, working bool) (*domain.PlatformExperiment, error) {
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
	if working {
		return s.draftPlan(ctx, pe, *p)
	}
	return s.apply(ctx, pe, *p)
}

// draftPlan stores proposal, checked, as the plan the planner of the draft pe works on, which its
// baseline agent starts from while the draft stays a draft.
func (s *Service) draftPlan(ctx context.Context, pe *domain.PlatformExperiment, proposal Proposal) (*domain.PlatformExperiment, error) {
	if pe.ProposedAt != nil {
		return nil, refusal.Refuse(http.StatusConflict, "already_answered", "draft "+pe.ID+" was answered for this goal: a working plan is for a goal not yet answered")
	}
	stored, err := json.Marshal(proposal)
	if err != nil {
		return nil, s.refusal("encode proposal", err)
	}
	if err := s.refusedAnswer(ctx, pe, s.store.SetWorkingPlan(ctx, pe.ID, proposal.GoalVersion, stored)); err != nil {
		return nil, s.refusal("store working plan", err)
	}
	s.logger.Info("draft working plan", zap.String("id", pe.ID))
	return s.stored(ctx, pe.ID)
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

// answered is the proposal's experiment with each question and the planner's answer added to the
// end of its description, a line each, so that every agent reads what the run assumes, and the
// answers as the description states them.
func answered(experiment json.RawMessage, questions []ProposalQuestion) (json.RawMessage, []string, error) {
	if len(questions) == 0 {
		return experiment, nil, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(experiment, &fields); err != nil {
		return nil, nil, err
	}
	var description string
	if err := json.Unmarshal(fields["description"], &description); err != nil {
		return nil, nil, err
	}
	lines := []string{strings.TrimRight(description, "\n"), "", "Assumptions the plan runs on:"}
	given := make([]string, len(questions))
	for i, q := range questions {
		given[i] = strings.Join(strings.Fields(q.Answer), " ")
		lines = append(lines, "- "+strings.Join(strings.Fields(q.Question), " ")+" Answer: "+given[i])
	}
	var err error
	if fields["description"], err = json.Marshal(strings.Join(lines, "\n")); err != nil {
		return nil, nil, err
	}
	experiment, err = json.Marshal(fields)
	return experiment, given, err
}

func invalidTransition(format string, args ...any) error {
	return refusal.Refuse(http.StatusConflict, "invalid_transition", fmt.Sprintf(format, args...))
}

// apply makes the draft pe the open platform experiment proposal describes, with a sandboxed
// coordinator, its team and the agent that measures its baseline, owned by the person who
// created the draft, in the write that stores the proposal. The planner's answers to its
// questions are added to the description.
func (s *Service) apply(ctx context.Context, pe *domain.PlatformExperiment, proposal Proposal) (*domain.PlatformExperiment, error) {
	stored, err := json.Marshal(proposal)
	if err != nil {
		return nil, s.refusal("encode proposal", err)
	}
	var answers []string
	if proposal.Experiment, answers, err = answered(proposal.Experiment, proposal.Questions); err != nil {
		return nil, s.refusal("add the proposal's answers", err)
	}
	now := time.Now().UTC()
	experiment, err := s.checkProposal(ctx, &proposal, pe)
	if err != nil {
		return nil, s.refusal("check proposal", err)
	}
	if s.launch.DefaultModel == "" || s.launch.CoordinatorModel == "" {
		return nil, s.refusal("apply", errors.New("the handler has no agent launch settings"))
	}
	if err := requireAgentSandbox(s.launch.AgentSandbox); err != nil {
		return nil, s.refusal("apply", err)
	}
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}

	pe.Name, pe.Description, pe.Metrics, pe.ReportIntervalSeconds = experiment.Name, experiment.Description, experiment.Metrics, experiment.ReportIntervalSeconds
	pe.Stages, pe.DefaultQuotaTier = experiment.Stages, experiment.DefaultQuotaTier
	pe.MaxConcurrentAccelerators = experiment.MaxConcurrentAccelerators
	pe.BudgetAcceleratorHours, pe.MaxAgents, pe.Agents, pe.DurationHours = experiment.BudgetAcceleratorHours, experiment.MaxAgents, experiment.Agents, experiment.DurationHours
	pe.StartsAt, pe.BaselineDueAt = now, now.Add(time.Duration(proposal.BaselineMinutes)*time.Minute)
	pe.CoordinatorInAgentSandbox, pe.Coordinator = true, &domain.AgentSandboxCoordinator{}
	pe.AcceptedAt, pe.AcceptedBy, pe.AcceptedAnswers = &now, nil, answers

	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return nil, s.refusal("apply", err)
	}
	agents, signups, err := sandboxedAgents(catalog, proposal.Agents, by, now)
	if err != nil {
		return nil, s.refusal("apply", err)
	}
	signed, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return nil, s.refusal("apply", err)
	}
	if slices.ContainsFunc(signed, func(s domain.AgentSignup) bool { return s.Role == domain.SignupRoleBaseline }) {
		agents, signups = agents[:len(agents)-1], signups[:len(signups)-1]
	}
	err = s.store.ApplyProposal(ctx, pe, proposal.GoalVersion, stored, agents, signups)
	if errors.Is(err, db.ErrMaxAgentSandboxesReached) {
		return nil, refusedProposal("invalid_agents", "%s", maxAgentSandboxesReached(pe.MaxAgentSandboxes))
	}
	if err := s.refusedAnswer(ctx, pe, err); err != nil {
		return nil, s.refusal("apply", err)
	}
	s.logger.Info("draft applied", zap.String("id", pe.ID), zap.Int("agents", len(proposal.Agents)), zap.Float64("budget_accelerator_hours", pe.BudgetAcceleratorHours))
	return s.stored(ctx, pe.ID)
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
	return refusal.Refuse(http.StatusConflict, "not_a_draft", "platform experiment "+pe.ID+" is "+string(pe.Status)+": only a draft is proposed")
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

// ApplyStanding applies, once, every draft whose proposal was stored before proposals applied
// themselves and still waits, as the platform would have accepted it. One the check refuses now
// is told in the log and left to its planning limit.
func (s *Service) ApplyStanding(ctx context.Context, passTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	drafts, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpDraft)
	if err != nil {
		s.logger.Error("apply standing proposals: list drafts", zap.Error(err))
		return
	}
	for _, listed := range drafts {
		pe, err := s.store.GetPlatformExperiment(ctx, listed.ID)
		if err != nil || pe == nil || pe.ProposedAt == nil || len(pe.Proposal) == 0 || pe.DeclinedReason != "" {
			continue
		}
		var proposal Proposal
		if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
			s.logger.Error("apply standing proposals: decode", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		if _, err := s.apply(ctx, pe, proposal); err != nil {
			s.logger.Warn("standing proposal not applied", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		s.logger.Info("standing proposal applied", zap.String("platform_experiment_id", pe.ID))
	}
}
