package accounts

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
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
	ReasonEditRateLimited  = "rate_limited"
)

// AgentTypeStore is the persistence of agent types. *db.Store satisfies it.
type AgentTypeStore interface {
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

// AgentTypes serves an org's agent types: every member reads them, its administrators make and
// change its own, and nobody changes a built-in one.
type AgentTypes struct {
	store  AgentTypeStore
	logger *zap.Logger
	access *auth.Access
}

// NewAgentTypes serves store.
func NewAgentTypes(store AgentTypeStore, access *auth.Access, logger *zap.Logger) *AgentTypes {
	if store == nil || logger == nil {
		panic("accounts.NewAgentTypes: a store and a logger are required")
	}
	return &AgentTypes{store: store, logger: logger, access: access}
}

// ListedAgentType is an agent type with what the caller may do to it.
type ListedAgentType struct {
	domain.CurrentAgentType
	Actions []domain.AgentTypeAction `json:"actions" doc:"What the caller may do to the agent type now, each a write the API accepts from them: edit, archive, duplicate (create one with from) and restore. Archive is listed while a platform experiment still names the type, and refused then as agent_type_in_use."`
}

// agentTypeActions is what caller, holding role in the org, may do to t by the rules the writes
// refuse by: the org's administrators copy a type of a role an org makes, and edit, archive and
// restore the org's own until it is archived. Archiving one archived changes nothing, so it is not
// offered.
func agentTypeActions(caller auth.Principal, role domain.OrgRole, t domain.CurrentAgentType) []domain.AgentTypeAction {
	actions := []domain.AgentTypeAction{}
	if auth.Administers(caller, role) != nil {
		return actions
	}
	if !t.Builtin && t.ArchivedAt == nil {
		actions = append(actions, domain.AgentTypeActionEdit, domain.AgentTypeActionArchive, domain.AgentTypeActionRestore)
	}
	if domain.CheckOrgAgentRole(t.Role) == nil {
		actions = append(actions, domain.AgentTypeActionDuplicate)
	}
	return actions
}

// listed is types as caller reads them in orgID.
func (h *AgentTypes) listed(ctx context.Context, caller auth.Principal, orgID string, types ...domain.CurrentAgentType) ([]ListedAgentType, error) {
	role, err := h.access.Role(ctx, caller, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]ListedAgentType, len(types))
	for i, t := range types {
		out[i] = ListedAgentType{CurrentAgentType: t, Actions: agentTypeActions(caller, role, t)}
	}
	return out, nil
}

// one is t as caller reads it in orgID.
func (h *AgentTypes) one(ctx context.Context, caller auth.Principal, orgID string, t *domain.CurrentAgentType) (*ListedAgentType, error) {
	listed, err := h.listed(ctx, caller, orgID, *t)
	if err != nil {
		return nil, err
	}
	return &listed[0], nil
}

// EditedAgentType is an agent type after an edit or a restore, and whom its new version told.
type EditedAgentType struct {
	ListedAgentType
	Propagation domain.Propagation `json:"propagation" doc:"The version now current and how many agents and coordinators whose run goes on were told with agent.instructions: each runs it from its next turn boundary. A change that leaves prompt and settings as they were makes no version and tells nobody."`
}

// RestoreAgentTypeRequest names the version a restore copies.
type RestoreAgentTypeRequest struct {
	Version int `json:"version" minimum:"1" doc:"The version whose prompt and settings the new version holds."`
}

// CreateAgentTypeRequest makes an agent type: from scratch, or as a copy of one the org reads.
type CreateAgentTypeRequest struct {
	From        string                    `json:"from,omitempty" doc:"Copy this agent type, a built-in one or the org's own: its role, prompt and settings, at its current version. With it, only name may be given."`
	Name        string                    `json:"name,omitempty" doc:"Required without from. With from, the copy's name, \"Copy of\" the source's when left out."`
	Description string                    `json:"description,omitempty" maxLength:"200" doc:"One line on what agents of the type do, at most 200 characters. With from, the copy has the source's."`
	Role        domain.AgentRole          `json:"role,omitempty" doc:"Required without from, and fixed once made."`
	Prompt      string                    `json:"prompt,omitempty" doc:"Required without from. It must hold {{rules}} and {{hl_commands}}, and no placeholder its role has no value for."`
	Settings    *domain.AgentTypeSettings `json:"settings,omitempty" doc:"Required without from."`
}

// EditAgentTypeRequest is the next version of an agent type.
type EditAgentTypeRequest struct {
	IfVersion   int                      `json:"if_version" doc:"The version the edit was made from: 409 version_conflict when it is no longer current."`
	Name        string                   `json:"name" doc:"Its name, which no version holds: changing it alone makes no version."`
	Description *string                  `json:"description,omitempty" maxLength:"200" doc:"Its description, one line of at most 200 characters, which no version holds either. Left out, it stays as it was."`
	Prompt      string                   `json:"prompt"`
	Settings    domain.AgentTypeSettings `json:"settings"`
}

// List is the agent types orgID's agents may run as.
func (h *AgentTypes) List(ctx context.Context, orgID string) ([]ListedAgentType, error) {
	caller, err := h.access.OrgMember(ctx, orgID)
	if err != nil {
		return nil, err
	}
	catalog, err := h.store.List(ctx, orgID)
	if err != nil {
		return nil, h.answer("list-agent-types", err)
	}
	return h.listed(ctx, caller, orgID, catalog...)
}

func (h *AgentTypes) Get(ctx context.Context, orgID, id string) (*ListedAgentType, error) {
	caller, err := h.access.OrgMember(ctx, orgID)
	if err != nil {
		return nil, err
	}
	t, err := h.store.Get(ctx, orgID, id)
	if err != nil {
		return nil, h.answer("get-agent-type", err)
	}
	return h.one(ctx, caller, orgID, t)
}

func (h *AgentTypes) Versions(ctx context.Context, orgID, id string) ([]domain.AgentTypeVersion, error) {
	if _, err := h.access.OrgMember(ctx, orgID); err != nil {
		return nil, err
	}
	versions, err := h.store.Versions(ctx, orgID, id)
	if err != nil {
		return nil, h.answer("list-agent-type-versions", err)
	}
	return versions, nil
}

func (h *AgentTypes) Version(ctx context.Context, orgID, id string, n int) (*domain.AgentTypeVersion, error) {
	if _, err := h.access.OrgMember(ctx, orgID); err != nil {
		return nil, err
	}
	version, err := h.store.Version(ctx, orgID, id, n)
	if err != nil {
		return nil, h.answer("get-agent-type-version", err)
	}
	return version, nil
}

func (h *AgentTypes) Create(ctx context.Context, orgID string, body CreateAgentTypeRequest) (*ListedAgentType, error) {
	caller, err := h.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	name, description, role, prompt, settings, err := h.newType(ctx, orgID, body)
	if err != nil {
		return nil, h.answer("create-agent-type", err)
	}
	created, err := h.store.Create(ctx, orgID, name, description, role, prompt, settings, caller.Actor())
	if err != nil {
		return nil, h.answer("create-agent-type", err)
	}
	return h.one(ctx, caller, orgID, created)
}

func (h *AgentTypes) Edit(ctx context.Context, orgID, id string, body EditAgentTypeRequest) (*EditedAgentType, error) {
	caller, err := h.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	current, err := h.store.Get(ctx, orgID, id)
	if err != nil {
		return nil, h.answer("edit-agent-type", err)
	}
	name := strings.TrimSpace(body.Name)
	description := current.Description
	if body.Description != nil {
		description = *body.Description
	}
	if err := checkAgentType(current.Role, name, description, body.Prompt, &body.Settings); err != nil {
		return nil, err
	}
	edited, propagation, err := h.store.Edit(ctx, db.AgentTypeEdit{
		OrgID: orgID, ID: id, IfVersion: body.IfVersion, Name: name, Description: description, Prompt: body.Prompt, Settings: body.Settings, By: caller.Actor(),
	})
	if err != nil {
		return nil, h.answer("edit-agent-type", err)
	}
	listed, err := h.one(ctx, caller, orgID, edited)
	if err != nil {
		return nil, err
	}
	return &EditedAgentType{ListedAgentType: *listed, Propagation: *propagation}, nil
}

func (h *AgentTypes) Restore(ctx context.Context, orgID, id string, version int) (*EditedAgentType, error) {
	caller, err := h.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	restored, propagation, err := h.store.Restore(ctx, orgID, id, version, caller.Actor())
	if err != nil {
		return nil, h.answer("restore-agent-type", err)
	}
	listed, err := h.one(ctx, caller, orgID, restored)
	if err != nil {
		return nil, err
	}
	return &EditedAgentType{ListedAgentType: *listed, Propagation: *propagation}, nil
}

func (h *AgentTypes) Events(ctx context.Context, orgID, id string) ([]domain.AgentTypeEvent, error) {
	if _, err := h.access.OrgMember(ctx, orgID); err != nil {
		return nil, err
	}
	events, err := h.store.Events(ctx, orgID, id)
	if err != nil {
		return nil, h.answer("list-agent-type-events", err)
	}
	return events, nil
}

func (h *AgentTypes) Archive(ctx context.Context, orgID, id string) (*ListedAgentType, error) {
	caller, err := h.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	archived, err := h.store.Archive(ctx, orgID, id, caller.Actor())
	if err != nil {
		return nil, h.answer("archive-agent-type", err)
	}
	return h.one(ctx, caller, orgID, archived)
}

// newType is what a create makes: body's own, or a copy of the type it names in from.
func (h *AgentTypes) newType(ctx context.Context, orgID string, body CreateAgentTypeRequest) (string, string, domain.AgentRole, string, domain.AgentTypeSettings, error) {
	name := strings.TrimSpace(body.Name)
	if body.From == "" {
		role, err := domain.ParseAgentRole(string(body.Role))
		if err == nil {
			err = domain.CheckOrgAgentRole(role)
		}
		if err != nil {
			return "", "", "", "", domain.AgentTypeSettings{}, refusal.Invalid(ReasonInvalidAgentType, err.Error())
		}
		if body.Settings == nil {
			return "", "", "", "", domain.AgentTypeSettings{}, refusal.Invalid(ReasonInvalidAgentType, "settings are required without from")
		}
		settings := *body.Settings
		return name, body.Description, role, body.Prompt, settings, checkAgentType(role, name, body.Description, body.Prompt, &settings)
	}
	if body.Description != "" || body.Role != "" || body.Prompt != "" || body.Settings != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, refusal.Invalid(ReasonInvalidAgentType, "a copy (from) takes its role, prompt and settings from the type it copies: give only name, and edit the copy")
	}
	source, err := h.store.Get(ctx, orgID, body.From)
	if err != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, err
	}
	if err := domain.CheckOrgAgentRole(source.Role); err != nil {
		return "", "", "", "", domain.AgentTypeSettings{}, refusal.Invalid(ReasonInvalidAgentType, err.Error())
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
			return refusal.Invalid(ReasonInvalidAgentType, err.Error())
		}
	}
	return nil
}

// answer turns what the store refuses into what the caller is told, and anything else into the
// platform failing.
func (h *AgentTypes) answer(op string, err error) error {
	var answered interface{ GetStatus() int }
	var conflictErr *db.AgentTypeVersionConflict
	var inUse *db.AgentTypeInUse
	var limit *db.AgentTypeLimit
	switch {
	case err == nil:
		return nil
	case errors.As(err, &answered):
		return err
	case errors.Is(err, db.ErrAgentTypeNotFound):
		return refusal.NotFound("agent type")
	case errors.Is(err, db.ErrAgentTypeBuiltin):
		return refusal.Refuse(http.StatusForbidden, ReasonBuiltinReadOnly, "a built-in agent type is read-only for everyone: copy it (POST /orgs/{id}/agent-types with from) and change the copy")
	case errors.Is(err, db.ErrAgentTypeArchived):
		return refusal.Conflict(ReasonArchived, "the agent type is archived and no longer edited")
	case errors.As(err, &conflictErr):
		return refusal.Conflict(ReasonVersionConflict, conflictErr.Error())
	case errors.As(err, &inUse):
		return refusal.Conflict(ReasonAgentTypeInUse, inUse.Error()+": archive it once they closed or stopped using it")
	case errors.As(err, &limit) && limit.Types:
		return refusal.Conflict(ReasonAgentTypeLimit, limit.Error())
	case errors.As(err, &limit):
		return refusal.Refuse(http.StatusTooManyRequests, ReasonEditRateLimited, limit.Error())
	default:
		return auth.Internal(h.logger, "agent types request failed", err, zap.String("operation", op))
	}
}
