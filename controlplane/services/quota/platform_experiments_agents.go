package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// AddAgentRequest is the body of POST /platform-experiments/{id}/agents: the one way an agent
// joins a platform experiment.
type AddAgentRequest struct {
	AgentID         string         `json:"agent_id,omitempty" doc:"Join as this registered agent instead of creating a new one. Joining again once joined succeeds and changes nothing."`
	Name            string         `json:"name,omitempty" doc:"Display name of a created agent. Omitted, it is named by its place in the platform experiment, \"Researcher 2\"."`
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

// signupRefusal is a join or a removal refused for a reason the caller can act on. Reason leads
// the message, as every signup refusal always has.
type signupRefusal struct {
	status int
	reason string
	detail string
}

func (e *signupRefusal) Error() string { return e.reason + ": " + e.detail }

func refuseSignup(status int, reason, format string, args ...any) error {
	return &signupRefusal{status: status, reason: reason, detail: fmt.Sprintf(format, args...)}
}

// AddAgent joins one agent to a platform experiment: the registered agent req names, or a new
// one created in the experiment's org. The answer holds that one agent.
func (s *PlatformExperimentsService) AddAgent(ctx context.Context, platformExpID string, req AddAgentRequest, launch AgentLaunch) ([]domain.AgentSignup, error) {
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
		Hosted: req.Hosted, Flavor: flavor, Hyperparameters: req.Hyperparameters, Model: model,
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

// nextAgentName is what a created agent is called when the caller named none: "Researcher 3"
// for the third agent of the experiment, which is what a person reads in every list in place of
// a generated id. Two agents added at the same
// moment, or one added after another left, may share a name; the id is what tells them apart.
func (s *PlatformExperimentsService) nextAgentName(ctx context.Context, platformExpID string) (string, error) {
	joined, err := s.store.ListSignups(ctx, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.AddAgent: %w", err)
	}
	return fmt.Sprintf("Researcher %d", len(joined)+1), nil
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
	name := req.Name
	if name == "" {
		if name, err = s.nextAgentName(ctx, platformExpID); err != nil {
			return nil, err
		}
	}
	signup.AgentID = id
	agent := &domain.Agent{ID: id, Name: name, PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
	err = s.store.SpawnAgent(ctx, platformExpID, agent, signup)
	switch {
	case errors.Is(err, db.ErrSignupClosed):
		return nil, refuseSignup(400, "signup_closed", "experiment is %s", pe.Status)
	case errors.Is(err, db.ErrMaxAgentsReached):
		return nil, refuseSignup(400, "max_agents_reached", "limit is %d", pe.MaxAgents)
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
func (s *PlatformExperimentsService) RemoveAgent(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RemoveAgent: %w", err)
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	err = s.store.RemoveAgent(ctx, platformExpID, agentID)
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

// WithAgentLaunch attaches what joining needs beyond the store.
func (h *PlatformExperimentsHandler) WithAgentLaunch(launch AgentLaunch) *PlatformExperimentsHandler {
	h.launch = launch
	return h
}

// refusal answers a signupRefusal in the {"reason","message"} envelope; anything else is the
// platform's own failure, logged and answered without detail.
func (h *PlatformExperimentsHandler) refusal(op string, err error) error {
	var refused *signupRefusal
	if errors.As(err, &refused) {
		return &reasonError{status: refused.status, Reason: refused.reason, Message: refused.Error()}
	}
	h.logger.Error(op+" failed", zap.Error(err))
	return huma.Error500InternalServerError(auth.InternalError)
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
		Description: "The only way an agent joins, and only while the experiment is open: quota is divided at start over the agents present. " +
			"Creates one agent with a generated id and signs it up; or, with agent_id, signs up that registered agent. For several agents, call once for each. " +
			"The answer is agents, a list holding that one agent. " +
			"hosted=true has the platform start and keep a machine for the agent until the experiment closes or the agent is cut. " +
			"In a platform experiment that names no repository it starts in an empty directory and the platform keeps the code it pushes (hl code push). " +
			"flavor and model are resolved here and stored: the answer shows what the agent runs. " +
			"hosted=false returns the agent's token: run the agent yourself with it as HL_TOKEN and the API's address as API_URL, which is all it is started with. " +
			"The token acts as that agent in that platform experiment and nowhere else, and asking again with agent_id returns it again. " +
			"role decides ranking: \"competitor\" (default) is ranked, cut-eligible and counted against max_agents; \"baseline\" and " +
			"\"reviewer\" are none of the three, though their jobs are billed and gated identically.",
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
		signups, err := h.svc.AddAgent(ctx, in.ID, in.Body, h.launch)
		if err != nil {
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
		Description: "Every agent that joined, in the order they did, with its role and what it was launched with. No token is listed. " +
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
			"409 once the experiment has started, when the roster is what quota was divided over: from then on an agent leaves by being cut or by the experiment closing.",
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
		if err := h.svc.RemoveAgent(ctx, in.ID, in.AgentID); err != nil {
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
}
