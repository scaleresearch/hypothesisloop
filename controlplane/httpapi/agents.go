package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

type agentsAPI struct {
	agents *accounts.Agents
	access *auth.Access
}

// RegisterAgents registers the registry of agents whose standing carries from one platform
// experiment to the next.
func RegisterAgents(doc *Doc, agents *accounts.Agents) {
	a := agentsAPI{agents: agents, access: doc.Access}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "register-agent", Method: "POST", Path: "/agents",
		Summary: "Register an agent", Tags: []string{"agents"},
		DefaultStatus: 201,
		Description:   "Registers an agent under an id of your choosing, for one whose standing should carry from one platform experiment to the next. 409 agent_exists if the id already exists. It joins nothing: POST /platform-experiments/{id}/agents with agent_id does, and that call alone creates agents that need no identity of their own.",
	}, a.register)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-agents", Method: "GET", Path: "/agents",
		Summary: "List registered agents", Tags: []string{"agents"},
		Description: "Filter with ?platform_experiment_id (the agents signed up to it) and ?q (search id/name). " +
			"Optional ?limit (default 20, max 200, 422 invalid_page over it) and ?offset, newest first. Total match count is " +
			"returned in the X-Total-Count response header.",
	}, a.list)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "agent-stats", Method: "GET", Path: "/agents/stats",
		Summary: "Count registered agents and those with a top-3 placement", Tags: []string{"agents"},
		Description: "Whole-set figures for the same ?platform_experiment_id and ?q filters GET /agents accepts, " +
			"aggregated in PostgreSQL. A list read is one page, so these cannot be tallied from it.",
	}, a.stats)

}

func (a agentsAPI) register(ctx context.Context, in *struct {
	Body struct {
		ID    string `json:"id" doc:"Stable unique agent id"`
		Name  string `json:"name" minLength:"1" doc:"The name the agent is shown under and its branches carry."`
		OrgID string `json:"org_id,omitempty" required:"false" doc:"The org the agent belongs to. Needed only from a token that is not bound to a single org."`
	}
}) (*struct{ Body *domain.Agent }, error) {
	// Validated here rather than only where it is used: an agent id is pasted verbatim into
	// Kubernetes object names and into the object-store key policy that decides what a
	// job may write. Registration is the one moment it can be refused for free.
	if err := domain.ValidateIdentifier("id", in.Body.ID); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	orgID, err := a.access.Org(ctx, in.Body.OrgID)
	if err != nil {
		return nil, err
	}
	agent, err := a.agents.Register(ctx, in.Body.ID, in.Body.Name, orgID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *domain.Agent }{Body: agent}, nil
}

func (a agentsAPI) list(ctx context.Context, in *struct {
	PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
	Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
	ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
	Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200: a larger one is refused with 422 invalid_page. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
	Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
}) (*struct {
	Body       []*domain.Agent
	TotalCount int `header:"X-Total-Count"`
}, error) {
	scope, err := a.access.ListScope(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	agents, total, err := a.agents.List(ctx, in.PlatformExperimentID, in.Search, scope, in.Limit, in.Offset)
	if err != nil {
		return nil, err
	}
	return &struct {
		Body       []*domain.Agent
		TotalCount int `header:"X-Total-Count"`
	}{Body: agents, TotalCount: total}, nil
}

func (a agentsAPI) stats(ctx context.Context, in *struct {
	PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
	Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
	ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
}) (*struct{ Body *domain.AgentStats }, error) {
	scope, err := a.access.ListScope(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	stats, err := a.agents.Stats(ctx, in.PlatformExperimentID, in.Search, scope)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *domain.AgentStats }{Body: stats}, nil
}
