package accounts

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// memAgentTypes holds the built-in types and those orgs make, as db.AgentTypesStore scopes them.
type memAgentTypes struct {
	types    map[string]*domain.CurrentAgentType
	versions map[string][]domain.AgentTypeVersion
	next     int
}

func newMemAgentTypes() *memAgentTypes {
	m := &memAgentTypes{types: map[string]*domain.CurrentAgentType{}, versions: map[string][]domain.AgentTypeVersion{}}
	for _, t := range agenttypes.Catalog() {
		m.types[t.ID] = &t
		m.versions[t.ID] = []domain.AgentTypeVersion{t.Current}
	}
	return m
}

func (m *memAgentTypes) reads(orgID, id string) (*domain.CurrentAgentType, error) {
	t, ok := m.types[id]
	if !ok || !t.Builtin && t.OrgID != orgID {
		return nil, db.ErrAgentTypeNotFound
	}
	return t, nil
}

func (m *memAgentTypes) List(_ context.Context, orgID string) (domain.AgentTypeCatalog, error) {
	var out domain.AgentTypeCatalog
	for _, t := range m.types {
		if t.Builtin || t.OrgID == orgID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (m *memAgentTypes) Get(_ context.Context, orgID, id string) (*domain.CurrentAgentType, error) {
	return m.reads(orgID, id)
}

func (m *memAgentTypes) Versions(_ context.Context, orgID, id string) ([]domain.AgentTypeVersion, error) {
	if _, err := m.reads(orgID, id); err != nil {
		return nil, err
	}
	return m.versions[id], nil
}

func (m *memAgentTypes) Version(_ context.Context, orgID, id string, n int) (*domain.AgentTypeVersion, error) {
	if _, err := m.reads(orgID, id); err != nil || n < 1 || n > len(m.versions[id]) {
		return nil, db.ErrAgentTypeNotFound
	}
	return &m.versions[id][n-1], nil
}

func (m *memAgentTypes) Create(_ context.Context, orgID, name string, role domain.AgentRole, prompt string, settings domain.AgentTypeSettings, by domain.Actor) (*domain.CurrentAgentType, error) {
	m.next++
	id := "at-" + strings.Repeat("0", m.next)
	version := domain.AgentTypeVersion{TypeID: id, Version: 1, Prompt: prompt, Settings: settings, CreatedBy: &by}
	m.types[id] = &domain.CurrentAgentType{AgentType: domain.AgentType{ID: id, OrgID: orgID, Name: name, Role: role, CurrentVersion: 1, CreatedBy: &by}, Current: version}
	m.versions[id] = []domain.AgentTypeVersion{version}
	return m.types[id], nil
}

func (m *memAgentTypes) Edit(_ context.Context, e db.AgentTypeEdit, _ domain.ModelDefaults) (*domain.CurrentAgentType, *domain.Propagation, error) {
	t, err := m.reads(e.OrgID, e.ID)
	switch {
	case err != nil:
		return nil, nil, err
	case t.Builtin:
		return nil, nil, db.ErrAgentTypeBuiltin
	case t.CurrentVersion != e.IfVersion:
		return nil, nil, &db.AgentTypeVersionConflict{IfVersion: e.IfVersion, Current: t.CurrentVersion}
	}
	version := domain.AgentTypeVersion{TypeID: t.ID, Version: t.CurrentVersion + 1, Prompt: e.Prompt, Settings: e.Settings, CreatedBy: &e.By}
	m.versions[t.ID] = append(m.versions[t.ID], version)
	t.Name, t.CurrentVersion, t.Current = e.Name, version.Version, version
	return t, &domain.Propagation{Version: version.Version, Moved: []domain.PinMove{}, Failed: []domain.PinMove{}}, nil
}

func (m *memAgentTypes) Archive(_ context.Context, orgID, id string) (*domain.CurrentAgentType, error) {
	t, err := m.reads(orgID, id)
	if err != nil {
		return nil, err
	}
	if t.Builtin {
		return nil, db.ErrAgentTypeBuiltin
	}
	return t, nil
}

const ownPrompt = "{{rules}}\n{{hl_commands}}\nYou are {{agent_id}}."

func ownSettings() name {
	return name{"max_subagents": 3, "pacing_subagents": 1, "effort": "medium"}
}

func TestAnOrgsOwnerCopiesABuiltinAndEditsTheCopy(t *testing.T) {
	w := newWorld(t)
	var copied domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": "generalist"}, http.StatusCreated, &copied)
	generalist := w.agentTypes.types["generalist"]
	if copied.Name != "Copy of Generalist" || copied.Role != domain.AgentRoleResearcher || copied.Builtin || copied.OrgID != w.orgA ||
		copied.Current.Prompt != generalist.Current.Prompt || copied.Current.Settings.MaxSubagents != 4 || copied.CreatedBy == nil || copied.CreatedBy.ID != "u-owner" {
		t.Fatalf("copy: got = %+v, want the generalist's prompt and settings in a type of org a made by its owner", copied)
	}
	var edited EditedAgentType
	w.ok("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+copied.ID, name{"if_version": 1, "name": "Tuner", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusOK, &edited)
	if edited.CurrentVersion != 2 || edited.Name != "Tuner" || edited.Current.Prompt != ownPrompt || edited.Current.Settings.MaxSubagents != 3 || edited.Propagation.Version != 2 {
		t.Fatalf("edit: got = %+v", edited)
	}
	w.refusedWith("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+copied.ID, name{"if_version": 1, "name": "Tuner", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusConflict, ReasonVersionConflict)
	var versions []domain.AgentTypeVersion
	w.ok("member", "GET", "/orgs/"+w.orgA+"/agent-types/"+copied.ID+"/versions", nil, http.StatusOK, &versions)
	if len(versions) != 2 {
		t.Errorf("versions: got = %+v, want two", versions)
	}
	w.ok("outsider", "GET", "/orgs/"+w.orgB+"/agent-types/"+copied.ID+"/versions/1", nil, http.StatusNotFound, nil)
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": "generalist", "prompt": ownPrompt}, http.StatusUnprocessableEntity, ReasonInvalidAgentType)
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": copied.ID, "name": "Again"}, http.StatusCreated, nil)
}

func TestAPromptOrSettingsAnAgentCannotRunWithAreRefused(t *testing.T) {
	w := newWorld(t)
	for what, body := range map[string]name{
		"no rules":               {"name": "x", "role": "researcher", "prompt": "{{hl_commands}}", "settings": ownSettings()},
		"a placeholder of none":  {"name": "x", "role": "researcher", "prompt": ownPrompt + "{{flavor_brief}}", "settings": ownSettings()},
		"a coordinator agent id": {"name": "x", "role": "coordinator", "prompt": ownPrompt, "settings": ownSettings()},
		"no role":                {"name": "x", "prompt": ownPrompt, "settings": ownSettings()},
		"no settings":            {"name": "x", "role": "researcher", "prompt": ownPrompt},
		"no name":                {"role": "researcher", "prompt": ownPrompt, "settings": ownSettings()},
		"too many subagents":     {"name": "x", "role": "researcher", "prompt": ownPrompt, "settings": name{"max_subagents": 11, "pacing_subagents": 1, "effort": "high"}},
	} {
		if rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/agent-types", body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), ReasonInvalidAgentType) {
			t.Errorf("%s: got = %d %s, want 422 %s", what, rec.Code, rec.Body, ReasonInvalidAgentType)
		}
	}
	var made domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "Mine", "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, &made)
	if made.Current.Settings.Hyperparameters == nil {
		t.Error("hyperparameters left out are stored as none, an empty object")
	}
}
