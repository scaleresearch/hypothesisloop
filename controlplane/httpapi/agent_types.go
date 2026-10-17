package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

type agentTypeList struct {
	Body []domain.CurrentAgentType
}

type agentTypeAnswer struct {
	Body domain.CurrentAgentType
}

type agentTypesAPI struct {
	types *accounts.AgentTypes
}

// registerAgentTypes registers the agent types of an org on the shared API document.
func registerAgentTypes(doc *Doc, types *accounts.AgentTypes) {
	a := agentTypesAPI{types: types}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-types", Method: "GET", Path: "/orgs/{id}/agent-types", Tags: []string{"agent-types"},
		Summary: "List the agent types an org's agents may run as",
		Description: "The built-in agent types, read-only for everyone, and the org's own, archived ones included, each with its current version and how many agents and coordinators whose run goes on run it now. " +
			"An agent type is a role (researcher, baseline, coordinator or planner), a system prompt and settings: researchers, pool entries and added agents name one by id as agent_type.",
	}, a.list)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-agent-type", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}", Tags: []string{"agent-types"},
		Summary:     "Read an agent type with its current version",
		Description: "A built-in agent type or one of the org's own. Another org's is not found.",
	}, a.get)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-type-versions", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/versions", Tags: []string{"agent-types"},
		Summary:     "List every version of an agent type",
		Description: "The newest first. A version is never changed: each edit made the next one.",
	}, a.versions)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-agent-type-version", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/versions/{version}", Tags: []string{"agent-types"},
		Summary: "Read one version of an agent type",
	}, a.version)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types", Tags: []string{"agent-types"}, DefaultStatus: http.StatusCreated,
		Summary: "Make an agent type of an org",
		Description: "At version 1, from a role, a prompt and settings, or as a copy of an agent type the org reads (from), which is how a built-in one is changed. " +
			"An org makes types of role researcher or baseline: a coordinator or a planner runs only its built-in type, and asking for one, or copying one, is refused with 422 invalid_agent_type. " +
			"Only the org's administrators make one: a member is refused with 403. 422 invalid_agent_type for a prompt or settings an agent could not run with; " +
			"409 agent_type_limit once the org has 100 types that are not archived.",
	}, a.create)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "edit-agent-type", Method: "PUT", Path: "/orgs/{id}/agent-types/{type_id}", Tags: []string{"agent-types"},
		Summary: "Make the next version of an org's agent type",
		Description: "Inserts the next version and makes it current, which every agent and coordinator of the type runs, and in the same write tells each whose run goes on with agent.instructions; " +
			"its harness applies it at its next turn boundary: a new prompt, model or the effort max in a new session, the rest in the session it has. An agent given no model or hyperparameters of its own, and a pool entry naming none, run the type's from then on. " +
			"Only the org's administrators edit: a member is refused with 403, and so is anyone editing a built-in type (builtin_read_only), which only the platform's repository changes. " +
			"409 version_conflict when if_version is not current, archived for an archived type; 422 invalid_agent_type for a prompt or settings an agent could not run with; " +
			"429 edit_rate_limited once 60 versions of the type were made in the last hour.",
	}, a.edit)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "restore-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types/{type_id}/restore", Tags: []string{"agent-types"},
		Summary: "Make an earlier version of an org's agent type current again",
		Description: "Inserts the next version with the prompt and settings of version, makes it current and tells every agent and coordinator of the type whose run goes on with agent.instructions, as an edit does. " +
			"Restoring what the current version holds makes no version. Name and description stay as they are. " +
			"Only the org's administrators restore, and nobody a built-in type (builtin_read_only). 404 for a version the type does not have, 409 archived for an archived type, 429 edit_rate_limited as for an edit.",
	}, a.restore)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-type-events", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/events", Tags: []string{"agent-types"},
		Summary:     "List every change of an agent type",
		Description: "The newest first: who created, edited, renamed, described, archived or restored it, when, and the version it was at after. Nothing is ever removed from it.",
	}, a.events)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "archive-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types/{type_id}/archive", Tags: []string{"agent-types"},
		Summary: "Archive an org's agent type",
		Description: "An archived type takes no new agent and is no longer edited; nothing is deleted. " +
			"409 agent_type_in_use while a platform experiment that is not closed names it, in its researchers, for its coordinator or for one of its agents. " +
			"Only the org's administrators archive, and nobody a built-in type.",
	}, a.archive)
}

func (a agentTypesAPI) list(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*agentTypeList, error) {
	catalog, err := a.types.List(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &agentTypeList{Body: catalog}, nil
}

func (a agentTypesAPI) get(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
}) (*agentTypeAnswer, error) {
	t, err := a.types.Get(ctx, in.ID, in.TypeID)
	if err != nil {
		return nil, err
	}
	return &agentTypeAnswer{Body: *t}, nil
}

func (a agentTypesAPI) versions(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
}) (*struct{ Body []domain.AgentTypeVersion }, error) {
	versions, err := a.types.Versions(ctx, in.ID, in.TypeID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body []domain.AgentTypeVersion }{Body: versions}, nil
}

func (a agentTypesAPI) version(ctx context.Context, in *struct {
	ID      string `path:"id"`
	TypeID  string `path:"type_id"`
	Version int    `path:"version"`
}) (*struct{ Body domain.AgentTypeVersion }, error) {
	version, err := a.types.Version(ctx, in.ID, in.TypeID, in.Version)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.AgentTypeVersion }{Body: *version}, nil
}

func (a agentTypesAPI) create(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body accounts.CreateAgentTypeRequest
}) (*agentTypeAnswer, error) {
	created, err := a.types.Create(ctx, in.ID, in.Body)
	if err != nil {
		return nil, err
	}
	return &agentTypeAnswer{Body: *created}, nil
}

func (a agentTypesAPI) edit(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
	Body   accounts.EditAgentTypeRequest
}) (*struct{ Body accounts.EditedAgentType }, error) {
	edited, err := a.types.Edit(ctx, in.ID, in.TypeID, in.Body)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.EditedAgentType }{Body: *edited}, nil
}

func (a agentTypesAPI) restore(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
	Body   accounts.RestoreAgentTypeRequest
}) (*struct{ Body accounts.EditedAgentType }, error) {
	restored, err := a.types.Restore(ctx, in.ID, in.TypeID, in.Body.Version)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.EditedAgentType }{Body: *restored}, nil
}

func (a agentTypesAPI) events(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
}) (*struct{ Body []domain.AgentTypeEvent }, error) {
	events, err := a.types.Events(ctx, in.ID, in.TypeID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body []domain.AgentTypeEvent }{Body: events}, nil
}

func (a agentTypesAPI) archive(ctx context.Context, in *struct {
	ID     string `path:"id"`
	TypeID string `path:"type_id"`
}) (*agentTypeAnswer, error) {
	archived, err := a.types.Archive(ctx, in.ID, in.TypeID)
	if err != nil {
		return nil, err
	}
	return &agentTypeAnswer{Body: *archived}, nil
}
