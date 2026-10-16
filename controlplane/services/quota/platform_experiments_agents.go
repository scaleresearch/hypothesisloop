package quota

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
)

// AddAgentRequest is the body of POST /platform-experiments/{id}/agents: the one way an agent
// joins a platform experiment.
type AddAgentRequest struct {
	AgentID         string         `json:"agent_id,omitempty" doc:"Join as this registered agent instead of creating a new one. Joining again once joined succeeds and changes nothing."`
	Name            string         `json:"name,omitempty" doc:"Display name of a created agent, unique in the platform experiment: 409 agent_name_taken for one another agent of it carries. Omitted, it is named by its role and a number no other agent of the experiment has: \"Researcher 2\", \"Baseline 1\"."`
	Role            string         `json:"role,omitempty" doc:"\"competitor\" (default), \"baseline\" or \"reviewer\"."`
	QuotaTier       string         `json:"quota_tier,omitempty" doc:"\"guaranteed\" or \"burst_only\"; omitted, the platform experiment's default_quota_tier applies."`
	Hosted          bool           `json:"hosted,omitempty" doc:"true: the platform runs the agent. false (default): its token is returned and you run it."`
	Flavor          string         `json:"flavor,omitempty" doc:"Which specialization the agent runs, which it reads from its signup: generalist (default), hyperparameter-search, architecture-search or data-search."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object the agent reads from its signup, at most 4096 bytes as JSON."`
	Model           string         `json:"model,omitempty" doc:"The model the agent runs on, which it reads from its signup. Omitted, the platform's default applies and is what the answer shows."`
}

// AgentLaunch is what joining needs beyond the store.
type AgentLaunch struct {
	// Launcher is the launcher a hosted agent is given to.
	Launcher string
	// DefaultModel is what an agent that names no model runs on.
	DefaultModel string
	// CoordinatorModel is what a hosted coordinator that names no model runs on.
	CoordinatorModel string
	// SigningSecret derives an agent's own token.
	SigningSecret string
	// MaxHosted is how many machines the platform runs for one platform experiment, written
	// onto each when it is created.
	MaxHosted int
}

// JoinedAgent is one agent's place in a platform experiment as the API answers with it. Token is
// set only in the answer to joining, and only for an agent the platform does not run.
type JoinedAgent struct {
	domain.AgentSignup
	// StatusReason is why the agent is not at work yet: its platform experiment is open and has
	// not started, or its machine is asked for and not running.
	StatusReason *domain.StatusReason `json:"status_reason,omitempty" doc:"Why the agent is not at work yet. On an agent that waits for its open platform experiment to start, every one but those of role baseline: reason is the platform experiment's own (baseline_measuring, baseline_required and the others of its status_reason), and message says the agent takes no turn and, when the platform runs it, has no machine asked for until the start. Its instance reads waiting until then. On a hosted agent whose machine is asked for and not running yet, its instance reading requested or starting: instance_starting, and message says how long ago the machine was asked for. That is a start in progress and no failure. Such an agent whose launcher has not asked for its machines for a minute: launcher_unreachable, and message names the launcher and how long ago it last asked. On an agent that waited for the start of a platform experiment that closed before its run started, and had no machine asked for it: never_launched, with no waits_for, and message gives the close_reason and why it closed. Its instance reads stopped with no end_reason."`
	Token        string               `json:"token,omitempty"`
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
func (s *PlatformExperimentsService) roster(ctx context.Context, platformExpID string, signups []domain.AgentSignup, reader domain.Actor) ([]JoinedAgent, error) {
	out := make([]JoinedAgent, len(signups))
	now := time.Now()
	var starting []string
	for _, signup := range signups {
		if instanceStarting(signup, now) != nil {
			starting = append(starting, signup.Launcher)
		}
	}
	silent, err := s.silentLaunchers(ctx, starting, now)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.roster: %w", err)
	}
	for i, signup := range signups {
		out[i] = JoinedAgent{AgentSignup: signup, StatusReason: instanceStarting(signup, now)}
		if sentence, ok := silent[signup.Launcher]; ok && out[i].StatusReason != nil {
			out[i].StatusReason = &domain.StatusReason{Reason: reasonLauncherUnreachable, Message: launcherUnreachableMessage(sentence)}
		}
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
	who, err := domain.WaitsFor(why.Reason, pe.CoordinatorHosted)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.roster: %w", err)
	}
	for i, signup := range signups {
		if !signup.SharesBudget() {
			continue
		}
		waits := "it takes its first turn when the platform starts the run"
		if signup.Hosted {
			waits = "its machine is asked for when the platform starts the run, and none is before"
		}
		if signup.Instance != nil {
			instance := *signup.Instance
			instance.Waiting = true
			out[i].Instance = &instance
		}
		out[i].StatusReason = &domain.StatusReason{Reason: why.Reason, Message: waits + ": " + why.Message, WaitsFor: who}
	}
	return out, nil
}

// reasonInstanceStarting is the status_reason of an agent whose machine is on its way.
const reasonInstanceStarting = "instance_starting"

// neverLaunched is the status_reason of an agent of a platform experiment closed for why that
// waited for a run that never started: no machine was asked for it and it took no turn.
func neverLaunched(why *domain.StatusReason) *domain.StatusReason {
	return &domain.StatusReason{Reason: "never_launched", Message: "it never took a turn and no machine was asked for it: its platform experiment closed before the run started, with close_reason " + why.Reason + ": " + why.Message}
}

// instanceStarting is the status_reason of a hosted agent whose run goes on and whose machine is
// not running yet, nil for every other. Its instance reads requested or starting then, which is
// a start in progress and was read as a launch that failed.
func instanceStarting(signup domain.AgentSignup, now time.Time) *domain.StatusReason {
	if !signup.Hosted || signup.Instance == nil || signup.Ended != "" {
		return nil
	}
	instance := *signup.Instance
	state := instance.Shown()
	if state != domain.InstanceRequested && state != domain.InstanceStarting {
		return nil
	}
	where := "its machine is asked of its launcher at the launcher's next pass"
	switch {
	case instance.Name != "" && instance.AskedAt != nil && state == domain.InstanceStarting:
		where = fmt.Sprintf("its machine was asked for %s ago and is starting", now.Sub(*instance.AskedAt).Round(time.Second))
	case instance.Name != "" && instance.AskedAt != nil:
		where = fmt.Sprintf("its machine was asked for %s ago and its launcher has not listed it yet", now.Sub(*instance.AskedAt).Round(time.Second))
	}
	return &domain.StatusReason{Reason: reasonInstanceStarting, Message: where +
		": a start in progress and no failure. A machine that does not come up is replaced, which restarts counts, and one given up on reads stopped with end_reason and exit_log"}
}

// refuseSignup is a join or a removal refused for a reason the caller can act on. Reason leads
// the message, as every signup refusal always has.
func refuseSignup(status int, reason, format string, args ...any) error {
	return api.Refuse(status, reason, reason+": "+fmt.Sprintf(format, args...))
}

// AddAgent joins one agent to a platform experiment: the registered agent req names, or a new
// one created in the experiment's org. by is whose credential asked. The answer holds that one
// agent.
func (s *PlatformExperimentsService) AddAgent(ctx context.Context, platformExpID string, req AddAgentRequest, by domain.Actor, launch AgentLaunch) ([]domain.AgentSignup, error) {
	role, err := domain.ParseSignupRole(req.Role)
	if err != nil {
		return nil, refuseSignup(400, "unknown_role", "%s", err)
	}
	if !domain.ValidQuotaTierOverride(req.QuotaTier) {
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
	choice, err := domain.ResolveAgentLaunch(catalog, domain.RoleOfSignup(role), req.Flavor, req.Model, req.Hyperparameters)
	if err != nil {
		return nil, refuseSignup(400, "invalid_request", "%s", err)
	}
	signup := domain.AgentSignup{
		AgentID: req.AgentID, Role: role, QuotaTier: domain.QuotaTier(req.QuotaTier),
		Hosted: req.Hosted, AgentType: choice.AgentType, Hyperparameters: choice.Hyperparameters, Model: choice.Model, Actor: &by,
	}
	if req.Hosted {
		signup.Launcher = launch.Launcher
		if err := s.requireLauncher(ctx, launch.Launcher); err != nil {
			return nil, err
		}
	}
	member := false
	if req.AgentID != "" {
		if member, err = s.store.IsSignedUp(ctx, platformExpID, req.AgentID); err != nil {
			return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
		}
	}
	var late *db.LateJoin
	if pe.Status == domain.PlatformExpRunning && !member {
		if late, err = s.lateJoin(ctx, pe, role, by); err != nil {
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
func (s *PlatformExperimentsService) lateJoin(ctx context.Context, pe *domain.PlatformExperiment, role domain.SignupRole, by domain.Actor) (*db.LateJoin, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, refuseSignup(409, "signup_closed", "platform experiment %s is running, and only a researcher (role competitor) joins it then", pe.ID)
	}
	if by.Kind == domain.ActorAgent {
		return nil, refuseSignup(403, "not_coordinator", "an agent adds no agent to a running platform experiment: its coordinator or its owner does")
	}
	return s.lateShare(ctx, pe)
}

// lateShare is what a researcher joining the running pe is given its quota from: what each
// researcher used, and the least share worth giving, one trial of the baseline.
func (s *PlatformExperimentsService) lateShare(ctx context.Context, pe *domain.PlatformExperiment) (*db.LateJoin, error) {
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
func (s *PlatformExperimentsService) researcherQuotas(ctx context.Context, pe *domain.PlatformExperiment) ([]domain.ResearcherQuota, error) {
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
	case errors.Is(err, db.ErrMaxHostedReached):
		return refuseSignup(409, "max_hosted_reached", "%s", maxHostedReached(pe.MaxHosted))
	case errors.Is(err, db.ErrNoShareLeft):
		return refuseSignup(409, "no_share_left", "a researcher that joins running platform experiment %s is given an even part, with the researchers at work, of what all researchers left unspent, and that part would not hold one trial of the baseline: one joins when a researcher at work finishes", pe.ID)
	}
	return nil
}

// agentNameTaken refuses an agent whose name another agent of the platform experiment carries.
func agentNameTaken(name string) error {
	return refuseSignup(409, "agent_name_taken", "another agent of this platform experiment is named %q, and a name tells its agents apart: state another name, or none to have the platform name it by its role", name)
}

// requireLauncher refuses to host anything while the launcher config gives new machines to is
// not registered: its token would be refused, and nothing would ever start.
func (s *PlatformExperimentsService) requireLauncher(ctx context.Context, launcher string) error {
	known, err := s.store.LauncherKnown(ctx, launcher)
	if err != nil {
		return fmt.Errorf("platform_experiments: launcher %s: %w", launcher, err)
	}
	if !known {
		return refuseSignup(503, "launcher_not_registered", "the platform gives hosted machines to launcher %q (config agents.launcher), which is not registered: an operator registers it with `hl-admin launcher add %s`", launcher, launcher)
	}
	return nil
}

// withHosting resolves the coordinator a create asks the platform to run: its launcher and the
// model it asked for, empty for its agent type's, and that the experiment names the repository it
// works in.
func (h *PlatformExperimentsHandler) withHosting(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	if req.CoordinatorModel != "" && !req.CoordinatorHosted {
		return req, refuseSignup(400, "invalid_request", "coordinator_model is set and coordinator_hosted is not")
	}
	req.maxHosted = h.launch.MaxHosted
	if !req.CoordinatorHosted {
		return req, nil
	}
	if h.launch.Launcher == "" || h.launch.CoordinatorModel == "" {
		return req, errors.New("platform_experiments.Create: the handler has no agent launch settings")
	}
	if err := h.svc.requireLauncher(ctx, h.launch.Launcher); err != nil {
		return req, err
	}
	if req.CoordinatorModel != "" {
		if err := domain.ValidateModel(req.CoordinatorModel); err != nil {
			return req, refuseSignup(400, "invalid_request", "coordinator_model: %s", err)
		}
	}
	// Checked before anything is made for the experiment: its repository is created ahead of
	// the write that would refuse this.
	if h.launch.MaxHosted < 2 {
		return req, refuseSignup(409, "max_hosted_reached", "%s", maxHostedReached(h.launch.MaxHosted))
	}
	req.coordinator = &domain.HostedCoordinator{Launcher: h.launch.Launcher, Model: req.CoordinatorModel}
	return req, nil
}

// hostedAgents is the agents a platform experiment whose coordinator the platform hosts is given
// in the write that makes it open, each run by the platform and added by by: one for each
// researcher asked for, and after them the one that measures the baseline, whose machine is asked
// for at once beside the coordinator's, so that nothing waits for anyone to add it. An accepted
// draft and a create with coordinator_hosted both get theirs here.
func (h *PlatformExperimentsHandler) hostedAgents(catalog domain.AgentTypeCatalog, researchers []ProposedAgent, by domain.Actor, now time.Time) ([]*domain.Agent, []domain.AgentSignup, error) {
	agents := make([]*domain.Agent, 0, len(researchers)+1)
	signups := make([]domain.AgentSignup, 0, len(researchers)+1)
	for i, asked := range append(slices.Clone(researchers), ProposedAgent{}) {
		role := domain.SignupRoleCompetitor
		if i == len(researchers) {
			role = domain.SignupRoleBaseline
		}
		choice, err := domain.ResolveAgentLaunch(catalog, domain.RoleOfSignup(role), asked.Flavor, asked.Model, asked.Hyperparameters)
		if err != nil {
			return nil, nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		agentID, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			return nil, nil, err
		}
		agents = append(agents, &domain.Agent{ID: agentID, CreatedAt: now})
		signups = append(signups, domain.AgentSignup{
			AgentID: agentID, Role: role, Hosted: true, Launcher: h.launch.Launcher,
			AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: choice.Hyperparameters, Actor: &by,
		})
	}
	return agents, signups, nil
}

func (s *PlatformExperimentsService) joinRegistered(ctx context.Context, pe *domain.PlatformExperiment, req AddAgentRequest, signup domain.AgentSignup, late *db.LateJoin) ([]domain.AgentSignup, error) {
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

func (s *PlatformExperimentsService) spawn(ctx context.Context, pe *domain.PlatformExperiment, req AddAgentRequest, signup domain.AgentSignup, late *db.LateJoin) ([]domain.AgentSignup, error) {
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
		zap.Bool("hosted", signup.Hosted), zap.String("launcher", signup.Launcher))
	return []domain.AgentSignup{*joined}, nil
}

// RemoveAgent takes an agent out of a platform experiment that has not started. Once it has, the
// roster is what quota was divided over: an agent leaves by being cut or by the experiment closing.
// by is whose credential asked: a coordinator's token removes only an agent a coordinator's token
// of this platform experiment added, and never one a person or an API key did.
func (s *PlatformExperimentsService) RemoveAgent(ctx context.Context, platformExpID, agentID string, by domain.Actor) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	if by.Kind == domain.ActorCoordinator {
		signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
		}
		if signup == nil {
			return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
		}
		if signup.Actor == nil || signup.Actor.Kind != domain.ActorCoordinator || signup.Actor.ID != by.ID {
			return refuseSignup(403, "agent_not_added_by_coordinator", "agent %s was not added by this platform experiment's coordinator, and a coordinator's token removes only the agents it added itself: its owner removes this one", agentID)
		}
	}
	err = s.store.RemoveAgent(ctx, platformExpID, agentID, s.dataStore.DeleteDataKey)
	switch {
	case errors.Is(err, db.ErrNotSignedUp):
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

// FinishAgent records that an agent has nothing left to do, which releases its machine, and
// answers its signup as finished. Calling it again changes nothing. The finish of an agent of
// role baseline is the hand-over of what it measured: the platform declares baselineJobID, or
// without it the one job it left (DeclareMeasured), in this request, which starts the run when
// nothing else is missing. A baselineJobID that could not be declared refuses the finish.
//
// A researcher that finishes with more than finishEarlyShare of the run's time and of its quota
// left, and more time than its jobs typically take, names the blocker that stops it, and the time and quota it leaves are recorded with its
// reason, which agent.finished tells the coordinator.
func (s *PlatformExperimentsService) FinishAgent(ctx context.Context, platformExpID, agentID, reason, blocker, baselineJobID string) (*domain.AgentSignup, error) {
	reason, err := s.finishReason(ctx, platformExpID, agentID, reason, blocker)
	if err != nil {
		return nil, err
	}
	if baselineJobID != "" {
		if err := s.checkNamedBaseline(ctx, platformExpID, agentID, baselineJobID); err != nil {
			return nil, err
		}
	}
	err = s.store.FinishAgent(ctx, platformExpID, agentID, domain.EventDetail(reason))
	switch {
	case errors.Is(err, db.ErrNotSignedUp):
		return nil, refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	case errors.Is(err, db.ErrSignupClosed):
		pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
		}
		// The same answer as to anything else sent past the close (RequireActive): the token's
		// wind-down is for filing summaries, and the close already ended the agent's run.
		if pe == nil || pe.Status == domain.PlatformExpClosed {
			return nil, api.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed: your run is over and nothing is left to finish")
		}
		return nil, refuseSignup(409, "not_running", "platform experiment %s is %s: only an agent of role baseline finishes before it runs", platformExpID, pe.Status)
	case errors.Is(err, db.ErrWorkUnfinished):
		return nil, refuseSignup(409, "work_unfinished", "agent %s has a job queued or running, or a completed job without its summary: cancel or wait for the one, write the other, then finish", agentID)
	case err != nil:
		return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
	}
	finished, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
	}
	if finished == nil {
		return nil, fmt.Errorf("platform_experiments.FinishAgent: agent %s is not signed up after finishing", agentID)
	}
	s.logger.Info("agent finished", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	if finished.Role == domain.SignupRoleBaseline {
		// The finish is stored, so it is answered as one: a declaration that fails now is the
		// sweep's to make again, and the experiment's status_reason says what it waits for.
		if err := s.declareFinished(ctx, platformExpID, baselineJobID); err != nil {
			s.logger.Error("declare the baseline of a finished agent", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID), zap.String("jobID", baselineJobID), zap.Error(err))
		}
	}
	return finished, nil
}

// checkNamedBaseline refuses, before anything is stored, a finish whose agent names as the
// baseline baselineJobID and could not have it declared: the agent is of role baseline, the job
// is its own, it passes the check of PATCH baseline_job_id, and no other job of the agent beat it.
func (s *PlatformExperimentsService) checkNamedBaseline(ctx context.Context, platformExpID, agentID, baselineJobID string) error {
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.checkNamedBaseline: %w", err)
	}
	if signup == nil {
		return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	}
	if signup.Role != domain.SignupRoleBaseline {
		return refuseSignup(422, "not_a_baseline_agent", "agent %s is of role %s, and only an agent of role baseline names the baseline it measured: finish without baseline_job_id", agentID, signup.Role)
	}
	pe, err := s.stored(ctx, platformExpID)
	if err != nil {
		return err
	}
	switch {
	case pe.Status == domain.PlatformExpClosed || pe.BaselineJobID == baselineJobID:
		return nil
	case pe.BaselineJobID != "":
		return refuseSignup(409, "baseline_declared", "platform experiment %s already has job %s declared as its baseline: finish without baseline_job_id, or name that job", platformExpID, pe.BaselineJobID)
	case pe.Status != domain.PlatformExpOpen:
		return nil
	}
	baseline, err := s.baseline(ctx, pe, baselineJobID)
	if err != nil {
		return fmt.Errorf("platform_experiments: read baseline %s: %w", baselineJobID, err)
	}
	if baseline == nil || baseline.Job == nil || baseline.Job.AgentID != agentID || baseline.Job.PlatformExperimentID != platformExpID {
		return refusedBaseline("no_baseline", "baseline_job_id %q is no job of yours in platform experiment %s: name the COMPLETED job of the control you submitted (`hl job list`)", baselineJobID, platformExpID)
	}
	declared := *pe
	declared.BaselineJobID = baselineJobID
	_, err = s.heldToBaseline(ctx, &declared, true)
	return err
}

// finishEarlyShare is the share of the run's time and of a researcher's quota that, both left,
// makes its finish one that needs a blocker.
const finishEarlyShare = 0.1

// finishReason is what the finish of agentID records as its reason: reason, the blocker, and for
// a researcher in a run that has a clock the time and quota it leaves. A researcher with more than
// finishEarlyShare of both left, and more time than its jobs typically take (typicalJobTime), and
// no blocker is refused, told what is left.
func (s *PlatformExperimentsService) finishReason(ctx context.Context, platformExpID, agentID, reason, blocker string) (string, error) {
	if blocker != "" {
		reason = sentence(reason) + " Blocker: " + blocker
	}
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if signup == nil || signup.Role != domain.SignupRoleCompetitor || signup.FinishedAt != nil {
		return reason, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if pe == nil || pe.Status != domain.PlatformExpRunning || pe.EndsAt.IsZero() {
		return reason, nil
	}
	quota, err := s.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if quota == nil {
		return reason, nil
	}
	run := time.Duration(pe.DurationHours * float64(time.Hour))
	timeLeft := max(0, time.Until(pe.EndsAt))
	room := max(quota.Remaining(domain.CapacityGuaranteed), quota.Remaining(domain.CapacityBurst))
	left := fmt.Sprintf("%d of the run's %d minutes and %.3g of the agent's %.3g AccH were left, room for a job of %.3g AccH",
		int(timeLeft.Minutes()), int(run.Minutes()), max(0, quota.Share()-quota.Used()), quota.Share(), room)
	// A researcher was refused with 4 minutes left, when its jobs took 2 to 7, and sat idle
	// until the close: time no job of it fits in is no time left.
	typical, err := s.typicalJobTime(ctx, platformExpID, agentID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	enough := max(time.Duration(finishEarlyShare*float64(run)), typical)
	if blocker == "" && timeLeft > enough && room > finishEarlyShare*quota.Share() {
		return "", refuseSignup(409, "time_and_quota_left", "%s. Keep pursuing credible improvements while both last. "+
			"If something stops you that this time and quota cannot fix, finish again with --blocker saying what it is "+
			"(the request's blocker field): a blocker written into --reason is not read as one", left)
	}
	return sentence(reason) + " When it finished, " + left + ".", nil
}

// typicalJobTime is the median time agentID's COMPLETED jobs in the platform experiment took from
// submit to end, or with none of its own every COMPLETED job's there, and 0 with none at all.
func (s *PlatformExperimentsService) typicalJobTime(ctx context.Context, platformExpID, agentID string) (time.Duration, error) {
	jobs, err := s.store.CompletedJobs(ctx, platformExpID)
	if err != nil {
		return 0, err
	}
	var own, all []time.Duration
	for _, job := range jobs {
		if job.FinishedAt == nil || job.FinishedAt.Before(job.CreatedAt) {
			continue
		}
		took := job.FinishedAt.Sub(job.CreatedAt)
		all = append(all, took)
		if job.AgentID == agentID {
			own = append(own, took)
		}
	}
	if len(own) == 0 {
		own = all
	}
	if len(own) == 0 {
		return 0, nil
	}
	slices.Sort(own)
	return own[len(own)/2], nil
}

// sentence is text ending in a full stop, so that another can follow it on the same line.
func sentence(text string) string {
	return strings.TrimRight(text, ". ") + "."
}

// WithAgentLaunch attaches what joining needs beyond the store.
func (h *PlatformExperimentsHandler) WithAgentLaunch(launch AgentLaunch) *PlatformExperimentsHandler {
	h.launch = launch
	return h
}

// refusal answers a refusal as it is; anything else is the platform's own failure, logged and
// answered without detail.
func (h *PlatformExperimentsHandler) refusal(op string, err error) error {
	var refused *api.Refusal
	if errors.As(err, &refused) {
		return refused
	}
	return auth.Internal(h.logger, op+" failed", err)
}

// nameAddedBy names whoever added each agent.
func (h *PlatformExperimentsHandler) nameAddedBy(ctx context.Context, signups []domain.AgentSignup) error {
	actors := make([]*domain.Actor, 0, len(signups))
	for i := range signups {
		if signups[i].Actor != nil {
			actors = append(actors, signups[i].Actor)
		}
	}
	if len(actors) == 0 {
		return nil
	}
	return h.svc.store.NameActors(ctx, actors)
}

type joinedAgentsBody struct {
	Agents []JoinedAgent `json:"agents"`
}

// Hypotheses reads the hypothesis graph, through the graph's own package. Implemented by
// graphdb.Store.
type Hypotheses interface {
	ListHypotheses(ctx context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error)
}

// OpenHypothesis is a hypothesis an agent left without a verdict: its own, or a person's idea its
// jobs tested.
type OpenHypothesis struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// maxOpenHypotheses is how many a finish answers: the largest page the graph lists.
const maxOpenHypotheses = 200

type finishedAgentBody struct {
	Agents         []JoinedAgent    `json:"agents"`
	OpenHypotheses []OpenHypothesis `json:"open_hypotheses" doc:"The hypotheses still open that are the agent's verdict to give, newest first, at most 200: its own, then the ideas of people no agent owns that a completed job of its tested. Each stays open for good unless the agent gives its verdict (POST /hypotheses/{id}/status) while its token still works. Empty when it left none. Nothing is refused for them."`
}

// openHypotheses is what the agent leaves without a verdict in the platform experiment: its own
// hypotheses, then the hypotheses no agent owns that one of its completed jobs tested, whose
// verdict is as much its to give.
func openHypotheses(ctx context.Context, graph Hypotheses, completed []*domain.Experiment, platformExpID, agentID string) ([]OpenHypothesis, error) {
	items, err := graph.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExpID, AgentID: agentID, Status: domain.HypothesisOpen, Limit: maxOpenHypotheses})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.openHypotheses: %w", err)
	}
	tested := []string{}
	for _, job := range completed {
		if job.AgentID == agentID && job.PlatformExperimentID == platformExpID && job.HypothesisID != "" && !slices.Contains(tested, job.HypothesisID) {
			tested = append(tested, job.HypothesisID)
		}
	}
	if len(tested) > 0 && len(items) < maxOpenHypotheses {
		others, err := graph.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExpID, Status: domain.HypothesisOpen, IDs: tested, Limit: maxOpenHypotheses})
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.openHypotheses: %w", err)
		}
		for _, item := range others {
			if item.AgentID == "" && len(items) < maxOpenHypotheses {
				items = append(items, item)
			}
		}
	}
	open := make([]OpenHypothesis, len(items))
	for i, item := range items {
		open[i] = OpenHypothesis{ID: item.ID, Text: item.Text}
	}
	return open, nil
}

// RegisterAgentsHuma registers how agents join and leave a platform experiment. graph is where a
// finish reads what the agent left open.
func RegisterAgentsHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler, graph Hypotheses) {
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" || h.launch.SigningSecret == "" || h.launch.MaxHosted <= 0 || h.repos == nil || graph == nil {
		panic("quota.RegisterAgentsHuma: the handler needs its repositories, WithAgentLaunch with every field set and the hypothesis graph")
	}
	access := doc.Access
	registerMessagesHuma(doc, h)

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "add-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents",
		Summary: "Add one agent to a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "The only way an agent joins, while the experiment is open: the platform starts the run once its baseline is declared, and quota is divided then over the agents present, those of role baseline apart. " +
			"While it runs, its coordinator or a person adds a researcher (role competitor, and no other role) in the place of one that finished, was cut or lost its machine, or in one its owner gained by raising max_agents: it is given an even part, with the researchers at work, of what all researchers left unspent, taken first from what the ones that ended left and then from the researchers at work that hold more unspent than that part, in proportion to what they hold above it, so none keeps less than the newcomer and none gives back what it used, and 409 no_share_left when that part would not hold one trial of the baseline (the job again at job_estimated_duration_hours). An agent's token adds no agent then (403). Adding the researcher a declared baseline still waited for starts the run in this request. " +
			"Creates one agent with a generated id and signs it up; or, with agent_id, signs up that registered agent. For several agents, call once for each. " +
			"An agent's name is unique in the platform experiment and, when the platform gives it, says its role: 409 agent_name_taken for a stated name, or a registered agent's, that another agent of it carries. " +
			"The answer is agents, a list holding that one agent. " +
			"hosted=true has the platform start a machine for the agent when the experiment starts and keep it until the experiment closes, the agent is cut or it finishes. " +
			"Until the start the agent answered carries status_reason, the reason the platform experiment is not running yet, and its instance reads waiting: no machine exists before the start. " +
			"409 max_hosted_reached beyond max_hosted machines at once, of which one is kept for an agent of role baseline: when one finished without a baseline that can be declared, or its machine stopped with no restart left, the next is added in its place. " +
			"flavor and model are resolved here and stored: the answer shows what the agent runs. " +
			"hosted=false returns the agent's token: run the agent yourself with it as HL_TOKEN and the API's address as API_URL, which is all it is started with. " +
			"The token acts as that agent in that platform experiment and nowhere else, and asking again with agent_id returns it again. " +
			"role decides ranking: \"competitor\" (default) is a researcher, whatever its flavor: ranked, cut-eligible and counted against max_agents; \"baseline\" and " +
			"\"reviewer\" are none of the three, though their jobs are billed and gated identically. " +
			"409 max_agents_reached for a researcher past max_agents, which counts the researchers at work, none that finished, was cut, was replaced or lost its machine with no restart left, and no agent of another role and no coordinator. 409 signup_closed for any role but researcher once the run started, and for every one once it closed. " +
			"An agent of role baseline measures the baseline before the start: its machine, when hosted, starts at once, it holds a quota from the moment it joins (half the budget, which no researcher can spend before the start, so a failed measurement can be run again; it follows a later change of the budget while the experiment is open)," +
			"and its job is the only one an open experiment accepts. It takes no share at the start, and what it spent is taken from the budget before the others' shares are carved.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body AddAgentRequest
	}) (*struct{ Body joinedAgentsBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if in.Body.AgentID != "" {
			if err := access.Agent(ctx, in.Body.AgentID); err != nil {
				return nil, err
			}
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		signups, err := h.svc.AddAgent(ctx, in.ID, in.Body, caller.Actor(), h.launch)
		if err != nil {
			return nil, h.refusal("add agent", err)
		}
		if err := h.nameAddedBy(ctx, signups); err != nil {
			return nil, h.refusal("add agent", err)
		}
		out := &struct{ Body joinedAgentsBody }{}
		if out.Body.Agents, err = h.svc.roster(ctx, in.ID, signups, caller.Actor()); err != nil {
			return nil, h.refusal("add agent", err)
		}
		for i, signup := range signups {
			if signup.Hosted {
				continue
			}
			token, err := auth.Mint(h.launch.SigningSecret, auth.AgentScope(in.ID, signup.AgentID))
			if err != nil {
				return nil, h.refusal("mint agent token", err)
			}
			out.Body.Agents[i].Token = token
		}
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-platform-experiment-agents", Method: "GET", Path: "/platform-experiments/{id}/agents",
		Summary: "List the agents of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Every agent that joined, in the order they did, with its role, what it was launched with and actor, whose credential added it. No token is listed. " +
			"A hosted agent carries instance: the name its machine was last asked for under, how many times it exited and was started again, " +
			"and once it exited with no restart left, stopped=true with exited_at and exit_log, the end of what its command wrote. " +
			"A hosted agent whose machine is up carries activity {text, at}: what the agent is doing now, in a short sentence its harness writes from the tool call in flight (\"thinking\", \"running hl job submit\", \"waiting for platform updates\"), and when the platform first read that sentence. Only the latest is kept, it is read from the machine about every 15 seconds, and it is absent while no machine is up. The coordinator's is coordinator.activity on GET /platform-experiments/{id}, and a draft's planner's is activity there. " +
			"Every agent carries in_flight {jobs, queued, running}: how many of its jobs are queued or running now. 0 is an agent with nothing of its own on the platform, whatever its activity says. " +
			"While the platform experiment is open or running, an agent whose run goes on carries messages {left_this_hour, next_at}, how many more messages the caller may send it this hour, and in a running one minutes_without_job_in_flight. " +
			"A hosted one whose machine reports carries what its harness last said of that machine: subagents, the subagents running, turn {state working or waiting, since}, and model_allowance {utilization, resets_at, refused_until}. GET /platform-experiments/{id}/situation reads the same in one answer with the run's clock, objective, capacity and budget. " +
			"While the platform experiment is open, every agent but those of role baseline waits for the start and carries status_reason: the reason the platform experiment is not running yet, and that no machine is asked for the agent before it is. Its instance reads waiting until then. One that closed before its run started leaves each of them status_reason never_launched, whose message gives the close_reason and why it closed.A hosted agent whose machine is asked for and not running yet, its instance reading requested or starting, carries status_reason instance_starting with how long ago the machine was asked for: a start in progress, and a failed one reads stopped with end_reason.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body joinedAgentsBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		pe, err := h.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, h.refusal("list agents", err)
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		signups, err := h.svc.store.ListAgentSignups(ctx, in.ID)
		if err != nil {
			return nil, h.refusal("list agents", err)
		}
		if err := h.nameAddedBy(ctx, signups); err != nil {
			return nil, h.refusal("list agents", err)
		}
		out := &struct{ Body joinedAgentsBody }{}
		if out.Body.Agents, err = h.svc.roster(ctx, in.ID, signups, caller.Actor()); err != nil {
			return nil, h.refusal("list agents", err)
		}
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "remove-platform-experiment-agent", Method: "DELETE", Path: "/platform-experiments/{id}/agents/{agent_id}",
		Summary: "Remove an agent from a platform experiment that has not started", Tags: []string{"platform-experiments"},
		DefaultStatus: http.StatusNoContent,
		Description: "Takes the agent out while the experiment is open: its token stops working and, if the platform runs it, its machine is taken down. " +
			"Only its place goes: the agent stays in the org and joins again with agent_id. " +
			"409 once the experiment has started, when the roster is what quota was divided over: from then on an agent leaves by being cut or by the experiment closing. " +
			"409 work_unfinished while the agent has a job queued or running, which an agent of role baseline may: the quota that job is charged to goes with the agent, so the job is cancelled or ends first. " +
			"A coordinator's token removes only an agent it added itself (actor.kind coordinator in the roster): 403 agent_not_added_by_coordinator for one a person or an API key added, which its owner removes.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct{}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		if err := h.svc.RemoveAgent(ctx, in.ID, in.AgentID, caller.Actor()); err != nil {
			return nil, h.refusal("remove agent", err)
		}
		return &struct{}{}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "finish-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents/{agent_id}/finish",
		Summary: "Say that an agent has nothing left to do", Tags: []string{"platform-experiments"},
		DefaultStatus: 200,
		Description: "Called by the agent itself, with its own token, in a running platform experiment, and by an agent of role baseline also in an open one, where it measures the baseline. From then its machine, if the platform runs it, is taken down after its last turn, " +
			"its signup reads finished_at and finished_reason, and agent.finished is sent. The answer is agents, a list holding that one agent as finished, and open_hypotheses, the id and text of each hypothesis of the agent that still has no verdict: the finish is taken whatever they are. Its quota, jobs and findings stay as they are, and its token stops working two minutes later. " +
			"A researcher's finished_reason, which agent.finished carries to the coordinator, ends with the time and quota it left, after the blocker it named. " +
			"409 time_and_quota_left, saying how much of each is left, for a researcher that has more than a tenth of the run's time and of its quota left and names no blocker. " +
			"409 work_unfinished while it has a job queued or running or a completed job without its summary, 409 experiment_closed once the platform experiment closed, which ended the agent's run already and leaves nothing to finish, 409 not_running in one not started yet for any role but baseline. Calling it again changes nothing. " +
			"The finish of an agent of role baseline in an open platform experiment hands over what it measured: with baseline_job_id the platform declares that job as the baseline in this request, as PATCH /platform-experiments/{id} with baseline_job_id does, and starts the run when nothing else is missing. " +
			"The job must be the agent's own and pass every check of that PATCH, or the finish is refused with its 422 (no_baseline, baseline_not_completed, baseline_without_finding, baseline_missing_metric, baseline_breaks_constraint, budget_below_baseline and the others) and nothing is stored, so the agent fixes what it says and finishes again. " +
			"422 not_a_baseline_agent for baseline_job_id from an agent of another role, 409 baseline_declared when it names another job than the baseline already declared. " +
			"Without baseline_job_id, when the baseline agents that finished left exactly one COMPLETED job that reports the ranking metric, the platform declares that job the same way. " +
			"Such a finish is answered 200 whatever became of the declaration: one that is refused, or a finish that left no such job or several, declares nothing, and the platform experiment's status_reason says which it was and who acts (baseline_required, baseline_undeclared, or the refusal of the declaration such as budget_below_baseline).",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
		Body    struct {
			Reason        string `json:"reason" minLength:"1" doc:"Why nothing is left to do: quota spent, or what was tried and why no further idea is worth a job."`
			Blocker       string `json:"blocker,omitempty" doc:"What stops the agent that the time and quota it has left cannot fix. Required of a researcher with more than a tenth of the run's time and of its quota left."`
			BaselineJobID string `json:"baseline_job_id,omitempty" doc:"For an agent of role baseline: its COMPLETED job the platform declares as the baseline in this request, the control the run is ranked against. Needed when it left several jobs that report the ranking metric."`
		}
	}) (*struct{ Body finishedAgentBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		if caller.Kind != auth.KindAgent || caller.AgentID != in.AgentID || caller.PlatformExperimentID != in.ID {
			return nil, huma.Error403Forbidden("an agent finishes itself, with its own token")
		}
		// Read before the finish is stored: a graph that does not answer leaves nothing changed.
		out := &struct{ Body finishedAgentBody }{}
		completed, err := h.svc.store.CompletedJobs(ctx, in.ID)
		if err != nil {
			return nil, h.refusal("finish agent", err)
		}
		if out.Body.OpenHypotheses, err = openHypotheses(ctx, graph, completed, in.ID, in.AgentID); err != nil {
			return nil, h.refusal("finish agent", err)
		}
		finished, err := h.svc.FinishAgent(ctx, in.ID, in.AgentID, in.Body.Reason, strings.TrimSpace(in.Body.Blocker), strings.TrimSpace(in.Body.BaselineJobID))
		if err != nil {
			return nil, h.refusal("finish agent", err)
		}
		signups := []domain.AgentSignup{*finished}
		if err := h.nameAddedBy(ctx, signups); err != nil {
			return nil, h.refusal("finish agent", err)
		}
		if out.Body.Agents, err = h.svc.roster(ctx, in.ID, signups, caller.Actor()); err != nil {
			return nil, h.refusal("finish agent", err)
		}
		return out, nil
	})
}
