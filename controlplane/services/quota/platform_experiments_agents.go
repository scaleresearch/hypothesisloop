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

// AddAgentsRequest is the body of POST /platform-experiments/{id}/agents: the one way an agent
// joins a platform experiment.
type AddAgentsRequest struct {
	AgentID         string         `json:"agent_id,omitempty" doc:"Join as this registered agent instead of creating new ones. Joining again once joined succeeds and changes nothing."`
	Count           int            `json:"count,omitempty" doc:"How many agents to create, 1 to 100. Default 1."`
	Name            string         `json:"name,omitempty" doc:"Display name of a created agent; with count above 1 each gets -1, -2, ... appended. Defaults to the generated id."`
	Kind            string         `json:"kind,omitempty" doc:"Kind of a created agent: \"agent\" (default) or \"human\"."`
	Role            string         `json:"role,omitempty" doc:"\"competitor\" (default), \"baseline\" or \"reviewer\"."`
	QuotaTier       string         `json:"quota_tier,omitempty" doc:"\"guaranteed\" or \"burst_only\"; omitted, the platform experiment's default_quota_tier applies."`
	Hosted          bool           `json:"hosted,omitempty" doc:"true: the platform runs the agent. false (default): its token is returned and you run it."`
	Flavor          string         `json:"flavor,omitempty" doc:"Which specialization the agent runs, handed to it as AGENT_FLAVOR: generalist (default), hyperparameter-search, architecture-search or data-search."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object handed to the agent as AGENT_HYPERPARAMETERS, at most 4096 bytes as JSON."`
	Model           string         `json:"model,omitempty" doc:"The model the agent runs on, handed to it as AGENT_MODEL. Omitted, the platform's default applies and is what the answer shows."`
}

// AgentLaunch is what joining needs beyond the store.
type AgentLaunch struct {
	// Launcher is the launcher a hosted agent is given to.
	Launcher string
	// DefaultModel is what an agent that names no model runs on.
	DefaultModel string
	// SigningSecret derives an agent's own token.
	SigningSecret string
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

// AddAgents joins agents to a platform experiment: the registered agent req names, or req.Count
// new ones created in the experiment's org. repos says whether the experiment's project has the
// repository a hosted agent works in.
func (s *PlatformExperimentsService) AddAgents(ctx context.Context, platformExpID string, req AddAgentsRequest, launch AgentLaunch, repos ProjectRepos) ([]domain.AgentSignup, error) {
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
		if err := s.requireRepo(ctx, platformExpID, repos); err != nil {
			return nil, err
		}
	}
	if req.AgentID != "" {
		return s.joinRegistered(ctx, platformExpID, req, signup)
	}
	return s.spawn(ctx, platformExpID, req, signup)
}

// requireRepo refuses to host an agent in a project with no repository attached: the agent
// clones the project's repository before its first turn, and a machine rented for it would only
// start and exit.
func (s *PlatformExperimentsService) requireRepo(ctx context.Context, platformExpID string, repos ProjectRepos) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	return requireProjectRepo(ctx, pe.ProjectID, repos)
}

func requireProjectRepo(ctx context.Context, projectID string, repos ProjectRepos) error {
	cloneURL, err := repos.CloneURL(ctx, projectID)
	if err != nil {
		return fmt.Errorf("platform_experiments: repository of project %s: %w", projectID, err)
	}
	if cloneURL == "" {
		return refuseSignup(409, "no_repository", "project %s has no repository attached, and a hosted agent or coordinator works in the project's repository: attach one to the project, then ask again", projectID)
	}
	return nil
}

// withHosting resolves what a create asks the platform to run: the coordinator's launcher and
// model, and that the project has the repository hosted agents and a coordinator work in.
func (h *PlatformExperimentsHandler) withHosting(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	if req.CoordinatorModel != "" && !req.CoordinatorHosted {
		return req, refuseSignup(400, "invalid_request", "coordinator_model is set and coordinator_hosted is not")
	}
	if req.HostedAgents < 0 || req.HostedAgents > domain.MaxAgentsPerSpawn || req.HostedAgents > req.MaxAgents {
		return req, refuseSignup(400, "invalid_request", "hosted_agents must be between 0 and max_agents, and at most %d", domain.MaxAgentsPerSpawn)
	}
	if !req.CoordinatorHosted && req.HostedAgents == 0 {
		return req, nil
	}
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" {
		return req, errors.New("platform_experiments.Create: the handler has no agent launch settings")
	}
	if err := requireProjectRepo(ctx, req.ProjectID, h.repos); err != nil {
		return req, err
	}
	if req.CoordinatorHosted {
		_, model, err := domain.ResolveAgentLaunch("", req.CoordinatorModel, h.launch.DefaultModel, nil)
		if err != nil {
			return req, refuseSignup(400, "invalid_request", "coordinator_model: %s", err)
		}
		req.coordinator = &domain.HostedCoordinator{Launcher: h.launch.Launcher, Model: model}
	}
	return req, nil
}

func (s *PlatformExperimentsService) joinRegistered(ctx context.Context, platformExpID string, req AddAgentsRequest, signup domain.AgentSignup) ([]domain.AgentSignup, error) {
	if req.Count > 1 || req.Name != "" || req.Kind != "" {
		return nil, refuseSignup(400, "invalid_request", "count, name and kind describe agents to create and cannot be set with agent_id")
	}
	agent, err := s.store.GetAgent(ctx, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	if agent == nil {
		return nil, refuseSignup(404, "agent_not_found", "no agent %s is registered", req.AgentID)
	}
	if signup.Hosted && agent.Kind != domain.AgentKindAgent {
		return nil, refuseSignup(400, "invalid_request", "only an agent of kind \"agent\" can be hosted, and %s is %s", agent.ID, agent.Kind)
	}
	if err := s.Signup(ctx, platformExpID, signup); err != nil {
		return nil, err
	}
	joined, err := s.store.GetAgentSignup(ctx, platformExpID, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	if joined == nil {
		return nil, fmt.Errorf("platform_experiments.AddAgents: agent %s is not signed up after signing up", req.AgentID)
	}
	return []domain.AgentSignup{*joined}, nil
}

func (s *PlatformExperimentsService) spawn(ctx context.Context, platformExpID string, req AddAgentsRequest, signup domain.AgentSignup) ([]domain.AgentSignup, error) {
	count := req.Count
	if count == 0 {
		count = 1
	}
	if count < 0 || count > domain.MaxAgentsPerSpawn {
		return nil, refuseSignup(400, "invalid_request", "count must be 1 to %d, got %d", domain.MaxAgentsPerSpawn, req.Count)
	}
	kind := domain.AgentKind(req.Kind)
	if kind == "" {
		kind = domain.AgentKindAgent
	}
	if !domain.ValidAgentKind(kind) {
		return nil, refuseSignup(400, "invalid_request", "kind must be \"agent\" or \"human\", got %s", req.Kind)
	}
	if signup.Hosted && kind != domain.AgentKindAgent {
		return nil, refuseSignup(400, "invalid_request", "only an agent of kind \"agent\" can be hosted")
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	if pe == nil {
		return nil, refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}

	now := time.Now().UTC()
	agents := make([]*domain.Agent, count)
	signups := make([]domain.AgentSignup, count)
	created := make(map[string]bool, count)
	for i := range agents {
		id, err := domain.NewAgentID()
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
		}
		name := req.Name
		switch {
		case name == "":
			name = id
		case count > 1:
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}
		agents[i] = &domain.Agent{ID: id, Name: name, Kind: kind, PerformanceScore: 0.5, CreatedAt: now}
		signups[i] = signup
		signups[i].AgentID = id
		created[id] = true
	}
	err = s.store.SpawnAgents(ctx, platformExpID, agents, signups)
	switch {
	case errors.Is(err, db.ErrSignupClosed):
		return nil, refuseSignup(400, "signup_closed", "experiment is %s", pe.Status)
	case errors.Is(err, db.ErrMaxAgentsReached):
		return nil, refuseSignup(400, "max_agents_reached", "limit is %d", pe.MaxAgents)
	case err != nil:
		return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	all, err := s.store.ListAgentSignups(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.AddAgents: %w", err)
	}
	joined := make([]domain.AgentSignup, 0, count)
	for _, one := range all {
		if created[one.AgentID] {
			joined = append(joined, one)
		}
	}
	s.logger.Info("agents spawned", zap.String("platformExpID", platformExpID), zap.Int("count", count),
		zap.Bool("hosted", signup.Hosted), zap.String("launcher", signup.Launcher))
	return joined, nil
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
	if h.launch.Launcher == "" || h.launch.DefaultModel == "" || h.launch.SigningSecret == "" || h.repos == nil {
		panic("quota.RegisterAgentsHuma: the handler needs its project repositories and WithAgentLaunch with every field set")
	}
	access := doc.Access

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "add-platform-experiment-agents", Method: "POST", Path: "/platform-experiments/{id}/agents",
		Summary: "Add agents to a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "The only way an agent joins, and only while the experiment is open: quota is divided at start over the agents present. " +
			"Creates count agents with generated ids and signs them up, all of them or none; or, with agent_id, signs up that registered agent. " +
			"hosted=true has the platform start and keep one machine per agent until the experiment closes or the agent is cut; it is refused " +
			"with 409 no_repository while the project has no repository attached, since the agent works in it. " +
			"flavor and model are resolved here and stored: the answer shows what the agent runs. " +
			"hosted=false returns each agent's token: run the agent yourself with it as HL_TOKEN, beside AGENT_ID and PLATFORM_EXPERIMENT_ID. " +
			"The token acts as that agent in that platform experiment and nowhere else, and asking again with agent_id returns it again. " +
			"role decides ranking: \"competitor\" (default) is ranked, cut-eligible and counted against max_agents; \"baseline\" and " +
			"\"reviewer\" are none of the three, though their jobs are billed and gated identically.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body AddAgentsRequest
	}) (*struct{ Body joinedAgentsBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if in.Body.AgentID != "" {
			if err := access.Agent(ctx, in.Body.AgentID); err != nil {
				return nil, err
			}
		}
		signups, err := h.svc.AddAgents(ctx, in.ID, in.Body, h.launch, h.repos)
		if err != nil {
			return nil, h.refusal("add agents", err)
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
