package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const agentsTestSecret = "a-signing-secret-only-this-test-knows"

// rosterStore is a platform experiment's roster in memory, with the store's own refusals.
type rosterStore struct {
	PlatformExperimentsStore
	pe      *domain.PlatformExperiment
	agents  map[string]*domain.Agent
	signups []domain.AgentSignup
}

func newRosterStore(status domain.PlatformExperimentStatus, maxAgents int) *rosterStore {
	return &rosterStore{
		pe:     &domain.PlatformExperiment{ID: "pe-1", Status: status, MaxAgents: maxAgents},
		agents: map[string]*domain.Agent{},
	}
}

func (f *rosterStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	if id != f.pe.ID {
		return nil, nil
	}
	return f.pe, nil
}

func (f *rosterStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return f.agents[id], nil
}

func (f *rosterStore) competitors() int {
	n := 0
	for _, s := range f.signups {
		if s.Role == domain.SignupRoleCompetitor {
			n++
		}
	}
	return n
}

func (f *rosterStore) IsSignedUp(_ context.Context, _, agentID string) (bool, error) {
	signup, _ := f.GetAgentSignup(context.Background(), f.pe.ID, agentID)
	return signup != nil, nil
}

func (f *rosterStore) Signup(_ context.Context, _ string, signup domain.AgentSignup) (bool, error) {
	if f.pe.Status != domain.PlatformExpOpen {
		return false, nil
	}
	if signup.Role == domain.SignupRoleCompetitor && f.competitors() >= f.pe.MaxAgents {
		return false, db.ErrMaxAgentsReached
	}
	signup.PlatformExperimentID, signup.Name = f.pe.ID, f.agents[signup.AgentID].Name
	f.signups = append(f.signups, signup)
	return true, nil
}

func (f *rosterStore) SpawnAgents(_ context.Context, _ string, agents []*domain.Agent, signups []domain.AgentSignup) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	added := 0
	for _, s := range signups {
		if s.Role == domain.SignupRoleCompetitor {
			added++
		}
	}
	if f.competitors()+added > f.pe.MaxAgents {
		return db.ErrMaxAgentsReached
	}
	for i, agent := range agents {
		agent.OrgID = "org-a"
		f.agents[agent.ID] = agent
		signups[i].PlatformExperimentID, signups[i].Name = f.pe.ID, agent.Name
		f.signups = append(f.signups, signups[i])
	}
	return nil
}

func (f *rosterStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	return append([]domain.AgentSignup{}, f.signups...), nil
}

func (f *rosterStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	for _, s := range f.signups {
		if s.AgentID == agentID {
			return &s, nil
		}
	}
	return nil, nil
}

func (f *rosterStore) RemoveAgent(_ context.Context, _, agentID string) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	for i, s := range f.signups {
		if s.AgentID == agentID {
			f.signups = append(f.signups[:i], f.signups[i+1:]...)
			return nil
		}
	}
	return db.ErrNotSignedUp
}

var agentsTestPlacement = authtest.Placement{
	PlatformExperiments: map[string]domain.Tenancy{
		"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "project-a", OrgID: "org-a"},
	},
	Agents: map[string]string{"veteran": "org-a", "dana": "org-a", "outsider": "org-b"},
}

func agentsRouter(t *testing.T, store *rosterStore, caller auth.Principal) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(agentsTestPlacement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(newSignupTestService(t, store), &fakeProjectRepos{}, zap.NewNop()).WithAgentLaunch("launcher-main", agentsTestSecret)
	RegisterAgentsHuma(doc, handler)
	return r
}

func call(t *testing.T, r http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code, out
}

func joined(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	list, ok := body["agents"].([]any)
	if !ok {
		t.Fatalf("answer has no agents list: %v", body)
	}
	out := make([]map[string]any, len(list))
	for i, a := range list {
		out[i] = a.(map[string]any)
	}
	return out
}

var staff = auth.Principal{Kind: auth.KindStaff}

func TestHostedAgentsAreCreatedForTheConfiguredLauncherAndHandedNoToken(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	code, body := call(t, r, "POST", "/platform-experiments/pe-1/agents",
		`{"count":3,"hosted":true,"name":"sweep","flavor":"hyperparameter-search","model":"claude-sonnet-5","hyperparameters":{"batch_size":64}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, body)
	}
	agents := joined(t, body)
	if len(agents) != 3 || len(store.signups) != 3 {
		t.Fatalf("agents = %d, signups = %d, want 3 and 3", len(agents), len(store.signups))
	}
	seen := map[string]bool{}
	for i, a := range agents {
		id := a["agent_id"].(string)
		if err := domain.ValidateIdentifier("agent_id", id); err != nil || !strings.HasPrefix(id, "ag-") || seen[id] {
			t.Errorf("agent id %q: %v", id, err)
		}
		seen[id] = true
		if _, has := a["token"]; has {
			t.Errorf("a hosted agent was handed its token: %v", a)
		}
		want := map[string]any{
			"hosted": true, "launcher": "launcher-main", "flavor": "hyperparameter-search", "model": "claude-sonnet-5",
			"role": "competitor", "hyperparameters": map[string]any{"batch_size": float64(64)}, "platform_experiment_id": "pe-1",
		}
		for key, value := range want {
			if !reflect.DeepEqual(a[key], value) {
				t.Errorf("agent %d %s = %v, want %v", i, key, a[key], value)
			}
		}
		if store.agents[id].Kind != domain.AgentKindAgent || store.agents[id].OrgID != "org-a" {
			t.Errorf("created agent row = %+v", store.agents[id])
		}
	}
	names := []string{agents[0]["name"].(string), agents[1]["name"].(string), agents[2]["name"].(string)}
	if !reflect.DeepEqual(names, []string{"sweep-1", "sweep-2", "sweep-3"}) {
		t.Errorf("names = %v", names)
	}
}

func TestASelfRunAgentIsHandedTheTokenThatActsAsItAndNoLauncher(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	code, body := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, body)
	}
	agents := joined(t, body)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want one", agents)
	}
	id := agents[0]["agent_id"].(string)
	want, err := auth.Mint(agentsTestSecret, auth.AgentScope("pe-1", id))
	if err != nil {
		t.Fatal(err)
	}
	if agents[0]["token"] != want {
		t.Errorf("token = %v, want the agent:pe-1/%s token", agents[0]["token"], id)
	}
	if agents[0]["hosted"] != false || agents[0]["launcher"] != nil || agents[0]["name"] != id {
		t.Errorf("agent = %v", agents[0])
	}

	_, listing := call(t, r, "GET", "/platform-experiments/pe-1/agents", "")
	listed := joined(t, listing)
	if len(listed) != 1 || listed[0]["agent_id"] != id {
		t.Fatalf("listing = %v", listed)
	}
	if _, has := listed[0]["token"]; has {
		t.Error("the listing carries a token")
	}
}

func TestARegisteredAgentJoinsByIDAndJoiningAgainReturnsTheSameTokenAndChangesNothing(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.agents["veteran"] = &domain.Agent{ID: "veteran", Name: "Veteran", Kind: domain.AgentKindAgent, OrgID: "org-a"}
	r := agentsRouter(t, store, staff)

	code, first := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"agent_id":"veteran","role":"baseline","quota_tier":"burst_only","flavor":"generalist"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, first)
	}
	a := joined(t, first)[0]
	if a["agent_id"] != "veteran" || a["name"] != "Veteran" || a["role"] != "baseline" || a["quota_tier"] != "burst_only" || a["token"] == nil {
		t.Fatalf("agent = %v", a)
	}

	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning, domain.PlatformExpClosed} {
		store.pe.Status = status
		code, again := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"agent_id":"veteran","role":"competitor","hosted":true}`)
		if code != http.StatusCreated {
			t.Fatalf("%s: repeat status = %d: %v", status, code, again)
		}
		b := joined(t, again)[0]
		if b["token"] != a["token"] || b["role"] != "baseline" || b["hosted"] != false {
			t.Errorf("%s: a repeat join changed the agent: %v", status, b)
		}
	}
	if len(store.signups) != 1 {
		t.Errorf("signups = %d, want 1", len(store.signups))
	}
}

func TestJoiningIsRefusedWithAReasonTheCallerCanActOn(t *testing.T) {
	for name, tc := range map[string]struct {
		status     domain.PlatformExperimentStatus
		path, body string
		wantCode   int
		wantReason string
	}{
		"running":                 {domain.PlatformExpRunning, "pe-1", `{}`, 400, "signup_closed"},
		"closed":                  {domain.PlatformExpClosed, "pe-1", `{"hosted":true}`, 400, "signup_closed"},
		"registered, running":     {domain.PlatformExpRunning, "pe-1", `{"agent_id":"dana"}`, 400, "signup_closed"},
		"field full":              {domain.PlatformExpOpen, "pe-1", `{"count":3}`, 400, "max_agents_reached"},
		"registered, full":        {domain.PlatformExpOpen, "pe-1", `{"count":2}` + "\x00" + `{"agent_id":"dana"}`, 400, "max_agents_reached"},
		"no such experiment":      {domain.PlatformExpOpen, "pe-none", `{}`, 404, "experiment_not_found"},
		"no such agent":           {domain.PlatformExpOpen, "pe-1", `{"agent_id":"ghost"}`, 404, "agent_not_found"},
		"unknown role":            {domain.PlatformExpOpen, "pe-1", `{"role":"judge"}`, 400, "unknown_role"},
		"unknown tier":            {domain.PlatformExpOpen, "pe-1", `{"quota_tier":"gold"}`, 400, "invalid_request"},
		"unknown kind":            {domain.PlatformExpOpen, "pe-1", `{"kind":"robot"}`, 400, "invalid_request"},
		"hosted human":            {domain.PlatformExpOpen, "pe-1", `{"kind":"human","hosted":true}`, 400, "invalid_request"},
		"hosted registered human": {domain.PlatformExpOpen, "pe-1", `{"agent_id":"dana","hosted":true}`, 400, "invalid_request"},
		"count too high":          {domain.PlatformExpOpen, "pe-1", `{"count":101}`, 400, "invalid_request"},
		"negative count":          {domain.PlatformExpOpen, "pe-1", `{"count":-1}`, 400, "invalid_request"},
		"count with agent_id":     {domain.PlatformExpOpen, "pe-1", `{"agent_id":"dana","count":2}`, 400, "invalid_request"},
		"name with agent_id":      {domain.PlatformExpOpen, "pe-1", `{"agent_id":"dana","name":"x"}`, 400, "invalid_request"},
		"flavor not an id":        {domain.PlatformExpOpen, "pe-1", `{"flavor":"../x"}`, 400, "invalid_request"},
		"model with a space":      {domain.PlatformExpOpen, "pe-1", `{"model":"a b"}`, 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRosterStore(tc.status, 2)
			store.agents["dana"] = &domain.Agent{ID: "dana", Name: "Dana", Kind: domain.AgentKindHuman, OrgID: "org-a"}
			r := agentsRouter(t, store, staff)
			body := tc.body
			if before, last, split := strings.Cut(tc.body, "\x00"); split {
				if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", before); code != http.StatusCreated {
					t.Fatalf("setup: %d %v", code, out)
				}
				body = last
			}
			had := len(store.signups)
			code, out := call(t, r, "POST", "/platform-experiments/"+tc.path+"/agents", body)
			if code != tc.wantCode || out["reason"] != tc.wantReason {
				t.Fatalf("got = %d %v, want = %d with reason %s", code, out, tc.wantCode, tc.wantReason)
			}
			if !strings.HasPrefix(out["message"].(string), tc.wantReason) {
				t.Errorf("message %q does not lead with its reason", out["message"])
			}
			if len(store.signups) != had {
				t.Errorf("a refused join left %d signups behind", len(store.signups)-had)
			}
		})
	}
}

func TestBaselinesAndReviewersJoinAFullField(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 1)
	r := agentsRouter(t, store, staff)
	for _, body := range []string{`{}`, `{"role":"baseline"}`, `{"role":"reviewer","count":2}`} {
		if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body); code != http.StatusCreated {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if len(store.signups) != 4 {
		t.Fatalf("signups = %d, want 4", len(store.signups))
	}
}

func TestAnAgentIsRemovedOnlyBeforeTheStart(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	_, body := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"count":2,"hosted":true}`)
	agents := joined(t, body)
	first, second := agents[0]["agent_id"].(string), agents[1]["agent_id"].(string)

	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+first, ""); code != http.StatusOK || out["status"] != "removed" {
		t.Fatalf("remove: %d %v", code, out)
	}
	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+first, ""); code != http.StatusNotFound || out["reason"] != "not_signed_up" {
		t.Fatalf("remove twice: %d %v", code, out)
	}
	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-none/agents/"+second, ""); code != http.StatusNotFound || out["reason"] != "experiment_not_found" {
		t.Fatalf("remove from no experiment: %d %v", code, out)
	}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		store.pe.Status = status
		if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+second, ""); code != http.StatusConflict || out["reason"] != "experiment_started" {
			t.Fatalf("remove while %s: %d %v", status, code, out)
		}
	}
	if len(store.signups) != 1 || store.signups[0].AgentID != second {
		t.Fatalf("signups = %+v", store.signups)
	}
}

func TestOnlyACallerOfTheExperimentsOrgMayChangeOrReadItsRoster(t *testing.T) {
	inOrg := []string{"org-a"}
	for name, tc := range map[string]struct {
		caller auth.Principal
		want   int
	}{
		"staff":                         {auth.Principal{Kind: auth.KindStaff}, http.StatusCreated},
		"user of the org":               {auth.Principal{Kind: auth.KindUser, OrgIDs: inOrg}, http.StatusCreated},
		"key of the org":                {auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg}, http.StatusCreated},
		"key of the project":            {auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg, ProjectID: "project-a"}, http.StatusCreated},
		"coordinator of the experiment": {auth.Principal{Kind: auth.KindCoordinator, OrgIDs: inOrg, ProjectID: "project-a", PlatformExperimentID: "pe-1"}, http.StatusCreated},
		"user of another org":           {auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-b"}}, http.StatusNotFound},
		"key of another project":        {auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg, ProjectID: "project-z"}, http.StatusNotFound},
		"coordinator of another":        {auth.Principal{Kind: auth.KindCoordinator, OrgIDs: inOrg, ProjectID: "project-a", PlatformExperimentID: "pe-2"}, http.StatusNotFound},
		"agent of the experiment":       {auth.Principal{Kind: auth.KindAgent, OrgIDs: inOrg, ProjectID: "project-a", PlatformExperimentID: "pe-1", AgentID: "veteran"}, http.StatusForbidden},
		"job":                           {auth.Principal{Kind: auth.KindJob, OrgIDs: inOrg, PlatformExperimentID: "pe-1", ExperimentID: "job-1"}, http.StatusForbidden},
		"runtime":                       {auth.Principal{Kind: auth.KindRuntime, OrgIDs: inOrg, ClusterName: "pe-1"}, http.StatusForbidden},
		"launcher":                      {auth.Principal{Kind: auth.KindLauncher, LauncherName: "launcher-main"}, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRosterStore(domain.PlatformExpOpen, 5)
			r := agentsRouter(t, store, tc.caller)
			code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true}`)
			if code != tc.want {
				t.Fatalf("add: got = %d %v, want = %d", code, out, tc.want)
			}
			admitted := tc.want == http.StatusCreated
			if admitted != (len(store.signups) == 1) {
				t.Fatalf("signups = %d for status %d", len(store.signups), code)
			}

			// What a refused caller is refused on, it is refused on for reading and removing too.
			store.signups = []domain.AgentSignup{{AgentID: "ag-seeded", PlatformExperimentID: "pe-1"}}
			wantRead, wantRemove := http.StatusOK, http.StatusOK
			if !admitted {
				wantRead, wantRemove = tc.want, tc.want
			}
			if code, out := call(t, r, "GET", "/platform-experiments/pe-1/agents", ""); code != wantRead {
				t.Errorf("list: got = %d %v, want = %d", code, out, wantRead)
			}
			if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/ag-seeded", ""); code != wantRemove {
				t.Errorf("remove: got = %d %v, want = %d", code, out, wantRemove)
			}
			if !admitted && len(store.signups) != 1 {
				t.Error("a refused caller removed an agent")
			}
		})
	}
}

func TestACallerMayOnlyJoinARegisteredAgentOfItsOwnOrg(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.agents["outsider"] = &domain.Agent{ID: "outsider", Kind: domain.AgentKindAgent, OrgID: "org-b"}
	r := agentsRouter(t, store, auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a"}})
	code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"agent_id":"outsider"}`)
	if code != http.StatusNotFound || len(store.signups) != 0 {
		t.Fatalf("got = %d %v with %d signups, want = 404 and none", code, out, len(store.signups))
	}
}

func TestTheAgentsAPICannotBeRegisteredWithoutALauncherAndASecret(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registered without WithAgentLaunch")
		}
	}()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(agentsTestPlacement), zap.NewNop())
	RegisterAgentsHuma(doc, NewPlatformExperimentsHandler(newSignupTestService(t, newRosterStore(domain.PlatformExpOpen, 1)), &fakeProjectRepos{}, zap.NewNop()))
}
