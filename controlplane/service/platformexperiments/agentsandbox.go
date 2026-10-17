package platformexperiments

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// reasonAgentSandboxNotConfigured is why agent sandboxes are unavailable on a deployment with no sandbox
// provider in its settings.
const reasonAgentSandboxNotConfigured = "agentsandbox_not_configured"

// AgentSandbox is whether the platform can run an agent in an agent sandbox: GET /agentsandbox, and what every request that
// hosts one is refused with while it cannot.
type AgentSandbox struct {
	Available   bool                 `json:"available"`
	Unavailable *domain.StatusReason `json:"unavailable,omitempty" doc:"Why it cannot, while available is false: reason agentsandbox_not_configured."`
}

// agentSandboxOf is AgentSandbox on a deployment that has a sandbox provider or not.
func agentSandboxOf(configured bool) AgentSandbox {
	if configured {
		return AgentSandbox{Available: true}
	}
	return AgentSandbox{Unavailable: &domain.StatusReason{
		Reason:         reasonAgentSandboxNotConfigured,
		Message:        "this deployment has no sandbox provider to run agent sandboxes on",
		Summary:        "Sandboxed agents are not available on this platform.",
		OperatorDetail: "The settings have no sandbox section.",
	}}
}

// requireAgentSandbox refuses to host anything on a deployment that cannot: nothing would ever start it.
func requireAgentSandbox(configured bool) error {
	why := agentSandboxOf(configured).Unavailable
	if why == nil {
		return nil
	}
	return refusal.Refuse(http.StatusServiceUnavailable, why.Reason, why.Summary)
}

// withAgentSandbox resolves the coordinator a create asks the platform to run with launch: the model
// it asked for, empty for its agent type's, and that the experiment names the repository it works
// in.
func withAgentSandbox(req CreatePlatformExperimentRequest, launch AgentLaunch) (CreatePlatformExperimentRequest, error) {
	if req.CoordinatorModel != "" && !req.CoordinatorInAgentSandbox {
		return req, refuseSignup(400, "invalid_request", "coordinator_model is set and coordinator_in_agentsandbox is not")
	}
	req.maxAgentSandboxes = launch.MaxAgentSandboxes
	if !req.CoordinatorInAgentSandbox {
		return req, nil
	}
	if launch.CoordinatorModel == "" {
		return req, errors.New("platform_experiments.Create: the handler has no agent launch settings")
	}
	if err := requireAgentSandbox(launch.AgentSandbox); err != nil {
		return req, err
	}
	if req.CoordinatorModel != "" {
		if err := domain.ValidateModel(req.CoordinatorModel); err != nil {
			return req, refuseSignup(400, "invalid_request", "coordinator_model: %s", err)
		}
	}
	// Checked before anything is made for the experiment: its repository is created ahead of
	// the write that would refuse this.
	if launch.MaxAgentSandboxes < 2 {
		return req, refuseSignup(409, "max_agentsandboxes_reached", "%s", maxAgentSandboxesReached(launch.MaxAgentSandboxes))
	}
	req.coordinator = &domain.AgentSandboxCoordinator{Model: req.CoordinatorModel}
	return req, nil
}

// sandboxedAgents is the agents a platform experiment whose coordinator the platform hosts is given
// in the write that makes it open, each run by the platform and added by by: one for each
// researcher asked for, and after them the one that measures the baseline, whose machine is asked
// for at once beside the coordinator's, so that nothing waits for anyone to add it. An accepted
// draft and a create with coordinator_in_agentsandbox both get theirs here.
func sandboxedAgents(catalog domain.AgentTypeCatalog, researchers []ProposedAgent, by domain.Actor, now time.Time) ([]*domain.Agent, []domain.AgentSignup, error) {
	agents := make([]*domain.Agent, 0, len(researchers)+1)
	signups := make([]domain.AgentSignup, 0, len(researchers)+1)
	for i, asked := range append(slices.Clone(researchers), ProposedAgent{}) {
		role := domain.SignupRoleCompetitor
		if i == len(researchers) {
			role = domain.SignupRoleBaseline
		}
		choice, err := domain.ResolveAgentLaunch(catalog, domain.RoleOfSignup(role), asked.AgentType, asked.Model, asked.Hyperparameters)
		if err != nil {
			return nil, nil, refusedProposal("invalid_agents", "agents[%d]: %s", i, err)
		}
		agentID, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			return nil, nil, err
		}
		agents = append(agents, &domain.Agent{ID: agentID, CreatedAt: now})
		signups = append(signups, domain.AgentSignup{
			AgentID: agentID, Role: role, InAgentSandbox: true,
			AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: choice.Hyperparameters, Actor: &by,
		})
	}
	return agents, signups, nil
}

// AgentSandbox says whether the platform runs agents itself.
func (s *Service) AgentSandbox() AgentSandbox {
	return agentSandboxOf(s.launch.AgentSandbox)
}
