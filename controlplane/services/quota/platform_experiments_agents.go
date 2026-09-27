package quota

import (
	"context"
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
	Token string `json:"token,omitempty"`
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
	flavor, model, err := domain.ResolveAgentLaunch(req.Flavor, req.Model, launch.DefaultModel, req.Hyperparameters)
	if err != nil {
		return nil, refuseSignup(400, "invalid_request", "%s", err)
	}
	signup := domain.AgentSignup{
		AgentID: req.AgentID, Role: role, QuotaTier: domain.QuotaTier(req.QuotaTier),
		Hosted: req.Hosted, Flavor: flavor, Hyperparameters: req.Hyperparameters, Model: model, Actor: &by,
	}
	if req.Hosted {
		signup.Launcher = launch.Launcher
		if err := s.requireLauncher(ctx, launch.Launcher); err != nil {
			return nil, err
		}
	}
	if req.AgentID != "" {
		return s.joinRegistered(ctx, platformExpID, req, signup)
	}
	return s.spawn(ctx, platformExpID, req, signup)
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

// withHosting resolves the coordinator a create asks the platform to run: its launcher and
// model, and that the experiment names the repository it works in.
func (h *PlatformExperimentsHandler) withHosting(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	if req.CoordinatorModel != "" && !req.CoordinatorHosted {
		return req, refuseSignup(400, "invalid_request", "coordinator_model is set and coordinator_hosted is not")
	}
	req.maxHosted = h.launch.MaxHosted
	if !req.CoordinatorHosted {
		return req, nil
	}
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" {
		return req, errors.New("platform_experiments.Create: the handler has no agent launch settings")
	}
	if err := h.svc.requireLauncher(ctx, h.launch.Launcher); err != nil {
		return req, err
	}
	_, model, err := domain.ResolveAgentLaunch("", req.CoordinatorModel, h.launch.DefaultModel, nil)
	if err != nil {
		return req, refuseSignup(400, "invalid_request", "coordinator_model: %s", err)
	}
	req.coordinator = &domain.HostedCoordinator{Launcher: h.launch.Launcher, Model: model}
	return req, nil
}

func (s *PlatformExperimentsService) joinRegistered(ctx context.Context, platformExpID string, req AddAgentRequest, signup domain.AgentSignup) ([]domain.AgentSignup, error) {
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
	if err := s.Signup(ctx, platformExpID, signup); err != nil {
		return nil, err
	}
	joined, err := s.store.GetAgentSignup(ctx, platformExpID, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if joined == nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: agent %s is not signed up after signing up", req.AgentID)
	}
	return []domain.AgentSignup{*joined}, nil
}

func (s *PlatformExperimentsService) spawn(ctx context.Context, platformExpID string, req AddAgentRequest, signup domain.AgentSignup) ([]domain.AgentSignup, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	if pe == nil {
		return nil, refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	id, err := domain.NewAgentID()
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	signup.AgentID = id
	agent := &domain.Agent{ID: id, Name: strings.TrimSpace(req.Name), PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
	err = s.store.SpawnAgent(ctx, platformExpID, agent, signup)
	switch {
	case errors.Is(err, db.ErrSignupClosed):
		return nil, refuseSignup(400, "signup_closed", "experiment is %s", pe.Status)
	case errors.Is(err, db.ErrAgentNameTaken):
		return nil, agentNameTaken(agent.Name)
	case errors.Is(err, db.ErrMaxAgentsReached):
		return nil, maxAgentsReached(pe.MaxAgents)
	case errors.Is(err, db.ErrMaxHostedReached):
		return nil, refuseSignup(400, "max_hosted_reached", "%s", maxHostedReached(pe.MaxHosted))
	case err != nil:
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
	case err != nil:
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	s.logger.Info("agent removed", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	return nil
}

// FinishAgent records that an agent has nothing left to do, which releases its machine. Calling
// it again changes nothing.
func (s *PlatformExperimentsService) FinishAgent(ctx context.Context, platformExpID, agentID, reason string) error {
	err := s.store.FinishAgent(ctx, platformExpID, agentID, reason)
	switch {
	case errors.Is(err, db.ErrNotSignedUp):
		return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	case errors.Is(err, db.ErrSignupClosed):
		pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
		if err != nil {
			return fmt.Errorf("platform_experiments.FinishAgent: %w", err)
		}
		// The same answer as to anything else sent past the close (RequireActive): the token's
		// wind-down is for filing summaries, and the close already ended the agent's run.
		if pe == nil || pe.Status == domain.PlatformExpClosed {
			return api.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed: your run is over and nothing is left to finish")
		}
		return refuseSignup(409, "not_running", "platform experiment %s is %s: only an agent of role baseline finishes before it runs", platformExpID, pe.Status)
	case errors.Is(err, db.ErrWorkUnfinished):
		return refuseSignup(409, "work_unfinished", "agent %s has a job queued or running, or a completed job without its summary: cancel or wait for the one, write the other, then finish", agentID)
	case err != nil:
		return fmt.Errorf("platform_experiments.FinishAgent: %w", err)
	}
	s.logger.Info("agent finished", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	return nil
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

// RegisterAgentsHuma registers how agents join and leave a platform experiment.
func RegisterAgentsHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" || h.launch.SigningSecret == "" || h.launch.MaxHosted <= 0 || h.repos == nil {
		panic("quota.RegisterAgentsHuma: the handler needs its repositories and WithAgentLaunch with every field set")
	}
	access := doc.Access

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "add-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents",
		Summary: "Add one agent to a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "The only way an agent joins, and only while the experiment is open: the platform starts the run once its baseline is declared, and quota is divided then over the agents present, those of role baseline apart. " +
			"Creates one agent with a generated id and signs it up; or, with agent_id, signs up that registered agent. For several agents, call once for each. " +
			"An agent's name is unique in the platform experiment and, when the platform gives it, says its role: 409 agent_name_taken for a stated name, or a registered agent's, that another agent of it carries. " +
			"The answer is agents, a list holding that one agent. " +
			"hosted=true has the platform start a machine for the agent when the experiment starts and keep it until the experiment closes, the agent is cut or it finishes. " +
			"400 max_hosted_reached beyond max_hosted machines at once, of which one is kept for an agent of role baseline: when one finished without a baseline that can be declared, the next is added in its place. " +
			"flavor and model are resolved here and stored: the answer shows what the agent runs. " +
			"hosted=false returns the agent's token: run the agent yourself with it as HL_TOKEN and the API's address as API_URL, which is all it is started with. " +
			"The token acts as that agent in that platform experiment and nowhere else, and asking again with agent_id returns it again. " +
			"role decides ranking: \"competitor\" (default) is a researcher, whatever its flavor: ranked, cut-eligible and counted against max_agents; \"baseline\" and " +
			"\"reviewer\" are none of the three, though their jobs are billed and gated identically. " +
			"400 max_agents_reached for a researcher past max_agents, which counts every researcher that joined, one that finished or was cut included, and no agent of another role and no coordinator. " +
			"An agent of role baseline measures the baseline before the start: its machine, when hosted, starts at once, it holds a quota from the moment it joins (the first stage's share of the budget over max_agents, what one researcher holds then, which follows a later change of either while the experiment is open), " +
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
		caller, _ := auth.FromContext(ctx)
		signups, err := h.svc.AddAgent(ctx, in.ID, in.Body, caller.Actor(), h.launch)
		if err != nil {
			return nil, h.refusal("add agent", err)
		}
		if err := h.nameAddedBy(ctx, signups); err != nil {
			return nil, h.refusal("add agent", err)
		}
		out := &struct{ Body joinedAgentsBody }{}
		out.Body.Agents = make([]JoinedAgent, len(signups))
		for i, signup := range signups {
			out.Body.Agents[i] = JoinedAgent{AgentSignup: signup}
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
			"and once it exited with no restart left, stopped=true with exited_at and exit_log, the end of what its command wrote.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body joinedAgentsBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
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
		out.Body.Agents = make([]JoinedAgent, len(signups))
		for i, signup := range signups {
			out.Body.Agents[i] = JoinedAgent{AgentSignup: signup}
		}
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "remove-platform-experiment-agent", Method: "DELETE", Path: "/platform-experiments/{id}/agents/{agent_id}",
		Summary: "Remove an agent from a platform experiment that has not started", Tags: []string{"platform-experiments"},
		DefaultStatus: 200,
		Description: "Takes the agent out while the experiment is open: its token stops working and, if the platform runs it, its machine is taken down. " +
			"Only its place goes: the agent stays in the org and joins again with agent_id. " +
			"409 once the experiment has started, when the roster is what quota was divided over: from then on an agent leaves by being cut or by the experiment closing. " +
			"A coordinator's token removes only an agent it added itself (actor.kind coordinator in the roster): 403 agent_not_added_by_coordinator for one a person or an API key added, which its owner removes.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, _ := auth.FromContext(ctx)
		if err := h.svc.RemoveAgent(ctx, in.ID, in.AgentID, caller.Actor()); err != nil {
			return nil, h.refusal("remove agent", err)
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "removed"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "finish-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents/{agent_id}/finish",
		Summary: "Say that an agent has nothing left to do", Tags: []string{"platform-experiments"},
		DefaultStatus: 200,
		Description: "Called by the agent itself, with its own token, in a running platform experiment, and by an agent of role baseline also in an open one, where it measures the baseline. From then its machine, if the platform runs it, is taken down after its last turn, " +
			"its signup reads finished_at and finished_reason, and agent.finished is sent. Its quota, jobs and findings stay as they are, and its token stops working two minutes later. " +
			"409 work_unfinished while it has a job queued or running or a completed job without its summary, 409 experiment_closed once the platform experiment closed, which ended the agent's run already and leaves nothing to finish, 409 not_running in one not started yet for any role but baseline. Calling it again changes nothing.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
		Body    struct {
			Reason string `json:"reason" minLength:"1" doc:"Why nothing is left to do: quota spent, or what was tried and why no further idea is worth a job."`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if caller, _ := auth.FromContext(ctx); caller.Kind != auth.KindAgent || caller.AgentID != in.AgentID || caller.PlatformExperimentID != in.ID {
			return nil, huma.Error403Forbidden("an agent finishes itself, with its own token")
		}
		if err := h.svc.FinishAgent(ctx, in.ID, in.AgentID, in.Body.Reason); err != nil {
			return nil, h.refusal("finish agent", err)
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "finished"
		return out, nil
	})
}
