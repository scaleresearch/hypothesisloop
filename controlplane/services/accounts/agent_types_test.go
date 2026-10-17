package accounts

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// memAgentTypes holds the built-in types and those orgs make, as db.AgentTypesStore scopes them.
type memAgentTypes struct {
	types    map[string]*domain.CurrentAgentType
	versions map[string][]domain.AgentTypeVersion
	events   map[string][]domain.AgentTypeEvent
	next     int
	// limit, when set, is what the next write is refused with.
	limit *db.AgentTypeLimit
}

func newMemAgentTypes() *memAgentTypes {
	m := &memAgentTypes{types: map[string]*domain.CurrentAgentType{}, versions: map[string][]domain.AgentTypeVersion{}, events: map[string][]domain.AgentTypeEvent{}}
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

func (m *memAgentTypes) Events(_ context.Context, orgID, id string) ([]domain.AgentTypeEvent, error) {
	if _, err := m.reads(orgID, id); err != nil {
		return nil, err
	}
	return m.events[id], nil
}

func (m *memAgentTypes) record(id string, action domain.AgentTypeAction, version int, by domain.Actor) {
	m.events[id] = append([]domain.AgentTypeEvent{{TypeID: id, Action: action, Version: version, By: &by}}, m.events[id]...)
}

func (m *memAgentTypes) Create(_ context.Context, orgID, name, description string, role domain.AgentRole, prompt string, settings domain.AgentTypeSettings, by domain.Actor) (*domain.CurrentAgentType, error) {
	if m.limit != nil {
		return nil, m.limit
	}
	m.next++
	id := "at-" + strings.Repeat("0", m.next)
	version := domain.AgentTypeVersion{TypeID: id, Version: 1, Prompt: prompt, Settings: settings, CreatedBy: &by}
	m.types[id] = &domain.CurrentAgentType{AgentType: domain.AgentType{ID: id, OrgID: orgID, Name: name, Description: description, Role: role, CurrentVersion: 1, CreatedBy: &by}, Current: version}
	m.versions[id] = []domain.AgentTypeVersion{version}
	m.record(id, domain.AgentTypeCreated, 1, by)
	return m.types[id], nil
}

func (m *memAgentTypes) changes(orgID, id string) (*domain.CurrentAgentType, error) {
	t, err := m.reads(orgID, id)
	switch {
	case err != nil:
		return nil, err
	case t.Builtin:
		return nil, db.ErrAgentTypeBuiltin
	case m.limit != nil:
		return nil, m.limit
	}
	return t, nil
}

func (m *memAgentTypes) newVersion(t *domain.CurrentAgentType, prompt string, settings domain.AgentTypeSettings, action domain.AgentTypeAction, by domain.Actor) *domain.Propagation {
	version := domain.AgentTypeVersion{TypeID: t.ID, Version: t.CurrentVersion + 1, Prompt: prompt, Settings: settings, CreatedBy: &by}
	m.versions[t.ID] = append(m.versions[t.ID], version)
	t.CurrentVersion, t.Current = version.Version, version
	m.record(t.ID, action, version.Version, by)
	return &domain.Propagation{Version: version.Version, Told: 1}
}

func (m *memAgentTypes) Edit(_ context.Context, e db.AgentTypeEdit) (*domain.CurrentAgentType, *domain.Propagation, error) {
	t, err := m.changes(e.OrgID, e.ID)
	if err != nil {
		return nil, nil, err
	}
	if t.CurrentVersion != e.IfVersion {
		return nil, nil, &db.AgentTypeVersionConflict{IfVersion: e.IfVersion, Current: t.CurrentVersion}
	}
	t.Name, t.Description = e.Name, e.Description
	return t, m.newVersion(t, e.Prompt, e.Settings, domain.AgentTypeEdited, e.By), nil
}

func (m *memAgentTypes) Restore(_ context.Context, orgID, id string, n int, by domain.Actor) (*domain.CurrentAgentType, *domain.Propagation, error) {
	t, err := m.changes(orgID, id)
	if err != nil {
		return nil, nil, err
	}
	if n < 1 || n > len(m.versions[id]) {
		return nil, nil, db.ErrAgentTypeNotFound
	}
	from := m.versions[id][n-1]
	return t, m.newVersion(t, from.Prompt, from.Settings, domain.AgentTypeRestored, by), nil
}

func (m *memAgentTypes) Archive(_ context.Context, orgID, id string, by domain.Actor) (*domain.CurrentAgentType, error) {
	t, err := m.reads(orgID, id)
	if err != nil {
		return nil, err
	}
	if t.Builtin {
		return nil, db.ErrAgentTypeBuiltin
	}
	m.record(id, domain.AgentTypeArchived, t.CurrentVersion, by)
	return t, nil
}

const ownPrompt = "{{rules}}\n{{hl_commands}}\nYou are {{agent_id}}."

func ownSettings() name {
	return name{"max_subagents": 3, "effort": "medium"}
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
		"too many subagents":     {"name": "x", "role": "researcher", "prompt": ownPrompt, "settings": name{"max_subagents": 11, "effort": "high"}},
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

// An org makes researcher and baseline types only: a coordinator or a planner runs its built-in
// type, whether asked for from scratch or as a copy.
func TestAnOrgMakesNoCoordinatorOrPlannerType(t *testing.T) {
	w := newWorld(t)
	for _, role := range []string{"coordinator", "planner"} {
		w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "x", "role": role, "prompt": "{{rules}}\n{{hl_commands}}", "settings": ownSettings()}, http.StatusUnprocessableEntity, ReasonInvalidAgentType)
		w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": role}, http.StatusUnprocessableEntity, ReasonInvalidAgentType)
	}
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": "baseline"}, http.StatusCreated, nil)
}

// A restore makes the next version hold an earlier one's prompt and settings; each change is kept
// and every member reads them.
func TestAnOwnerRestoresAnEarlierVersionAndEveryChangeIsKept(t *testing.T) {
	w := newWorld(t)
	var made domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "Mine", "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, &made)
	w.ok("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+made.ID, name{"if_version": 1, "name": "Mine", "prompt": ownPrompt + " Two.", "settings": ownSettings()}, http.StatusOK, nil)
	var restored EditedAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 1}, http.StatusOK, &restored)
	if restored.CurrentVersion != 3 || restored.Current.Prompt != ownPrompt || restored.Propagation.Version != 3 || restored.Propagation.Told != 1 {
		t.Fatalf("restore: got = %+v, want version 3 holding version 1", restored)
	}
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 9}, http.StatusNotFound, nil)
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 0}, http.StatusUnprocessableEntity, nil)
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types/generalist/restore", name{"version": 1}, http.StatusForbidden, ReasonBuiltinReadOnly)
	w.ok("member", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 1}, http.StatusForbidden, nil)
	w.ok("outsider", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 1}, http.StatusNotFound, nil)
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/archive", nil, http.StatusOK, nil)

	var events []domain.AgentTypeEvent
	w.ok("member", "GET", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/events", nil, http.StatusOK, &events)
	var got []string
	for _, e := range events {
		got = append(got, string(e.Action))
	}
	if strings.Join(got, ",") != "archive,restore,edit,create" || events[0].By == nil || events[0].By.ID != "u-owner" {
		t.Errorf("events: got = %+v, want archive, restore, edit, create by the owner", events)
	}
	w.ok("outsider", "GET", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/events", nil, http.StatusNotFound, nil)
	w.ok("outsider", "GET", "/orgs/"+w.orgB+"/agent-types/"+made.ID+"/events", nil, http.StatusNotFound, nil)
}

// Only an org's administrators change its types: a member is refused every write and reads, and to
// another org's people the org is not found.
func TestOnlyAnOrgsAdministratorsChangeItsTypes(t *testing.T) {
	w := newWorld(t)
	var made domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "Mine", "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, &made)
	base := "/orgs/" + w.orgA + "/agent-types"
	edit := name{"if_version": 1, "name": "x", "prompt": ownPrompt, "settings": ownSettings()}
	for caller, want := range map[string]int{"member": http.StatusForbidden, "outsider": http.StatusNotFound} {
		w.ok(caller, "POST", base, name{"from": "generalist"}, want, nil)
		w.ok(caller, "PUT", base+"/"+made.ID, edit, want, nil)
		w.ok(caller, "POST", base+"/"+made.ID+"/archive", nil, want, nil)
		w.ok(caller, "POST", base+"/"+made.ID+"/restore", name{"version": 1}, want, nil)
	}
	for _, path := range []string{base, base + "/" + made.ID, base + "/" + made.ID + "/versions", base + "/" + made.ID + "/versions/1", base + "/" + made.ID + "/events"} {
		w.ok("member", "GET", path, nil, http.StatusOK, nil)
		w.ok("outsider", "GET", path, nil, http.StatusNotFound, nil)
	}
	w.refusedWith("owner", "PUT", base+"/generalist", edit, http.StatusForbidden, ReasonBuiltinReadOnly)
	w.refusedWith("owner", "POST", base+"/generalist/archive", nil, http.StatusForbidden, ReasonBuiltinReadOnly)
}

// The limits on an org's types and on the versions of one are refused with what they are.
func TestTheLimitsOnAnOrgsTypesAreRefusedClearly(t *testing.T) {
	w := newWorld(t)
	var made domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "Mine", "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, &made)
	w.agentTypes.limit = &db.AgentTypeLimit{Types: true, Limit: domain.MaxOrgAgentTypes}
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": "generalist"}, http.StatusConflict, ReasonAgentTypeLimit)
	w.agentTypes.limit = &db.AgentTypeLimit{Limit: domain.MaxAgentTypeEditsAnHour}
	w.refusedWith("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+made.ID, name{"if_version": 1, "name": "x", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusTooManyRequests, ReasonEditRateLimited)
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/agent-types/"+made.ID+"/restore", name{"version": 1}, http.StatusTooManyRequests, ReasonEditRateLimited)
}
