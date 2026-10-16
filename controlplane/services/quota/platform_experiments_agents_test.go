package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
)

const agentsTestSecret = "a-signing-secret-only-this-test-knows"

// rosterStore is a platform experiment's roster in memory, with the store's own refusals.
type rosterStore struct {
	PlatformExperimentsStore
	pe      *domain.PlatformExperiment
	agents  map[string]*domain.Agent
	signups []domain.AgentSignup
	// unregistered is a launcher config names and no operator registered.
	unregistered bool
	// left is the COMPLETED jobs on the objective its baseline agents that finished left.
	left []*domain.Experiment
	// objective is every job on the objective, oldest first.
	objective []*domain.Experiment
	// completed is every COMPLETED job, oldest first.
	completed []*domain.Experiment
	// inFlight is the agents that have a job queued or running.
	inFlight map[string]bool
	// lateShare is what the researchers that ended left unspent, for one joining the run.
	lateShare float64
	// submitted is how many jobs each agent submitted.
	submitted map[string]int
	// hypotheses is the graph, and graphErr what reading it fails with.
	hypotheses []*domain.Hypothesis
	graphErr   error
	// quota is every agent's allocation, nil for none.
	quota *domain.AgentQuota
}

// MessageAllowance answers every agent the whole hour's messages.
func (s *rosterStore) MessageAllowance(context.Context, string) (map[string]domain.MessageAllowance, error) {
	out := map[string]domain.MessageAllowance{}
	for _, signup := range s.signups {
		out[signup.AgentID] = domain.MessageAllowance{LeftThisHour: domain.MaxAgentMessagesPerHour}
	}
	return out, nil
}

func (s *rosterStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	jobs := map[string]db.AgentJobs{}
	for agentID, submitted := range s.submitted {
		jobs[agentID] = db.AgentJobs{Submitted: submitted}
	}
	return jobs, nil
}

// ListHypotheses answers as the graph does: the hypotheses the filter keeps, in the order given.
func (f *rosterStore) ListHypotheses(_ context.Context, filter graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.graphErr != nil {
		return nil, f.graphErr
	}
	var out []*domain.HypothesisListItem
	for _, h := range f.hypotheses {
		agent := filter.AgentID == "" || h.AgentID == filter.AgentID
		listed := filter.IDs == nil || slices.Contains(filter.IDs, h.ID)
		if h.PlatformExperimentID == filter.PlatformExperimentID && agent && listed && h.Status == filter.Status && len(out) < filter.Limit {
			out = append(out, &domain.HypothesisListItem{Hypothesis: h})
		}
	}
	return out, nil
}

func (f *rosterStore) ListExperimentsTesting(context.Context, []string) ([]*domain.Experiment, error) {
	return f.objective, nil
}

func (f *rosterStore) FinishedBaselineJobs(context.Context, string) ([]*domain.Experiment, error) {
	return f.left, nil
}

func (f *rosterStore) CompletedJobs(context.Context, string) ([]*domain.Experiment, error) {
	return f.completed, nil
}

func (f *rosterStore) Reachable(_ context.Context, _, repository string) (string, error) {
	return repository, nil
}

func (f *rosterStore) URL(string) string { return "" }

func (f *rosterStore) File(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (f *rosterStore) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	return nil, nil
}

func (f *rosterStore) Serves(string, string, string, string) bool { return false }

func (f *rosterStore) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, githost.ErrFileNotFound
}

func (f *rosterStore) Create(context.Context, string, string, string, string, map[string]string) (string, error) {
	return "", nil
}

func (f *rosterStore) CreatesOn(context.Context, string, string) (string, error) { return "", nil }

// LauncherAnsweredAt answers every launcher as asking now.
func (f *rosterStore) LauncherAnsweredAt(_ context.Context, names []string) (map[string]*time.Time, error) {
	now := time.Now()
	out := map[string]*time.Time{}
	for _, name := range names {
		out[name] = &now
	}
	return out, nil
}

const rosterMaxHosted = 6

func (f *rosterStore) ListSignups(context.Context, string) ([]string, error) {
	ids := make([]string, 0, len(f.signups))
	for _, s := range f.signups {
		ids = append(ids, s.AgentID)
	}
	return ids, nil
}

func (f *rosterStore) LauncherKnown(context.Context, string) (bool, error) {
	return !f.unregistered, nil
}

// machines is what the platform rents for the experiment at once, with more of them added.
func (f *rosterStore) machines(more ...domain.AgentSignup) int {
	measuring, others := f.rented(more...)
	return measuring + others
}

// rented counts the machines as the store does: an agent that finished holds none, and those
// that measure the baseline are counted apart from the coordinator and the other agents.
func (f *rosterStore) rented(more ...domain.AgentSignup) (measuring, others int) {
	if f.pe.CoordinatorHosted {
		others++
	}
	for _, s := range append(append([]domain.AgentSignup{}, f.signups...), more...) {
		switch {
		case !s.Hosted || s.FinishedAt != nil || s.Instance != nil && s.Instance.Stopped:
		case s.Role == domain.SignupRoleBaseline:
			measuring++
		default:
			others++
		}
	}
	return measuring, others
}

// full says more would be a machine too many, or would take the one kept for the baseline.
func (f *rosterStore) full(more domain.AgentSignup) bool {
	measuring, others := f.rented(more)
	return more.Hosted && (others > f.pe.MaxHosted-1 || measuring+others > f.pe.MaxHosted)
}

func newRosterStore(status domain.PlatformExperimentStatus, maxAgents int) *rosterStore {
	return &rosterStore{
		pe: &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "project-a", Status: status, MaxAgents: maxAgents, MaxHosted: rosterMaxHosted, Repository: "acme/model",
		},
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
		if s.Role == domain.SignupRoleCompetitor && s.FinishedAt == nil && (s.Instance == nil || !s.Instance.Stopped) {
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
	if f.agents[signup.AgentID] == nil || f.agents[signup.AgentID].OrgID != "org-a" {
		return false, db.ErrAgentOutsideOrg
	}
	if signup.Role == domain.SignupRoleCompetitor && f.competitors() >= f.pe.MaxAgents {
		return false, db.ErrMaxAgentsReached
	}
	if f.full(signup) {
		return false, db.ErrMaxHostedReached
	}
	signup.PlatformExperimentID, signup.Name = f.pe.ID, f.agents[signup.AgentID].Name
	f.signups = append(f.signups, signup)
	return true, nil
}

func (f *rosterStore) SpawnAgent(_ context.Context, _ string, agent *domain.Agent, signup domain.AgentSignup) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	if signup.Role == domain.SignupRoleCompetitor && f.competitors() >= f.pe.MaxAgents {
		return db.ErrMaxAgentsReached
	}
	if f.full(signup) {
		return db.ErrMaxHostedReached
	}
	taken := func(name string) bool {
		for _, s := range f.signups {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	if agent.Name == "" {
		agent.Name = domain.AgentName(signup.Role, taken)
	} else if taken(agent.Name) {
		return db.ErrAgentNameTaken
	}
	agent.OrgID = "org-a"
	f.agents[agent.ID] = agent
	signup.PlatformExperimentID, signup.Name = f.pe.ID, agent.Name
	f.signups = append(f.signups, signup)
	return nil
}

func (f *rosterStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	var ids []string
	for _, s := range f.signups {
		if s.Role == role {
			ids = append(ids, s.AgentID)
		}
	}
	return ids, nil
}

// JoinRunning joins as an open experiment would, while the researchers that ended left
// lateShare unspent.
func (f *rosterStore) JoinRunning(ctx context.Context, id string, agent *domain.Agent, signup domain.AgentSignup, _ db.LateJoin) error {
	if f.pe.Status != domain.PlatformExpRunning || signup.Role != domain.SignupRoleCompetitor {
		return db.ErrSignupClosed
	}
	if f.lateShare <= 0 {
		return db.ErrNoShareLeft
	}
	f.pe.Status = domain.PlatformExpOpen
	defer func() { f.pe.Status = domain.PlatformExpRunning }()
	if agent != nil {
		return f.SpawnAgent(ctx, id, agent, signup)
	}
	_, err := f.Signup(ctx, id, signup)
	return err
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

func (f *rosterStore) NameActors(context.Context, []*domain.Actor) error { return nil }

func (f *rosterStore) RemoveAgent(_ context.Context, _, agentID string, _ func(context.Context, string) error) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	for i, s := range f.signups {
		if s.AgentID == agentID {
			if f.inFlight[agentID] {
				return db.ErrWorkUnfinished
			}
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
	r := authtest.Router(authtest.As(caller))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()).WithAgentLaunch(agentsTestLaunch())
	RegisterAgentsHuma(doc, handler, store)
	return r
}

func agentsTestLaunch() AgentLaunch {
	return AgentLaunch{Launcher: "launcher-main", DefaultModel: "platform-model-1", CoordinatorModel: "platform-coordinator-1", SigningSecret: agentsTestSecret, MaxHosted: 20}
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

// addEach makes the same request n times, as a client that wants n agents does, and returns the
// one agent each answered with.
func addEach(t *testing.T, r http.Handler, n int, body string) []map[string]any {
	t.Helper()
	var all []map[string]any
	for range n {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		if code != http.StatusCreated {
			t.Fatalf("%s: got = %d %v, want = 201", body, code, out)
		}
		one := joined(t, out)
		if len(one) != 1 {
			t.Fatalf("%s: answered %d agents, want the one it added", body, len(one))
		}
		all = append(all, one[0])
	}
	return all
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
	agents := addEach(t, r, 3, `{"hosted":true,"flavor":"hyperparameter-search","model":"claude-sonnet-5","hyperparameters":{"batch_size":64}}`)
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
		if store.agents[id].OrgID != "org-a" {
			t.Errorf("created agent row = %+v", store.agents[id])
		}
	}
	names := []string{agents[0]["name"].(string), agents[1]["name"].(string), agents[2]["name"].(string)}
	if !reflect.DeepEqual(names, []string{"Researcher 1", "Researcher 2", "Researcher 3"}) {
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
	// Named by its place, which is what a person reads in every list in place of the id.
	if agents[0]["hosted"] != false || agents[0]["launcher"] != nil || agents[0]["name"] != "Researcher 1" {
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
	store.agents["veteran"] = &domain.Agent{ID: "veteran", Name: "Veteran", OrgID: "org-a"}
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
		"running, nothing left":  {domain.PlatformExpRunning, "pe-1", `{}`, 409, "no_share_left"},
		"running, a reviewer":    {domain.PlatformExpRunning, "pe-1", `{"role":"reviewer"}`, 409, "signup_closed"},
		"closed":                 {domain.PlatformExpClosed, "pe-1", `{"hosted":true}`, 409, "signup_closed"},
		"registered, running":    {domain.PlatformExpRunning, "pe-1", `{"agent_id":"dana"}`, 409, "no_share_left"},
		"field full":             {domain.PlatformExpOpen, "pe-1", `{}` + "\x00" + `{}` + "\x00" + `{}`, 409, "max_agents_reached"},
		"registered, full":       {domain.PlatformExpOpen, "pe-1", `{}` + "\x00" + `{}` + "\x00" + `{"agent_id":"dana"}`, 409, "max_agents_reached"},
		"no such experiment":     {domain.PlatformExpOpen, "pe-none", `{}`, 404, "experiment_not_found"},
		"no such agent":          {domain.PlatformExpOpen, "pe-1", `{"agent_id":"ghost"}`, 404, "agent_not_found"},
		"unknown role":           {domain.PlatformExpOpen, "pe-1", `{"role":"judge"}`, 400, "unknown_role"},
		"unknown tier":           {domain.PlatformExpOpen, "pe-1", `{"quota_tier":"gold"}`, 400, "invalid_request"},
		"name with agent_id":     {domain.PlatformExpOpen, "pe-1", `{"agent_id":"dana","name":"x"}`, 400, "invalid_request"},
		"flavor not an id":       {domain.PlatformExpOpen, "pe-1", `{"flavor":"../x"}`, 400, "invalid_request"},
		"unknown flavor":         {domain.PlatformExpOpen, "pe-1", `{"flavor":"prompt-search"}`, 400, "invalid_request"},
		"unknown flavor, hosted": {domain.PlatformExpOpen, "pe-1", `{"flavor":"prompt-search","hosted":true}`, 400, "invalid_request"},
		"model with a space":     {domain.PlatformExpOpen, "pe-1", `{"model":"a b"}`, 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRosterStore(tc.status, 2)
			store.agents["dana"] = &domain.Agent{ID: "dana", Name: "Dana", OrgID: "org-a"}
			r := agentsRouter(t, store, staff)
			bodies := strings.Split(tc.body, "\x00")
			body := bodies[len(bodies)-1]
			for _, before := range bodies[:len(bodies)-1] {
				addEach(t, r, 1, before)
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
	for _, body := range []string{`{}`, `{"role":"baseline"}`, `{"role":"reviewer"}`, `{"role":"reviewer"}`} {
		if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body); code != http.StatusCreated {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if len(store.signups) != 4 {
		t.Fatalf("signups = %d, want 4", len(store.signups))
	}
}

// max_agents bounds competitors only, so without a bound on machines a caller could add hosted
// baselines or reviewers a few at a time for as long as it liked.
func TestHostedAgentsOfEveryRoleStopAtThePlatformsBoundOnMachines(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 1)
	store.pe.CoordinatorHosted = true
	store.agents["veteran"] = &domain.Agent{ID: "veteran", Name: "Veteran", OrgID: "org-a"}
	r := agentsRouter(t, store, staff)
	addEach(t, r, 3, `{"hosted":true,"role":"baseline"}`)
	addEach(t, r, 2, `{"hosted":true,"role":"reviewer"}`)
	for _, body := range []string{`{"hosted":true,"role":"baseline"}`, `{"hosted":true,"role":"reviewer"}`, `{"hosted":true}`, `{"agent_id":"veteran","hosted":true,"role":"reviewer"}`} {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		if code != http.StatusConflict || out["reason"] != "max_hosted_reached" {
			t.Fatalf("%s with the coordinator and five agents hosted: got = %d %v, want = 409 max_hosted_reached", body, code, out)
		}
		if message, _ := out["message"].(string); !strings.Contains(message, "at most 6 machines") {
			t.Errorf("%s: the refusal does not say the bound: %q", body, message)
		}
	}
	if store.machines() != rosterMaxHosted {
		t.Fatalf("machines = %d, want %d", store.machines(), rosterMaxHosted)
	}
	// An agent its owner runs is no machine of the platform's.
	addEach(t, r, 3, `{"role":"reviewer"}`)
}

// Nothing starts without a baseline, so the agents that do not measure it never take the machine
// one that does needs, and the refusal says what the machines are for.
func TestOneMachineIsKeptForAnAgentThatMeasuresTheBaseline(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, rosterMaxHosted)
	store.pe.CoordinatorHosted = true
	r := agentsRouter(t, store, staff)
	addEach(t, r, rosterMaxHosted-2, `{"hosted":true}`)
	for _, body := range []string{`{"hosted":true}`, `{"hosted":true,"role":"reviewer"}`} {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		message, _ := out["message"].(string)
		if code != http.StatusConflict || out["reason"] != "max_hosted_reached" || !strings.Contains(message, "One is kept for an agent of role baseline") {
			t.Fatalf("%s for the last machine: got = %d %v, want = 409 max_hosted_reached naming the baseline", body, code, out)
		}
	}
	addEach(t, r, 1, `{"hosted":true,"role":"baseline"}`)
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true,"role":"baseline"}`); code != http.StatusConflict || out["reason"] != "max_hosted_reached" {
		t.Fatalf("a second baseline agent beside the first: got = %d %v, want = 409 max_hosted_reached", code, out)
	}
	// The first one finished without a baseline that can be declared: its machine is released,
	// and the next takes its place.
	finished := time.Now()
	store.signups[len(store.signups)-1].FinishedAt = &finished
	addEach(t, r, 1, `{"hosted":true,"role":"baseline"}`)
}

func TestHostingIsRefusedWhileTheLauncherConfigNamesIsNotRegistered(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.unregistered = true
	store.agents["veteran"] = &domain.Agent{ID: "veteran", Name: "Veteran", OrgID: "org-a"}
	r := agentsRouter(t, store, staff)
	for _, body := range []string{`{"hosted":true}`, `{"agent_id":"veteran","hosted":true}`} {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		if code != http.StatusServiceUnavailable || out["reason"] != "launcher_not_registered" {
			t.Fatalf("%s: got = %d %v, want = 503 launcher_not_registered", body, code, out)
		}
		if message, _ := out["message"].(string); !strings.Contains(message, "hl-admin launcher add launcher-main") {
			t.Errorf("%s: the refusal does not say what registers it: %q", body, message)
		}
	}
	if len(store.signups) != 0 {
		t.Fatalf("a refused hosted join left %d signups behind", len(store.signups))
	}
	addEach(t, r, 2, `{}`)
}

// A coordinator's token outlives the close by its wind-down, and adds nothing in it.
func TestACoordinatorAddsNoAgentOnceItsExperimentClosed(t *testing.T) {
	coordinator := auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1"}
	store := newRosterStore(domain.PlatformExpClosed, 5)
	store.lateShare = 1
	r := agentsRouter(t, store, coordinator)
	for _, body := range []string{`{"hosted":true}`, `{"hosted":true,"role":"reviewer"}`, `{}`} {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		if code != http.StatusConflict || out["reason"] != "signup_closed" {
			t.Fatalf("%s: got = %d %v, want = 409 signup_closed", body, code, out)
		}
	}
	if len(store.signups) != 0 || len(store.agents) != 0 {
		t.Fatalf("a coordinator added %d agents", len(store.signups))
	}
}

// A run whose researchers ended while time and quota are left goes on with new ones: its
// coordinator adds a researcher, and nothing of another role, and an agent adds none.
func TestACoordinatorReplacesAResearcherWhileTheRunGoesOn(t *testing.T) {
	coordinator := auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1"}
	store := newRosterStore(domain.PlatformExpRunning, 1)
	store.lateShare = 1
	finished := time.Now()
	store.signups = []domain.AgentSignup{{AgentID: "ended", Role: domain.SignupRoleCompetitor, FinishedAt: &finished}}
	r := agentsRouter(t, store, coordinator)
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true,"role":"reviewer"}`); code != http.StatusConflict || out["reason"] != "signup_closed" {
		t.Fatalf("a reviewer: got = %d %v, want = 409 signup_closed", code, out)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true}`); code != http.StatusCreated {
		t.Fatalf("a researcher: got = %d %v, want = 201", code, out)
	}
	if len(store.signups) != 2 {
		t.Fatalf("signups = %d, want the one that ended and its replacement", len(store.signups))
	}
	agent := agentsRouter(t, store, auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1", AgentID: "ended"})
	if code, out := call(t, agent, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true}`); code == http.StatusCreated {
		t.Fatalf("an agent's token: got = %d %v, want a refusal", code, out)
	}
}

func TestAnAgentIsRemovedOnlyBeforeTheStart(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	agents := addEach(t, r, 2, `{"hosted":true}`)
	first, second := agents[0]["agent_id"].(string), agents[1]["agent_id"].(string)

	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+first, ""); code != http.StatusNoContent || len(out) != 0 {
		t.Fatalf("remove: %d %v, want 204 and no body", code, out)
	}
	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+first, ""); code != http.StatusNotFound || out["reason"] != "not_signed_up" {
		t.Fatalf("remove twice: %d %v", code, out)
	}
	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-none/agents/"+second, ""); code != http.StatusNotFound || out["reason"] != "experiment_not_found" {
		t.Fatalf("remove from no experiment: %d %v", code, out)
	}
	// Its quota goes with it, and a job of it still in flight is charged to that quota.
	store.inFlight = map[string]bool{second: true}
	if code, out := call(t, r, "DELETE", "/platform-experiments/pe-1/agents/"+second, ""); code != http.StatusConflict || out["reason"] != "work_unfinished" {
		t.Fatalf("remove with a job in flight: %d %v, want 409 work_unfinished", code, out)
	}
	store.inFlight = nil
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

// A coordinator once removed the researcher its owner had added, to make the roster match what
// it thought max_agents meant.
func TestACoordinatorRemovesOnlyTheAgentsItAddedItself(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	owner := agentsRouter(t, store, auth.Principal{Kind: auth.KindUser, UserID: "user-1", OrgIDs: []string{"org-a"}})
	coordinator := agentsRouter(t, store, auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1"})
	owners := addEach(t, owner, 1, `{}`)[0]["agent_id"].(string)
	added := addEach(t, coordinator, 1, `{"role":"baseline"}`)[0]

	if actor, _ := added["actor"].(map[string]any); actor["kind"] != "coordinator" || actor["id"] != "pe-1" {
		t.Fatalf("the agent a coordinator added: actor = %v, want the coordinator of pe-1", added["actor"])
	}
	if code, out := call(t, coordinator, "DELETE", "/platform-experiments/pe-1/agents/"+owners, ""); code != http.StatusForbidden || out["reason"] != "agent_not_added_by_coordinator" {
		t.Fatalf("a coordinator removing its owner's agent: got = %d %v, want 403 agent_not_added_by_coordinator", code, out)
	}
	if code, out := call(t, coordinator, "DELETE", "/platform-experiments/pe-1/agents/"+added["agent_id"].(string), ""); code != http.StatusNoContent {
		t.Fatalf("a coordinator removing the agent it added: got = %d %v, want 204", code, out)
	}
	if code, out := call(t, owner, "DELETE", "/platform-experiments/pe-1/agents/"+owners, ""); code != http.StatusNoContent {
		t.Fatalf("the owner removing its own agent: got = %d %v, want 204", code, out)
	}
	if len(store.signups) != 0 {
		t.Fatalf("signups left = %+v, want none", store.signups)
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
			by := tc.caller.Actor()
			store.signups = []domain.AgentSignup{{AgentID: "ag-seeded", PlatformExperimentID: "pe-1", Actor: &by}}
			wantRead, wantRemove := http.StatusOK, http.StatusNoContent
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
	store.agents["outsider"] = &domain.Agent{ID: "outsider", OrgID: "org-b"}
	r := agentsRouter(t, store, auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a"}})
	code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"agent_id":"outsider"}`)
	if code != http.StatusNotFound || len(store.signups) != 0 {
		t.Fatalf("got = %d %v with %d signups, want = 404 and none", code, out, len(store.signups))
	}
}

// Hosting needed a repository to clone, and a person with an idea and no repository could not
// start. The agent now starts in an empty directory and the platform keeps what it pushes.
func TestAHostedAgentJoinsAPlatformExperimentThatNamesNoRepository(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.pe.Repository = ""
	store.agents["veteran"] = &domain.Agent{ID: "veteran", Name: "Veteran", OrgID: "org-a"}
	r := agentsRouter(t, store, staff)
	for _, body := range []string{`{"hosted":true}`, `{"agent_id":"veteran","hosted":true}`} {
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", body)
		if code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("%s: got = %d %v, want it joined", body, code, out)
		}
		if agent := joined(t, out)[0]; agent["hosted"] != true || agent["launcher"] == nil {
			t.Errorf("%s: joined as %v, want hosted with a launcher", body, agent)
		}
	}
	if len(store.signups) != 2 {
		t.Fatalf("%d signups, want both agents", len(store.signups))
	}
}

func TestFlavorAndModelAreResolvedWhenAnAgentJoinsAndStored(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	code, body := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"hosted":true}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, body)
	}
	if a := joined(t, body)[0]; a["flavor"] != "generalist" || a["model"] != "platform-model-1" {
		t.Errorf("an agent that named neither: flavor %v, model %v, want the defaults", a["flavor"], a["model"])
	}
	if s := store.signups[0]; s.Flavor != "generalist" || s.Model != "platform-model-1" {
		t.Errorf("stored signup: flavor %q, model %q", s.Flavor, s.Model)
	}
	code, body = call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"flavor":"data-search","model":"another-model"}`)
	if a := joined(t, body)[0]; code != http.StatusCreated || a["flavor"] != "data-search" || a["model"] != "another-model" {
		t.Errorf("an agent that named both: %d %v", code, a)
	}
}

func TestHyperparametersAboveTheBoundAreRefusedAtJoin(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 200)
	r := agentsRouter(t, store, staff)
	big := `{"hosted":true,"hyperparameters":{"blob":"` + strings.Repeat("x", domain.MaxHyperparametersBytes) + `"}}`
	code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", big)
	if code != http.StatusBadRequest || out["reason"] != "invalid_request" || !strings.Contains(out["message"].(string), "4096") {
		t.Fatalf("got = %d %v, want = 400 naming the limit", code, out)
	}
	if len(store.signups) != 0 {
		t.Fatalf("%d signups were written", len(store.signups))
	}
	fits := `{"hosted":true,"hyperparameters":{"blob":"` + strings.Repeat("x", domain.MaxHyperparametersBytes-len(`{"blob":""}`)) + `"}}`
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", fits); code != http.StatusCreated {
		t.Fatalf("hyperparameters of exactly the limit: got = %d %v", code, out)
	}
}

// A person in two orgs is admitted to both, so the access check passes for an agent of org B and
// an experiment of org A: the store is what keeps the two apart.
func TestARegisteredAgentOfAnotherOrgIsNotFoundEvenForACallerOfBothOrgs(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.agents["outsider"] = &domain.Agent{ID: "outsider", OrgID: "org-b"}
	r := agentsRouter(t, store, auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a", "org-b"}})
	code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"agent_id":"outsider"}`)
	if code != http.StatusNotFound || out["reason"] != "agent_not_found" || len(store.signups) != 0 {
		t.Fatalf("got = %d %v with %d signups, want = 404 agent_not_found and none", code, out, len(store.signups))
	}
}

// Every list showed a created agent by its generated id, which nobody can tell apart.
func TestACreatedAgentIsNamedByItsPlaceUnlessTheCallerNamesIt(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, staff)
	call(t, r, "POST", "/platform-experiments/pe-1/agents", `{}`)
	_, second := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{}`)
	_, named := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"name":"ablation"}`)
	if got := joined(t, second)[0]["name"]; got != "Researcher 2" {
		t.Errorf("the second agent is named %v, want Researcher 2", got)
	}
	if got := joined(t, named)[0]["name"]; got != "ablation" {
		t.Errorf("a stated name became %v", got)
	}
	// A baseline agent and a researcher were both "Researcher 2".
	_, baseline := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"role":"baseline"}`)
	if got := joined(t, baseline)[0]["name"]; got != "Baseline 1" {
		t.Errorf("the baseline agent is named %v, want Baseline 1", got)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents", `{"name":"ablation"}`); code != http.StatusConflict || out["reason"] != "agent_name_taken" {
		t.Errorf("a second agent named ablation: got = %d %v, want 409 agent_name_taken", code, out)
	}
}

// FinishAgent refuses as the store does: an agent finishes in a running platform experiment, and
// one of role baseline also in an open one.
func (f *rosterStore) FinishAgent(_ context.Context, _, agentID, reason string) error {
	for i, signup := range f.signups {
		if signup.AgentID != agentID {
			continue
		}
		if f.pe.Status == domain.PlatformExpRunning || f.pe.Status == domain.PlatformExpOpen && signup.Role == domain.SignupRoleBaseline {
			now := time.Now()
			f.signups[i].FinishedAt, f.signups[i].FinishedReason = &now, reason
			return nil
		}
		return db.ErrSignupClosed
	}
	return db.ErrNotSignedUp
}

func (f *rosterStore) AddDesiredQuotaUsageOne(context.Context, *domain.AgentQuota) error { return nil }

func (f *rosterStore) IsAgentCut(context.Context, string, string) (bool, error) { return false, nil }

func (f *rosterStore) GetAgentRunningExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return nil, nil
}

// In a live run both researchers finished with 11 to 16 minutes of an hour and most of their quota
// left, and the coordinator heard only their reasons. A researcher with more than a tenth of
// both left names its blocker or is told what is left, and every researcher's finish records
// the time and quota it left, which agent.finished carries. No other role is asked.
func TestAResearcherFinishingEarlyNamesItsBlockerAndTheTimeAndQuotaItLeavesAreTold(t *testing.T) {
	newStore := func(role domain.SignupRole, left time.Duration, spent float64) *rosterStore {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.pe.DurationHours, store.pe.EndsAt = 1, time.Now().Add(left)
		store.pe.StartsAt = store.pe.EndsAt.Add(-time.Hour)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: role}}
		store.quota = &domain.AgentQuota{AgentID: "dana", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 3, UsedGuaranteedAccH: spent}
		return store
	}
	finish := func(store *rosterStore, blocker string) (*domain.AgentSignup, error) {
		svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), noUsage(t), 3*time.Minute)
		return svc.FinishAgent(context.Background(), "pe-1", "dana", "nothing beat the control", blocker, "")
	}

	store := newStore(domain.SignupRoleCompetitor, 16*time.Minute+30*time.Second, 0.2)
	_, err := finish(store, "")
	var refused *api.Refusal
	if !errors.As(err, &refused) || refused.Reason != "time_and_quota_left" || refused.GetStatus() != http.StatusConflict ||
		!strings.Contains(refused.Message, "16 of the run's 60 minutes and 2.8 of the agent's 3 AccH were left") || !strings.Contains(refused.Message, "--blocker") ||
		store.signups[0].FinishedAt != nil {
		t.Fatalf("an early finish with no blocker: got %v, want 409 time_and_quota_left saying what is left, and no finish", err)
	}
	finished, err := finish(store, "every job needs a toolchain the environment cannot build")
	if err != nil || !strings.HasPrefix(finished.FinishedReason, "nothing beat the control. Blocker: every job needs a toolchain the environment cannot build. When it finished, 16 of the run's 60 minutes") {
		t.Errorf("an early finish naming its blocker: got %+v, %v, want finished with the reason, the blocker and what it left", finished, err)
	}

	for name, store := range map[string]*rosterStore{
		"little time left":  newStore(domain.SignupRoleCompetitor, 5*time.Minute, 0.2),
		"little quota left": newStore(domain.SignupRoleCompetitor, 30*time.Minute, 2.8),
	} {
		finished, err := finish(store, "")
		if err != nil || !strings.HasPrefix(finished.FinishedReason, "nothing beat the control. When it finished, ") {
			t.Errorf("%s: got %+v, %v, want finished with what it left and no blocker asked", name, finished, err)
		}
	}
	store = newStore(domain.SignupRoleReviewer, 30*time.Minute, 0.2)
	if finished, err := finish(store, ""); err != nil || finished.FinishedReason != "nothing beat the control" {
		t.Errorf("a reviewer: got %+v, %v, want finished with its reason as it gave it", finished, err)
	}
}

// A researcher was refused a finish with 4 minutes of 30 left, more than a tenth, when its
// jobs took 2 to 7 minutes, and sat idle until the close. Less time left than the agent's jobs
// typically take, or with none of its own the experiment's, is too little to ask a blocker for.
func TestAResearcherFinishesWithoutABlockerWhenNoJobOfItFitsInTheTimeLeft(t *testing.T) {
	took := func(agent string, minutes int) *domain.Experiment {
		end := time.Now().Add(-time.Hour)
		return &domain.Experiment{AgentID: agent, Status: domain.StatusCompleted, CreatedAt: end.Add(-time.Duration(minutes) * time.Minute), FinishedAt: &end}
	}
	finish := func(completed ...*domain.Experiment) error {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.pe.DurationHours, store.pe.EndsAt = 0.5, time.Now().Add(4*time.Minute+30*time.Second)
		store.pe.StartsAt = store.pe.EndsAt.Add(-30 * time.Minute)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: domain.SignupRoleCompetitor}}
		store.quota = &domain.AgentQuota{AgentID: "dana", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 3, UsedGuaranteedAccH: 0.2}
		store.completed = completed
		svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), noUsage(t), 3*time.Minute)
		_, err := svc.FinishAgent(context.Background(), "pe-1", "dana", "nothing beat the control", "", "")
		return err
	}
	for name, completed := range map[string][]*domain.Experiment{
		"its own jobs take longer":          {took("dana", 2), took("dana", 6), took("dana", 7), took("ali", 1)},
		"the experiment's jobs take longer": {took("ali", 5), took("ali", 6)},
	} {
		if err := finish(completed...); err != nil {
			t.Errorf("%s: %v, want finished without a blocker", name, err)
		}
	}
	for name, completed := range map[string][]*domain.Experiment{
		"its own jobs are quick": {took("dana", 1), took("dana", 2), took("ali", 20)},
		"no job completed":       nil,
	} {
		var refused *api.Refusal
		if err := finish(completed...); !errors.As(err, &refused) || refused.Reason != "time_and_quota_left" {
			t.Errorf("%s: %v, want 409 time_and_quota_left", name, err)
		}
	}
}

// A baseline agent that finished after its platform experiment closed was answered 409 not_running
// and a paragraph about roles. Its token still works then, to file summaries, and anything else it
// sends is answered experiment_closed. A finish that is taken answers the agent as finished.
func TestAFinishPastTheCloseIsRefusedAsEverythingElseSentThen(t *testing.T) {
	for status, want := range map[domain.PlatformExperimentStatus]string{
		domain.PlatformExpClosed: "experiment_closed", domain.PlatformExpOpen: "not_running", domain.PlatformExpRunning: "",
	} {
		store := newRosterStore(status, 5)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: domain.SignupRoleCompetitor}}
		r := agentsRouter(t, store, auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: "dana", OrgIDs: []string{"org-a"}})
		code, out := call(t, r, "POST", "/platform-experiments/pe-1/agents/dana/finish", `{"reason":"nothing left"}`)
		if want == "" {
			if code != http.StatusOK || joined(t, out)[0]["agent_id"] != "dana" || joined(t, out)[0]["finished_reason"] != "nothing left" {
				t.Errorf("%s: got = %d %v, want = 200 and the agent as finished", status, code, out)
			}
			continue
		}
		if code != http.StatusConflict || out["reason"] != want {
			t.Errorf("%s: got = %d %v, want = 409 %s", status, code, out, want)
		}
		if message, _ := out["message"].(string); status == domain.PlatformExpClosed && message != "platform experiment pe-1 is closed: your run is over and nothing is left to finish" {
			t.Errorf("closed: message %q, want one short sentence", message)
		}
	}
}

// A researcher was briefed to settle every hypothesis before it finishes, and nothing told one
// that had not. The finish answers the hypotheses the agent leaves open, its own and of this
// platform experiment alone, and is taken whatever they are. A graph that does not answer leaves
// the agent unfinished.
func TestAFinishAnswersTheHypothesesTheAgentLeavesOpen(t *testing.T) {
	dana := auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: "dana", OrgIDs: []string{"org-a"}}
	newStore := func() *rosterStore {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: domain.SignupRoleCompetitor}}
		store.hypotheses = []*domain.Hypothesis{
			{ID: "h-open", AgentID: "dana", PlatformExperimentID: "pe-1", Text: "warmup helps", Status: domain.HypothesisOpen},
			{ID: "h-settled", AgentID: "dana", PlatformExperimentID: "pe-1", Text: "depth helps", Status: domain.HypothesisRefuted},
			{ID: "h-another's", AgentID: "eli", PlatformExperimentID: "pe-1", Text: "width helps", Status: domain.HypothesisOpen},
			{ID: "h-elsewhere", AgentID: "dana", PlatformExperimentID: "pe-2", Text: "dropout helps", Status: domain.HypothesisOpen},
		}
		return store
	}
	const finish = "/platform-experiments/pe-1/agents/dana/finish"

	store := newStore()
	code, out := call(t, agentsRouter(t, store, dana), "POST", finish, `{"reason":"quota spent"}`)
	want := []any{map[string]any{"id": "h-open", "text": "warmup helps"}}
	if code != http.StatusOK || !reflect.DeepEqual(out["open_hypotheses"], want) || store.signups[0].FinishedAt == nil {
		t.Errorf("got = %d %v, finished %v, want 200 with the one hypothesis left open and the agent finished", code, out, store.signups[0].FinishedAt)
	}

	store = newStore()
	store.hypotheses = store.hypotheses[1:]
	code, out = call(t, agentsRouter(t, store, dana), "POST", finish, `{"reason":"quota spent"}`)
	if open, listed := out["open_hypotheses"].([]any); code != http.StatusOK || !listed || len(open) != 0 {
		t.Errorf("nothing left open: got = %d %v, want 200 and an empty list", code, out)
	}

	// A person's idea is the verdict of the agent whose completed job tested it, and no other's.
	store = newStore()
	store.hypotheses = append(store.hypotheses,
		&domain.Hypothesis{ID: "h-person", PlatformExperimentID: "pe-1", Text: "a ray predictor helps", Status: domain.HypothesisOpen},
		&domain.Hypothesis{ID: "h-person-untested", PlatformExperimentID: "pe-1", Text: "a lattice helps", Status: domain.HypothesisOpen},
		&domain.Hypothesis{ID: "h-person-by-eli", PlatformExperimentID: "pe-1", Text: "origin refit helps", Status: domain.HypothesisOpen})
	store.completed = []*domain.Experiment{
		{ID: "job-1", AgentID: "dana", PlatformExperimentID: "pe-1", HypothesisID: "h-person", Status: domain.StatusCompleted},
		{ID: "job-2", AgentID: "dana", PlatformExperimentID: "pe-1", HypothesisID: "h-another's", Status: domain.StatusCompleted},
		{ID: "job-3", AgentID: "eli", PlatformExperimentID: "pe-1", HypothesisID: "h-person-by-eli", Status: domain.StatusCompleted},
	}
	code, out = call(t, agentsRouter(t, store, dana), "POST", finish, `{"reason":"quota spent"}`)
	want = []any{map[string]any{"id": "h-open", "text": "warmup helps"}, map[string]any{"id": "h-person", "text": "a ray predictor helps"}}
	if code != http.StatusOK || !reflect.DeepEqual(out["open_hypotheses"], want) {
		t.Errorf("with a person's idea tested: got = %d %v, want 200 with %v", code, out["open_hypotheses"], want)
	}

	store = newStore()
	store.graphErr = errors.New("the graph is away")
	if code, out := call(t, agentsRouter(t, store, dana), "POST", finish, `{"reason":"quota spent"}`); code != http.StatusInternalServerError || store.signups[0].FinishedAt != nil {
		t.Errorf("a graph that does not answer: got = %d %v, finished %v, want 500 and nothing changed", code, out, store.signups[0].FinishedAt)
	}
}

// A researcher of an open platform experiment has no machine asked for and its instance reads
// requested, which a coordinator reported as hosted instances that never launched. Each agent
// that waits for the start says so in the roster, for the reason its platform experiment waits.
func TestTheRosterOfAnOpenPlatformExperimentSaysWhyAnAgentWaits(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	r := agentsRouter(t, store, waitingCoordinator)
	addEach(t, r, 1, `{"hosted":true,"role":"baseline"}`)
	added := addEach(t, r, 1, `{"hosted":true}`)
	addEach(t, r, 1, `{}`)

	reasonOf := func(agent map[string]any) (string, string) {
		why, _ := agent["status_reason"].(map[string]any)
		reason, _ := why["reason"].(string)
		message, _ := why["message"].(string)
		return reason, message
	}
	if reason, message := reasonOf(added[0]); reason != "baseline_measuring" || !strings.HasPrefix(message, "its machine is asked for when the platform starts the run, and none is before: ") {
		t.Errorf("the researcher as it was added: status_reason %q %q, want baseline_measuring and that no machine is asked for yet", reason, message)
	}
	code, out := call(t, r, "GET", "/platform-experiments/pe-1/agents", "")
	if code != http.StatusOK {
		t.Fatalf("list: got = %d %v", code, out)
	}
	roster := joined(t, out)
	if _, waits := roster[0]["status_reason"]; waits {
		t.Errorf("the baseline agent, whose machine starts at once, carries status_reason: %v", roster[0])
	}
	for i, begins := range map[int]string{1: "its machine is asked for when the platform starts the run", 2: "it takes its first turn when the platform starts the run"} {
		reason, message := reasonOf(roster[i])
		if reason != "baseline_measuring" || !strings.HasPrefix(message, begins) || !strings.Contains(message, "the platform declares its COMPLETED job and starts the run") {
			t.Errorf("agent %d: status_reason %q %q, want baseline_measuring beginning %q", i, reason, message, begins)
		}
	}

	store.pe.Status = domain.PlatformExpRunning
	_, out = call(t, r, "GET", "/platform-experiments/pe-1/agents", "")
	for _, agent := range joined(t, out) {
		if _, waits := agent["status_reason"]; waits {
			t.Errorf("an agent of a running platform experiment carries status_reason: %v", agent)
		}
	}
}
