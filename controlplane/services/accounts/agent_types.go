package accounts

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// The reasons a client tells a refused change of an agent type apart by.
const (
	ReasonInvalidAgentType = "invalid_agent_type"
	ReasonBuiltinReadOnly  = "builtin_read_only"
	ReasonVersionConflict  = "version_conflict"
	ReasonAgentTypeInUse   = "agent_type_in_use"
	ReasonArchived         = "archived"
	ReasonAgentTypeLimit   = "agent_type_limit"
	ReasonEditRateLimited  = "edit_rate_limited"
)

// AgentTypes is the persistence of agent types. *db.AgentTypesStore satisfies it.
type AgentTypes interface {
	List(ctx context.Context, orgID string) (domain.AgentTypeCatalog, error)
	Get(ctx context.Context, orgID, id string) (*domain.CurrentAgentType, error)
	Versions(ctx context.Context, orgID, id string) ([]domain.AgentTypeVersion, error)
	Version(ctx context.Context, orgID, id string, n int) (*domain.AgentTypeVersion, error)
	Events(ctx context.Context, orgID, id string) ([]domain.AgentTypeEvent, error)
	Create(ctx context.Context, orgID, name, description string, role domain.AgentRole, prompt string, settings domain.AgentTypeSettings, by domain.Actor) (*domain.CurrentAgentType, error)
	Edit(ctx context.Context, e db.AgentTypeEdit) (*domain.CurrentAgentType, *domain.Propagation, error)
	Restore(ctx context.Context, orgID, id string, version int, by domain.Actor) (*domain.CurrentAgentType, *domain.Propagation, error)
	Archive(ctx context.Context, orgID, id string, by domain.Actor) (*domain.CurrentAgentType, error)
}

// AgentTypesHandler serves an org's agent types: every member reads them, its administrators make
// and change its own, and nobody changes a built-in one.
type AgentTypesHandler struct {
	store  AgentTypes
	logger *zap.Logger
	access *auth.Access
}

// NewAgentTypesHandler serves store.
func NewAgentTypesHandler(store AgentTypes, logger *zap.Logger) *AgentTypesHandler {
	if store == nil || logger == nil {
		panic("accounts.NewAgentTypesHandler: a store and a logger are required")
	}
	return &AgentTypesHandler{store: store, logger: logger}
}

type agentTypeList struct {
	Body []domain.CurrentAgentType
}

type agentTypeAnswer struct {
	Body domain.CurrentAgentType
}

// EditedAgentType is an agent type after an edit or a restore, and whom its new version told.
type EditedAgentType struct {
	domain.CurrentAgentType
	Propagation domain.Propagation `json:"propagation" doc:"The version now current and how many agents and coordinators whose run goes on were told with agent.instructions: each runs it from its next turn boundary. A change that leaves prompt and settings as they were makes no version and tells nobody."`
}

// RestoreAgentTypeBody names the version a restore copies.
type RestoreAgentTypeBody struct {
	Version int `json:"version" minimum:"1" doc:"The version whose prompt and settings the new version holds."`
}

// CreateAgentTypeBody makes an agent type: from scratch, or as a copy of one the org reads.
type CreateAgentTypeBody struct {
	From        string                    `json:"from,omitempty" doc:"Copy this agent type, a built-in one or the org's own: its role, prompt and settings, at its current version. With it, only name may be given."`
	Name        string                    `json:"name,omitempty" doc:"Required without from. With from, the copy's name, \"Copy of\" the source's when left out."`
	Description string                    `json:"description,omitempty" maxLength:"200" doc:"One line on what agents of the type do, at most 200 characters. With from, the copy has the source's."`
	Role        domain.AgentRole          `json:"role,omitempty" enum:"researcher,baseline,coordinator,planner" doc:"Required without from, and fixed once made."`
	Prompt      string                    `json:"prompt,omitempty" doc:"Required without from. It must hold {{rules}} and {{hl_commands}}, and no placeholder its role has no value for."`
	Settings    *domain.AgentTypeSettings `json:"settings,omitempty" doc:"Required without from."`
}

// EditAgentTypeBody is the next version of an agent type.
type EditAgentTypeBody struct {
	IfVersion   int                      `json:"if_version" doc:"The version the edit was made from: 409 version_conflict when it is no longer current."`
	Name        string                   `json:"name" doc:"Its name, which no version holds: changing it alone makes no version."`
	Description *string                  `json:"description,omitempty" maxLength:"200" doc:"Its description, one line of at most 200 characters, which no version holds either. Left out, it stays as it was."`
	Prompt      string                   `json:"prompt"`
	Settings    domain.AgentTypeSettings `json:"settings"`
}

// RegisterAgentTypesHuma registers the agent types of an org on the shared API document.
func RegisterAgentTypesHuma(doc *apidocs.Doc, h *AgentTypesHandler) {
	h.access = doc.Access
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-types", Method: "GET", Path: "/orgs/{id}/agent-types", Tags: []string{"agent-types"},
		Summary: "List the agent types an org's agents may run as",
		Description: "The built-in agent types, read-only for everyone, and the org's own, archived ones included, each with its current version and how many agents and coordinators whose run goes on run it now. " +
			"An agent type is a role (researcher, baseline, coordinator or planner), a system prompt and settings: researchers, pool entries and added agents name one by id as agent_type (flavor on a signup).",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*agentTypeList, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		catalog, err := h.store.List(ctx, in.ID)
		if err != nil {
			return nil, h.answer("list-agent-types", err)
		}
		return &agentTypeList{Body: catalog}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-agent-type", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}", Tags: []string{"agent-types"},
		Summary:     "Read an agent type with its current version",
		Description: "A built-in agent type or one of the org's own. Another org's is not found.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
	}) (*agentTypeAnswer, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		t, err := h.store.Get(ctx, in.ID, in.TypeID)
		if err != nil {
			return nil, h.answer("get-agent-type", err)
		}
		return &agentTypeAnswer{Body: *t}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-type-versions", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/versions", Tags: []string{"agent-types"},
		Summary:     "List every version of an agent type",
		Description: "The newest first. A version is never changed: each edit made the next one.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
	}) (*struct{ Body []domain.AgentTypeVersion }, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		versions, err := h.store.Versions(ctx, in.ID, in.TypeID)
		if err != nil {
			return nil, h.answer("list-agent-type-versions", err)
		}
		return &struct{ Body []domain.AgentTypeVersion }{Body: versions}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-agent-type-version", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/versions/{version}", Tags: []string{"agent-types"},
		Summary: "Read one version of an agent type",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		TypeID  string `path:"type_id"`
		Version int    `path:"version"`
	}) (*struct{ Body domain.AgentTypeVersion }, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		version, err := h.store.Version(ctx, in.ID, in.TypeID, in.Version)
		if err != nil {
			return nil, h.answer("get-agent-type-version", err)
		}
		return &struct{ Body domain.AgentTypeVersion }{Body: *version}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types", Tags: []string{"agent-types"}, DefaultStatus: http.StatusCreated,
		Summary: "Make an agent type of an org",
		Description: "At version 1, from a role, a prompt and settings, or as a copy of an agent type the org reads (from), which is how a built-in one is changed. " +
			"An org makes types of role researcher or baseline: a coordinator or a planner runs only its built-in type, and asking for one, or copying one, is refused with 422 invalid_agent_type. " +
			"Only the org's administrators make one: a member is refused with 403. 422 invalid_agent_type for a prompt or settings an agent could not run with; " +
			"409 agent_type_limit once the org has 100 types that are not archived.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body CreateAgentTypeBody
	}) (*agentTypeAnswer, error) {
		caller, err := h.access.OrgAdmin(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		name, description, role, prompt, settings, err := h.newType(ctx, in.ID, in.Body)
		if err != nil {
			return nil, h.answer("create-agent-type", err)
		}
		created, err := h.store.Create(ctx, in.ID, name, description, role, prompt, settings, caller.Actor())
		if err != nil {
			return nil, h.answer("create-agent-type", err)
		}
		return &agentTypeAnswer{Body: *created}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "edit-agent-type", Method: "PUT", Path: "/orgs/{id}/agent-types/{type_id}", Tags: []string{"agent-types"},
		Summary: "Make the next version of an org's agent type",
		Description: "Inserts the next version and makes it current, which every agent and coordinator of the type runs, and in the same write tells each whose run goes on with agent.instructions; " +
			"its harness applies it at its next turn boundary: a new prompt, model or the effort max in a new session, the rest in the session it has. An agent given no model or hyperparameters of its own, and a pool entry naming none, run the type's from then on. " +
			"Only the org's administrators edit: a member is refused with 403, and so is anyone editing a built-in type (builtin_read_only), which only the platform's repository changes. " +
			"409 version_conflict when if_version is not current, archived for an archived type; 422 invalid_agent_type for a prompt or settings an agent could not run with; " +
			"429 edit_rate_limited once 60 versions of the type were made in the last hour.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
		Body   EditAgentTypeBody
	}) (*struct{ Body EditedAgentType }, error) {
		caller, err := h.access.OrgAdmin(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		current, err := h.store.Get(ctx, in.ID, in.TypeID)
		if err != nil {
			return nil, h.answer("edit-agent-type", err)
		}
		name := strings.TrimSpace(in.Body.Name)
		description := current.Description
		if in.Body.Description != nil {
			description = *in.Body.Description
		}
		if err := checkAgentType(current.Role, name, description, in.Body.Prompt, &in.Body.Settings); err != nil {
			return nil, err
		}
		edited, propagation, err := h.store.Edit(ctx, db.AgentTypeEdit{
			OrgID: in.ID, ID: in.TypeID, IfVersion: in.Body.IfVersion, Name: name, Description: description, Prompt: in.Body.Prompt, Settings: in.Body.Settings, By: caller.Actor(),
		})
		if err != nil {
			return nil, h.answer("edit-agent-type", err)
		}
		return &struct{ Body EditedAgentType }{Body: EditedAgentType{CurrentAgentType: *edited, Propagation: *propagation}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "restore-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types/{type_id}/restore", Tags: []string{"agent-types"},
		Summary: "Make an earlier version of an org's agent type current again",
		Description: "Inserts the next version with the prompt and settings of version, makes it current and tells every agent and coordinator of the type whose run goes on with agent.instructions, as an edit does. " +
			"Restoring what the current version holds makes no version. Name and description stay as they are. " +
			"Only the org's administrators restore, and nobody a built-in type (builtin_read_only). 404 for a version the type does not have, 409 archived for an archived type, 429 edit_rate_limited as for an edit.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
		Body   RestoreAgentTypeBody
	}) (*struct{ Body EditedAgentType }, error) {
		caller, err := h.access.OrgAdmin(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		restored, propagation, err := h.store.Restore(ctx, in.ID, in.TypeID, in.Body.Version, caller.Actor())
		if err != nil {
			return nil, h.answer("restore-agent-type", err)
		}
		return &struct{ Body EditedAgentType }{Body: EditedAgentType{CurrentAgentType: *restored, Propagation: *propagation}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agent-type-events", Method: "GET", Path: "/orgs/{id}/agent-types/{type_id}/events", Tags: []string{"agent-types"},
		Summary:     "List every change of an agent type",
		Description: "The newest first: who created, edited, renamed, described, archived or restored it, when, and the version it was at after. Nothing is ever removed from it.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
	}) (*struct{ Body []domain.AgentTypeEvent }, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		events, err := h.store.Events(ctx, in.ID, in.TypeID)
		if err != nil {
			return nil, h.answer("list-agent-type-events", err)
		}
		return &struct{ Body []domain.AgentTypeEvent }{Body: events}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "archive-agent-type", Method: "POST", Path: "/orgs/{id}/agent-types/{type_id}/archive", Tags: []string{"agent-types"},
		Summary: "Archive an org's agent type",
		Description: "An archived type takes no new agent and is no longer edited; nothing is deleted. " +
			"409 agent_type_in_use while a platform experiment that is not closed names it, in its researchers, for its coordinator or for one of its agents. " +
			"Only the org's administrators archive, and nobody a built-in type.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		TypeID string `path:"type_id"`
	}) (*agentTypeAnswer, error) {
		caller, err := h.access.OrgAdmin(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		archived, err := h.store.Archive(ctx, in.ID, in.TypeID, caller.Actor())
		if err != nil {
			return nil, h.answer("archive-agent-type", err)
		}
		return &agentTypeAnswer{Body: *archived}, nil
	})
}

// newType is what a create makes: body's own, or a copy of the type it names in from.
func (h *AgentTypesHandler) newType(ctx context.Context, orgID string, body CreateAgentTypeBody) (string, string, domain.AgentRole, string, domain.AgentTypeSettings, error) {
	name := strings.TrimSpace(body.Name)
	if body.From == "" {
		role, err := domain.ParseAgentRole(string(body.Role))
		if err == nil {
			err = domain.CheckOrgAgentRole(role)
		}
		if err != nil {
			return "", "", "", "", domain.AgentTypeSettings{}, invalid(ReasonInvalidAgentType, err.Error())
		}
		if body.Settings == nil {
			return "", "", "", "", domain.AgentTypeSettings{}, invalid(ReasonInvalidAgentType, "settings are required without from")
		}
		settings := *body.Settings
		return name, body.Description, role, body.Prompt, settings, checkAgentType(role, name, body.Description, body.Prompt, &settings)
	}
	if body.Description != "" || body.Role != "" || body.Prompt != "" || body.Settings != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, invalid(ReasonInvalidAgentType, "a copy (from) takes its role, prompt and settings from the type it copies: give only name, and edit the copy")
	}
	source, err := h.store.Get(ctx, orgID, body.From)
	if err != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, err
	}
	if err := domain.CheckOrgAgentRole(source.Role); err != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, invalid(ReasonInvalidAgentType, err.Error())
	}
	if name == "" {
		name = "Copy of " + source.Name
	}
	settings := source.Current.Settings
	return name, source.Description, source.Role, source.Current.Prompt, settings, checkAgentType(source.Role, name, source.Description, source.Current.Prompt, &settings)
}

// checkAgentType refuses a name, description, prompt or settings an agent of role could not run with.
func checkAgentType(role domain.AgentRole, name, description, prompt string, settings *domain.AgentTypeSettings) error {
	if settings.Hyperparameters == nil {
		settings.Hyperparameters = map[string]any{}
	}
	for _, err := range []error{domain.ValidateAgentTypeName(name), domain.ValidateAgentTypeDescription(description), domain.ValidatePrompt(role, prompt), settings.Validate()} {
		if err != nil {
			return invalid(ReasonInvalidAgentType, err.Error())
		}
	}
	return nil
}

// answer turns what the store refuses into what the caller is told, and anything else into the
// platform failing.
func (h *AgentTypesHandler) answer(op string, err error) error {
	var answered huma.StatusError
	var conflictErr *db.AgentTypeVersionConflict
	var inUse *db.AgentTypeInUse
	var limit *db.AgentTypeLimit
	switch {
	case err == nil:
		return nil
	case errors.As(err, &answered):
		return err
	case errors.Is(err, db.ErrAgentTypeNotFound):
		return notFound("agent type")
	case errors.Is(err, db.ErrAgentTypeBuiltin):
		return refusal.Refuse(http.StatusForbidden, ReasonBuiltinReadOnly, "a built-in agent type is read-only for everyone: copy it (POST /orgs/{id}/agent-types with from) and change the copy")
	case errors.Is(err, db.ErrAgentTypeArchived):
		return conflict(ReasonArchived, "the agent type is archived and no longer edited")
	case errors.As(err, &conflictErr):
		return conflict(ReasonVersionConflict, conflictErr.Error())
	case errors.As(err, &inUse):
		return conflict(ReasonAgentTypeInUse, inUse.Error()+": archive it once they closed or stopped using it")
	case errors.As(err, &limit) && limit.Types:
		return conflict(ReasonAgentTypeLimit, limit.Error())
	case errors.As(err, &limit):
		return refusal.Refuse(http.StatusTooManyRequests, ReasonEditRateLimited, limit.Error())
	default:
		return auth.Internal(h.logger, "agent types request failed", err, zap.String("operation", op))
	}
}
