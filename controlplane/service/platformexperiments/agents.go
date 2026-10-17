package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// AddAgentRequest is the body of POST /platform-experiments/{id}/agents: the one way an agent
// joins a platform experiment.
type AddAgentRequest struct {
	AgentID         string         `json:"agent_id,omitempty" doc:"Join as this registered agent instead of creating a new one. Joining again once joined succeeds and changes nothing."`
	Name            string         `json:"name,omitempty" doc:"Display name of a created agent, unique in the platform experiment: 409 agent_name_taken for one another agent of it carries. Omitted, it is named by its role and a number no other agent of the experiment has: \"Researcher 2\", \"Baseline 1\"."`
	Role            string         `json:"role,omitempty" doc:"\"competitor\" (default), \"baseline\" or \"reviewer\"."`
	QuotaTier       string         `json:"quota_tier,omitempty" doc:"\"guaranteed\" or \"burst_only\"; omitted, the platform experiment's default_quota_tier applies."`
	InAgentSandbox  bool           `json:"in_agentsandbox,omitempty" doc:"true: the platform runs the agent. false (default): its token is returned and you run it."`
	AgentType       string         `json:"agent_type,omitempty" doc:"Which specialization the agent runs, which it reads from its signup: generalist (default), hyperparameter-search, architecture-search or data-search."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object the agent reads from its signup, at most 4096 bytes as JSON."`
	Model           string         `json:"model,omitempty" doc:"The model the agent runs on, which it reads from its signup. Omitted, the platform's default applies and is what the answer shows."`
}

// AgentLaunch is what joining needs beyond the store.
type AgentLaunch struct {
	// AgentSandbox says the deployment has a sandbox provider: without one no agent runs in an agent sandbox.
	AgentSandbox bool
	// DefaultModel is what an agent that names no model runs on.
	DefaultModel string
	// CoordinatorModel is what a sandboxed coordinator that names no model runs on.
	CoordinatorModel string
	// SigningSecret derives an agent's own token.
	SigningSecret string
	// MaxAgentSandboxes is how many machines the platform runs for one platform experiment, written
	// onto each when it is created.
	MaxAgentSandboxes int
}

// JoinedAgent is one agent's place in a platform experiment as the API answers with it. Token is
// set only in the answer to joining, and only for an agent the platform does not run.
type JoinedAgent struct {
	domain.AgentSignup
	// StatusReason is why the agent is not at work yet: its platform experiment is open and has
	// not started, or its machine is asked for and not running.
	StatusReason *domain.StatusReason  `json:"status_reason,omitempty" doc:"Why the agent is not at work yet. On an agent that waits for its open platform experiment to start, every one but those of role baseline: reason is the platform experiment's own (baseline_measuring, baseline_required and the others of its status_reason), and message says the agent takes no turn and, when the platform runs it, has no machine asked for until the start. Its instance reads waiting until then. On a sandboxed agent whose machine is asked for and not running yet, its instance reading requested or starting: instance_starting, and message says how long ago the machine was asked for. That is a start in progress and no failure. On an agent that waited for the start of a platform experiment that closed before its run started, and had no machine asked for it: never_launched, with no waits_for, and message gives the close_reason and why it closed. Its instance reads stopped with no end_reason."`
	Token        string                `json:"token,omitempty"`
	Actions      []domain.SignupAction `json:"actions" doc:"What the reader may do to the agent now, each a write the API accepts from them: message (POST /platform-experiments/{id}/messages naming it), remove (DELETE /platform-experiments/{id}/agents/{agent_id}, which still refuses an agent with a job queued or running) and show_token (POST /platform-experiments/{id}/agents with its agent_id, which answers the token of an agent the platform does not run). Empty for a reader who writes nothing of the platform experiment."`
	// The fields below say what an agent of an open or running platform experiment has going,
	// absent once its run ended (withWork).
	Subagents                 *int                     `json:"subagents,omitempty" doc:"How many subagents its harness last reported running, of the machine it holds now. Absent with no such report, as for an agent the platform does not run."`
	Turn                      *domain.AgentTurn        `json:"turn,omitempty" doc:"Whether its harness last reported its model in a turn or waiting for platform updates, and since when, of the machine it holds now."`
	RunningPinnedInstructions *bool                    `json:"running_pinned_instructions,omitempty" doc:"Whether the instructions its harness last reported running are the ones the version of its agent type it is pinned to serves it now: false while an edit waits for its next turn boundary or a fresh session. Absent with no such report."`
	Messages                  *domain.MessageAllowance `json:"messages,omitempty" doc:"How many more messages it may be sent this hour, and when that next grows."`
	ModelAllowance            *domain.ModelAllowance   `json:"model_allowance,omitempty" doc:"Its model account's rate limit as its harness last read it, of the machine it holds now."`
	MinutesWithoutJobInFlight *int                     `json:"minutes_without_job_in_flight,omitempty" doc:"Whole minutes since it last had a job of its own created or running, 0 while one is in flight. In a running platform experiment that started."`
}

// roster is signups as the API answers them to reader, whose messages the allowance counts. In an open platform experiment a researcher's
// machine is not asked for, and reading requested it was taken for a launch that failed: each
// agent that waits for the start says so, and for what, and its instance reads waiting. Once it
// closed before the start, each says it never launched and why (neverLaunched). A machine that is
// asked for and not running yet says that too (instanceStarting), in any status.
func (s *Service) roster(ctx context.Context, platformExpID string, signups []domain.AgentSignup, reader domain.Actor) ([]JoinedAgent, error) {
	out := make([]JoinedAgent, len(signups))
	now := time.Now()
	for i, signup := range signups {
		out[i] = JoinedAgent{AgentSignup: signup, StatusReason: instanceStarting(signup, now), Actions: []domain.SignupAction{}}
		if why := out[i].StatusReason; why != nil {
			var err error
			if why.WaitsFor, err = domain.WaitsFor(why.Reason, false); err != nil {
				return nil, fmt.Errorf("platform_experiments.roster: %w", err)
			}
		}
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.roster: %w", err)
	}
	if pe != nil && (pe.Status == domain.PlatformExpOpen || pe.Status == domain.PlatformExpRunning) {
		if err := s.withWork(ctx, pe, out, now, reader); err != nil {
			return nil, fmt.Errorf("platform_experiments.roster: %w", err)
		}
	}
	if pe != nil && pe.Status == domain.PlatformExpClosed && pe.StartedAt.IsZero() {
		why, err := s.closedReason(ctx, pe)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.roster: %w", err)
		}
		for i, signup := range signups {
			if signup.SharesBudget() && (signup.Instance == nil || signup.Instance.AskedAt == nil) {
				out[i].StatusReason = neverLaunched(why)
			}
		}
		return out, nil
	}
	if pe == nil || pe.Status != domain.PlatformExpOpen {
		return out, nil
	}
	all, err := s.store.ListAgentSignups(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.roster: %w", err)
	}
	why, err := s.waiting(ctx, pe, all)
	if err != nil {
		return nil, err
	}
	who, err := domain.WaitsFor(why.Reason, pe.CoordinatorInAgentSandbox)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.roster: %w", err)
	}
	for i, signup := range signups {
		if !signup.SharesBudget() {
			continue
		}
		waits, summary := "it takes its first turn when the platform starts the run", "It starts with the run."
		if signup.InAgentSandbox {
			waits, summary = "its machine is asked for when the platform starts the run, and none is before", "Its machine starts with the run."
		}
		if signup.Instance != nil {
			instance := *signup.Instance
			instance.Waiting = true
			out[i].Instance = &instance
		}
		out[i].StatusReason = &domain.StatusReason{Reason: why.Reason, Message: waits + ": " + why.Message, Summary: summary, WaitsFor: who}
	}
	return out, nil
}

// reasonInstanceStarting is the status_reason of an agent whose machine is on its way.
const reasonInstanceStarting = "instance_starting"

// neverLaunched is the status_reason of an agent of a platform experiment closed for why that
// waited for a run that never started: no machine was asked for it and it took no turn.
func neverLaunched(why *domain.StatusReason) *domain.StatusReason {
	return &domain.StatusReason{Reason: "never_launched", Message: "it never took a turn and no machine was asked for it: its platform experiment closed before the run started, with close_reason " + why.Reason + ": " + why.Message,
		Summary: "Never started: the experiment closed before the run."}
}

// instanceStarting is the status_reason of a sandboxed agent whose run goes on and whose machine is
// not running yet, nil for every other. Its instance reads requested or starting then, which is
// a start in progress and was read as a launch that failed.
func instanceStarting(signup domain.AgentSignup, now time.Time) *domain.StatusReason {
	if !signup.InAgentSandbox || signup.Instance == nil || signup.Ended != "" {
		return nil
	}
	instance := *signup.Instance
	state := instance.Shown()
	if state != domain.InstanceRequested && state != domain.InstanceStarting {
		return nil
	}
	where, summary := "its machine is asked for at the launcher's next pass", "Its machine is being requested."
	if instance.Name != "" && instance.AskedAt != nil {
		ago := readableDuration(now.Sub(*instance.AskedAt))
		where, summary = "its machine was asked for "+ago+" ago and its launcher has not listed it yet", "Its machine was requested "+ago+" ago and is not listed yet."
		if state == domain.InstanceStarting {
			where, summary = "its machine was asked for "+ago+" ago and is starting", "Its machine is starting (requested "+ago+" ago)."
		}
	}
	return &domain.StatusReason{Reason: reasonInstanceStarting, Summary: summary, Message: where +
		": a start in progress and no failure. A machine that does not come up is replaced, which restarts counts, and one given up on reads stopped with end_reason and exit_log"}
}

// refuseSignup is a join or a removal refused for a reason the caller can act on. Reason leads
// the message, as every signup refusal always has.
func refuseSignup(status int, reason, format string, args ...any) error {
	return refusal.Refuse(status, reason, reason+": "+fmt.Sprintf(format, args...))
}

// AddAgent joins one agent to a platform experiment: the registered agent req names, or a new
// one created in the experiment's org. by is whose credential asked. The answer holds that one
// agent.
func (s *Service) AddAgent(ctx context.Context, platformExpID string, req AddAgentRequest, by domain.Actor, launch AgentLaunch) ([]domain.AgentSignup, error) {
	role, err := domain.ParseSignupRole(req.Role)
	if err != nil {
		return nil, refuseSignup(400, "unknown_role", "%s", err)
	}
	if !experimentspec.ValidQuotaTierOverride(req.QuotaTier) {
		return nil, refuseSignup(400, "invalid_request", "quota_tier must be \"guaranteed\", \"burst_only\", or omitted, got %s", req.QuotaTier)
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if pe == nil {
		return nil, refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	catalog, err := s.store.AgentTypeCatalog(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	choice, err := domain.ResolveAgentLaunch(catalog, domain.RoleOfSignup(role), req.AgentType, req.Model, req.Hyperparameters)
	if err != nil {
		return nil, refuseSignup(400, "invalid_request", "%s", err)
	}
	signup := domain.AgentSignup{
		AgentID: req.AgentID, Role: role, QuotaTier: domain.QuotaTier(req.QuotaTier),
		InAgentSandbox: req.InAgentSandbox, AgentType: choice.AgentType, Hyperparameters: choice.Hyperparameters, Model: choice.Model, Actor: &by,
	}
	if req.InAgentSandbox {
		if err := requireAgentSandbox(launch.AgentSandbox); err != nil {
			return nil, err
		}
	}
	member := false
	if req.AgentID != "" {
		if member, err = s.store.IsSignedUp(ctx, platformExpID, req.AgentID); err != nil {
			return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
		}
	}
	if !member && !takesSignup(pe.Status, role) {
		return nil, enrolRefused(db.ErrSignupClosed, pe, req.Name)
	}
	var late *db.LateJoin
	if pe.Status == domain.PlatformExpRunning && !member {
		if late, err = s.lateJoin(ctx, pe, by); err != nil {
			return nil, err
		}
	}
	join := s.spawn
	if req.AgentID != "" {
		join = s.joinRegistered
	}
	joined, err := join(ctx, pe, req, signup, late)
	if err != nil || !signup.SharesBudget() {
		return joined, err
	}
	// A researcher may be all a declared baseline still waited for.
	if err := s.StartIfReady(ctx, platformExpID); err != nil {
		return nil, err
	}
	return joined, nil
}

// lateJoin is what a researcher joining the running pe takes its quota from. Once the run started
// only a researcher joins, in the place of one that ended or one a raised max_agents gave, and its
// coordinator or a person adds it.
func (s *Service) lateJoin(ctx context.Context, pe *domain.PlatformExperiment, by domain.Actor) (*db.LateJoin, error) {
	if by.Kind == domain.ActorAgent {
		return nil, refuseSignup(403, "not_coordinator", "an agent adds no agent to a running platform experiment: its coordinator or its owner does")
	}
	return s.lateShare(ctx, pe)
}

// lateShare is what a researcher joining the running pe is given its quota from: what each
// researcher used, and the least share worth giving, one trial of the baseline.
func (s *Service) lateShare(ctx context.Context, pe *domain.PlatformExperiment) (*db.LateJoin, error) {
	quotas, err := s.researcherQuotas(ctx, pe)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.lateJoin: %w", err)
	}
	used := make(map[string]float64, len(quotas))
	for _, quota := range quotas {
		used[quota.AgentID] = quota.Used
	}
	held, err := s.runningTrialHeld(ctx, pe)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.lateJoin: %w", err)
	}
	return &db.LateJoin{Used: used, BurstFraction: s.cfg.BurstFraction, MinShare: held}, nil
}

// researcherQuotas is the quota and use of each researcher of pe that holds one and was not cut.
// Ended is left unset.
func (s *Service) researcherQuotas(ctx context.Context, pe *domain.PlatformExperiment) ([]domain.ResearcherQuota, error) {
	researchers, err := s.store.ListSignupsByRole(ctx, pe.ID, domain.SignupRoleCompetitor)
	if err != nil {
		return nil, err
	}
	var quotas []domain.ResearcherQuota
	for _, agentID := range researchers {
		cut, err := s.store.IsAgentCut(ctx, pe.ID, agentID)
		if err != nil {
			return nil, err
		}
		if cut {
			continue
		}
		quota, err := s.GetAgentQuota(ctx, agentID, pe.ID)
		if err != nil {
			return nil, err
		}
		if quota != nil {
			quotas = append(quotas, domain.ResearcherQuota{
				AgentID: agentID, Guaranteed: quota.GuaranteedAcceleratorHours, Burst: quota.BurstAcceleratorHours, Used: quota.Used(),
			})
		}
	}
	return quotas, nil
}

// enrolRefused answers what a signup to pe was refused for in the store, name being the agent's,
// and nil for anything else.
func enrolRefused(err error, pe *domain.PlatformExperiment, name string) error {
	switch {
	case errors.Is(err, db.ErrSignupClosed) && pe.Status == domain.PlatformExpRunning:
		return refuseSignup(409, "signup_closed", "platform experiment %s is running, and only a researcher (role competitor) joins it then", pe.ID)
	case errors.Is(err, db.ErrSignupClosed):
		return refuseSignup(409, "signup_closed", "experiment is %s", pe.Status)
	case errors.Is(err, db.ErrAgentNameTaken):
		return agentNameTaken(name)
	case errors.Is(err, db.ErrMaxAgentsReached):
		return maxAgentsReached(pe.MaxAgents)
	case errors.Is(err, db.ErrMaxAgentSandboxesReached):
		return refuseSignup(409, "max_agentsandboxes_reached", "%s", maxAgentSandboxesReached(pe.MaxAgentSandboxes))
	case errors.Is(err, db.ErrNoShareLeft):
		return refuseSignup(409, "no_share_left", "a researcher that joins running platform experiment %s is given an even part, with the researchers at work, of what all researchers left unspent, and that part would not hold one trial of the baseline: one joins when a researcher at work finishes", pe.ID)
	}
	return nil
}

// agentNameTaken refuses an agent whose name another agent of the platform experiment carries.
func agentNameTaken(name string) error {
	return refuseSignup(409, "agent_name_taken", "another agent of this platform experiment is named %q, and a name tells its agents apart: state another name, or none to have the platform name it by its role", name)
}

func (s *Service) joinRegistered(ctx context.Context, pe *domain.PlatformExperiment, req AddAgentRequest, signup domain.AgentSignup, late *db.LateJoin) ([]domain.AgentSignup, error) {
	platformExpID := pe.ID
	if req.Name != "" {
		return nil, refuseSignup(400, "invalid_request", "name describes an agent to create and cannot be set with agent_id")
	}
	agent, err := s.store.GetAgent(ctx, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if agent == nil {
		return nil, refuseSignup(404, "agent_not_found", "no agent %s is registered", req.AgentID)
	}
	joined, err := s.store.GetAgentSignup(ctx, platformExpID, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	switch {
	case joined != nil:
		// A member joining again has what it asked for.
		return []domain.AgentSignup{*joined}, nil
	case late != nil:
		err := s.store.JoinRunning(ctx, platformExpID, nil, signup, *late)
		if refused := enrolRefused(err, pe, agent.Name); refused != nil {
			return nil, refused
		}
		if errors.Is(err, db.ErrAgentOutsideOrg) {
			return nil, refuseSignup(404, "agent_not_found", "no agent %s is registered", req.AgentID)
		}
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
		}
	default:
		if err := s.Signup(ctx, platformExpID, signup); err != nil {
			return nil, err
		}
	}
	joined, err = s.store.GetAgentSignup(ctx, platformExpID, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if joined == nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: agent %s is not signed up after signing up", req.AgentID)
	}
	return []domain.AgentSignup{*joined}, nil
}

func (s *Service) spawn(ctx context.Context, pe *domain.PlatformExperiment, req AddAgentRequest, signup domain.AgentSignup, late *db.LateJoin) ([]domain.AgentSignup, error) {
	platformExpID := pe.ID
	id, err := domain.NewID(domain.AgentIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	signup.AgentID = id
	agent := &domain.Agent{ID: id, Name: strings.TrimSpace(req.Name), CreatedAt: time.Now().UTC()}
	if late != nil {
		err = s.store.JoinRunning(ctx, platformExpID, agent, signup, *late)
	} else {
		err = s.store.SpawnAgent(ctx, platformExpID, agent, signup)
	}
	if refused := enrolRefused(err, pe, agent.Name); refused != nil {
		return nil, refused
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	joined, err := s.store.GetAgentSignup(ctx, platformExpID, id)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if joined == nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: agent %s is not signed up after being created", id)
	}
	s.logger.Info("agent spawned", zap.String("platformExpID", platformExpID), zap.String("agentID", id),
		zap.Bool("in_agentsandbox", signup.InAgentSandbox))
	return []domain.AgentSignup{*joined}, nil
}

// RemoveAgent takes an agent out of a platform experiment that has not started. Once it has, the
// roster is what quota was divided over: an agent leaves by being cut or by the experiment closing.
// by is whose credential asked: a coordinator's token removes only an agent a coordinator's token
// of this platform experiment added, and never one a person or an API key did.
func (s *Service) RemoveAgent(ctx context.Context, platformExpID, agentID string, by domain.Actor) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	if signup == nil {
		return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	}
	if err := removesAgent(pe, *signup, by); err != nil {
		return err
	}
	err = s.store.RemoveAgent(ctx, platformExpID, agentID, s.dataStore.DeleteDataKey)
	switch {
	case errors.Is(err, domain.ErrNotSignedUp):
		return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	case errors.Is(err, db.ErrSignupClosed):
		return refuseSignup(409, "experiment_started", "experiment is %s: once started, an agent leaves by being cut or by the experiment closing", pe.Status)
	case errors.Is(err, db.ErrWorkUnfinished):
		return refuseSignup(409, "work_unfinished", "agent %s has a job queued or running, and its quota, which that job is charged to, goes with the agent: cancel the job (`hl job cancel`) or wait for it to end, then remove the agent", agentID)
	case err != nil:
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	s.logger.Info("agent removed", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	return nil
}

// nameAddedBy names whoever added each agent.
func (s *Service) nameAddedBy(ctx context.Context, signups []domain.AgentSignup) error {
	actors := make([]*domain.Actor, 0, len(signups))
	for i := range signups {
		if signups[i].Actor != nil {
			actors = append(actors, signups[i].Actor)
		}
	}
	if len(actors) == 0 {
		return nil
	}
	return s.store.NameActors(ctx, actors)
}

// Join adds the agent req asks for to the platform experiment, added by by, who writes it, and
// answers it as the roster lists it to by.
func (s *Service) Join(ctx context.Context, platformExpID string, req AddAgentRequest, by auth.Principal, launch AgentLaunch) ([]JoinedAgent, error) {
	signups, err := s.AddAgent(ctx, platformExpID, req, by.Actor(), launch)
	if err != nil {
		return nil, err
	}
	agents, err := s.namedRoster(ctx, platformExpID, signups, by.Actor())
	if err != nil {
		return nil, err
	}
	pe, err := s.stored(ctx, platformExpID)
	if err != nil {
		return nil, err
	}
	permitAgents(pe, agents, by, true)
	return agents, nil
}

// Roster is every agent that joined the platform experiment, in the order they did, as reader
// reads them, with what reader may do to each when it writes the platform experiment, or
// ErrPlatformExperimentNotFound.
func (s *Service) Roster(ctx context.Context, platformExpID string, reader auth.Principal, writes bool) ([]JoinedAgent, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	signups, err := s.store.ListAgentSignups(ctx, platformExpID)
	if err != nil {
		return nil, err
	}
	agents, err := s.namedRoster(ctx, platformExpID, signups, reader.Actor())
	if err != nil {
		return nil, err
	}
	permitAgents(pe, agents, reader, writes)
	return agents, nil
}

func permitAgents(pe *domain.PlatformExperiment, agents []JoinedAgent, caller auth.Principal, writes bool) {
	for i := range agents {
		agents[i].Actions = SignupActions(pe, agents[i].AgentSignup, caller, writes)
	}
}

// namedRoster is roster with whoever added each agent named.
func (s *Service) namedRoster(ctx context.Context, platformExpID string, signups []domain.AgentSignup, reader domain.Actor) ([]JoinedAgent, error) {
	if err := s.nameAddedBy(ctx, signups); err != nil {
		return nil, err
	}
	return s.roster(ctx, platformExpID, signups, reader)
}
