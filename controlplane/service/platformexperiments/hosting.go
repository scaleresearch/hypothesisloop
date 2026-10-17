package platformexperiments

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// reasonHostingNotConfigured is why hosting is unavailable on a deployment with no sandbox
// provider in its settings.
const reasonHostingNotConfigured = "hosting_not_configured"

// Hosting is whether the platform can host an agent: GET /hosting, and what every request that
// hosts one is refused with while it cannot.
type Hosting struct {
	Available   bool                 `json:"available"`
	Unavailable *domain.StatusReason `json:"unavailable,omitempty" doc:"Why it cannot, while available is false: reason hosting_not_configured."`
}

// hostingOf is Hosting on a deployment that has a sandbox provider or not.
func hostingOf(configured bool) Hosting {
	if configured {
		return Hosting{Available: true}
	}
	return Hosting{Unavailable: &domain.StatusReason{
		Reason:         reasonHostingNotConfigured,
		Message:        "this deployment has no sandbox provider to run hosted machines on",
		Summary:        "Hosted agents are not available on this platform.",
		OperatorDetail: "The settings have no sandbox section (on a dev stack: make dev-hosted).",
	}}
}

// requireHosting refuses to host anything on a deployment that cannot: nothing would ever start it.
func requireHosting(configured bool) error {
	why := hostingOf(configured).Unavailable
	if why == nil {
		return nil
	}
	refused := refusal.Refuse(http.StatusServiceUnavailable, why.Reason, why.Reason+": "+why.Message)
	refused.Summary, refused.OperatorDetail = why.Summary, why.OperatorDetail
	return refused
}

// withHosting resolves the coordinator a create asks the platform to run with launch: the model
// it asked for, empty for its agent type's, and that the experiment names the repository it works
// in.
func withHosting(req CreatePlatformExperimentRequest, launch AgentLaunch) (CreatePlatformExperimentRequest, error) {
	if req.CoordinatorModel != "" && !req.CoordinatorHosted {
		return req, refuseSignup(400, "invalid_request", "coordinator_model is set and coordinator_hosted is not")
	}
	req.maxHosted = launch.MaxHosted
	if !req.CoordinatorHosted {
		return req, nil
	}
	if launch.CoordinatorModel == "" {
		return req, errors.New("platform_experiments.Create: the handler has no agent launch settings")
	}
	if err := requireHosting(launch.Hosting); err != nil {
		return req, err
	}
	if req.CoordinatorModel != "" {
		if err := domain.ValidateModel(req.CoordinatorModel); err != nil {
			return req, refuseSignup(400, "invalid_request", "coordinator_model: %s", err)
		}
	}
	// Checked before anything is made for the experiment: its repository is created ahead of
	// the write that would refuse this.
	if launch.MaxHosted < 2 {
		return req, refuseSignup(409, "max_hosted_reached", "%s", maxHostedReached(launch.MaxHosted))
	}
	req.coordinator = &domain.HostedCoordinator{Model: req.CoordinatorModel}
	return req, nil
}

// hostedAgents is the agents a platform experiment whose coordinator the platform hosts is given
// in the write that makes it open, each run by the platform and added by by: one for each
// researcher asked for, and after them the one that measures the baseline, whose machine is asked
// for at once beside the coordinator's, so that nothing waits for anyone to add it. An accepted
// draft and a create with coordinator_hosted both get theirs here.
func hostedAgents(catalog domain.AgentTypeCatalog, researchers []ProposedAgent, by domain.Actor, now time.Time) ([]*domain.Agent, []domain.AgentSignup, error) {
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
			AgentID: agentID, Role: role, Hosted: true,
			AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: choice.Hyperparameters, Actor: &by,
		})
	}
	return agents, signups, nil
}

// Hosting says whether the platform runs agents itself.
func (s *Service) Hosting() Hosting {
	return hostingOf(s.launch.Hosting)
}
