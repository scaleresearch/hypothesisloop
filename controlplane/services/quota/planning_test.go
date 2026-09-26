package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// planningAPI is the planning routes and the ones a draft passes through, over store.
func planningAPI(t *testing.T, store PlatformExperimentsStore, caller auth.Principal) (*PlatformExperimentsService, func(method, path string, body any) (int, map[string]any)) {
	t.Helper()
	svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{"pe-draft": {PlatformExperimentID: "pe-draft", ProjectID: "prj-a", OrgID: "org-a"}},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(svc, &fakeRepositories{}, zap.NewNop()).
		WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "platform-model-1", SigningSecret: agentsTestSecret, MaxHosted: hostingMaxHosted}).
		WithCatalog(ResourceCatalog{AcceleratorTypes: []AcceleratorTypeInfo{{Name: "rtx-4090", AccHRate: 1}}})
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), handler)
	RegisterPlanningHuma(doc, handler)
	return svc, func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&payload).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := authtest.Authorize(httptest.NewRequest(method, path, &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

var planner = auth.Principal{Kind: auth.KindUser, UserID: "u-1", Name: "Ada", OrgIDs: []string{"org-a"}}

// A person with an idea had to state metrics, a budget, a ladder and dates before anything
// existed. A draft is the experiment made from the idea alone.
func TestADraftIsCreatedFromOneSentenceWithThePlanningAllowanceAndNothingElse(t *testing.T) {
	store := &draftStore{}
	_, call := planningAPI(t, store, planner)
	goal := "Find out whether a cosine schedule beats a constant learning rate for a small transformer on character-level text, measured by validation loss"
	before := time.Now()
	code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": "  " + goal + "\n"})
	if code != http.StatusCreated || len(store.created) != 1 {
		t.Fatalf("got = %d %v with %d created, want 201 and one", code, out, len(store.created))
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpDraft || pe.Goal != goal || out["status"] != "draft" || out["goal"] != goal {
		t.Fatalf("created %+v", pe)
	}
	if pe.BudgetAcceleratorHours != DefaultPlanning.AllowanceAcceleratorHours || len(pe.Metrics) != 0 || pe.MaxAgents != 1 || pe.CoordinatorHosted {
		t.Errorf("a draft holds %v AccH, %d metrics, %d places, coordinator %v: want the allowance and nothing of a run", pe.BudgetAcceleratorHours, len(pe.Metrics), pe.MaxAgents, pe.CoordinatorHosted)
	}
	if limit := pe.EndsAt.Sub(before); limit < DefaultPlanning.Limit-time.Minute || limit > DefaultPlanning.Limit+time.Minute {
		t.Errorf("a draft ends %v after it was created, want the planning limit", limit)
	}
	if !strings.HasSuffix(pe.Name, "…") || len([]rune(pe.Name)) > draftNameLength+1 || !strings.HasPrefix(goal, strings.TrimSuffix(pe.Name, "…")) {
		t.Errorf("name = %q, want the goal cut at a word", pe.Name)
	}
	if pe.CreatedBy == nil || *pe.CreatedBy != (domain.Actor{Kind: domain.ActorUser, ID: "u-1", Name: "Ada"}) {
		t.Errorf("created_by = %+v, want the person who asked", pe.CreatedBy)
	}
	// Its one agent is the planner, which the platform runs and never ranks or cuts.
	planner, signup := store.planners[pe.ID], store.signups[pe.ID]
	if planner == nil || planner.Name != "Planner" || signup.AgentID != planner.ID || !signup.Hosted || signup.Launcher != "main" ||
		signup.Flavor != domain.PlannerFlavor || signup.Role != domain.SignupRoleBaseline || signup.Model != "platform-model-1" || out["signup_count"] != float64(1) {
		t.Errorf("planner %+v signed up as %+v, want a hosted planner of role baseline on the platform's model", planner, signup)
	}

	for name, body := range map[string]map[string]any{
		"no goal":                {"project_id": "prj-a", "goal": "   "},
		"a goal that is a paper": {"project_id": "prj-a", "goal": strings.Repeat("x", domain.MaxGoalBytes+1)},
	} {
		if code, out := call("POST", "/platform-experiments/drafts", body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got = %d %v, want 422", name, code, out)
		}
	}
	if code, _ := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-b", "goal": goal}); code != http.StatusNotFound {
		t.Errorf("another org's project: got = %d, want 404", code)
	}
	if len(store.created) != 1 {
		t.Fatalf("a refused draft was created: %d", len(store.created))
	}

	// Nobody asks for a planner: it is no flavor a request can name.
	open := &rosterStore{pe: &domain.PlatformExperiment{ID: "pe-1", ProjectID: "prj-a", Status: domain.PlatformExpOpen, MaxAgents: 5, MaxHosted: rosterMaxHosted, Repository: "acme/model"}, agents: map[string]*domain.Agent{}}
	if code, body := call2(t, agentsRouter(t, open, staff), "POST", "/platform-experiments/pe-1/agents", `{"hosted":true,"flavor":"planner"}`); code != http.StatusBadRequest || len(open.signups) != 0 {
		t.Errorf("an agent asked for as a planner: got = %d %v, want 400 and nobody joined", code, body)
	}

	// With no launcher registered a planner would never start, so no draft is made.
	store.unregistered = true
	if code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": goal}); code != http.StatusServiceUnavailable || out["reason"] != "launcher_not_registered" || len(store.created) != 1 {
		t.Errorf("no launcher: got = %d %v with %d created, want 503 launcher_not_registered", code, out, len(store.created))
	}
}

// call2 is the agents tests' call under a name that does not shadow this file's.
func call2(t *testing.T, r http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	return call(t, r, method, path, body)
}

func TestADraftIsAbandonedByClosingItAndByItsPlanningLimit(t *testing.T) {
	now := time.Now().UTC()
	draft := func(id string, ends time.Time) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", EndsAt: ends, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &draftStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		draft("pe-planning", now.Add(time.Hour)), draft("pe-late", now.Add(-time.Minute)), draft("pe-dropped", now.Add(time.Hour)),
	}}}}
	svc, _ := planningAPI(t, store, planner)
	// Closing a draft derives no standings: it never ran and has no metric to rank on.
	if err := svc.Close(context.Background(), "pe-dropped", nil); err != nil {
		t.Fatalf("closing a draft: %v", err)
	}
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := map[string]domain.PlatformExperimentStatus{}
	for _, pe := range store.created {
		status[pe.ID] = pe.Status
	}
	if status["pe-planning"] != domain.PlatformExpDraft || status["pe-late"] != domain.PlatformExpClosed || status["pe-dropped"] != domain.PlatformExpClosed {
		t.Fatalf("statuses = %v, want the draft past its limit and the dropped one closed", status)
	}
}

// statusStore is createdStore with the reads and the write a close and a sweep make.
type statusStore struct{ createdStore }

func (s *statusStore) ListPlatformExperimentsByStatus(_ context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	var out []*domain.PlatformExperiment
	for _, pe := range s.created {
		if pe.Status == status {
			out = append(out, pe)
		}
	}
	return out, nil
}

func (s *statusStore) UpdatePlatformExperimentStatus(_ context.Context, id string, status domain.PlatformExperimentStatus) error {
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status = status
		}
	}
	return nil
}

// draftStore is statusStore with a draft's own write: the experiment and its planner together.
type draftStore struct {
	statusStore
	planners map[string]*domain.Agent
	signups  map[string]domain.AgentSignup
	// unregistered is a launcher config names and no operator registered.
	unregistered bool

	acceptedAgents  []*domain.Agent
	acceptedSignups []domain.AgentSignup
}

func (s *draftStore) LauncherKnown(context.Context, string) (bool, error) {
	return !s.unregistered, nil
}

func (s *draftStore) CreateDraft(_ context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup) error {
	if s.planners == nil {
		s.planners, s.signups = map[string]*domain.Agent{}, map[string]domain.AgentSignup{}
	}
	s.created = append(s.created, pe)
	s.planners[pe.ID], s.signups[pe.ID] = planner, signup
	return nil
}

func (s *draftStore) GetAgentSignup(_ context.Context, platformExperimentID, agentID string) (*domain.AgentSignup, error) {
	if signup, ok := s.signups[platformExperimentID]; ok && signup.AgentID == agentID {
		return &signup, nil
	}
	if agentID == "ag-researcher" {
		return &domain.AgentSignup{AgentID: agentID, Flavor: domain.DefaultAgentFlavor}, nil
	}
	return nil, nil
}

func (s *draftStore) SetProposal(_ context.Context, id string, proposal []byte, waitUntil time.Time) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpDraft {
			now := time.Now().UTC()
			pe.Proposal, pe.ProposedAt, pe.EndsAt = proposal, &now, waitUntil
			return nil
		}
	}
	return db.ErrNotDraft
}

// aProposal is one the check takes, with more laid over it.
func aProposal(more map[string]any) map[string]any {
	body := map[string]any{
		"experiment": map[string]any{
			"name": "Cosine or constant", "description": "Train the small transformer in train.py and report val_loss.",
			"metrics":                 []map[string]any{{"key": "val_loss", "direction": "minimize"}},
			"report_interval_seconds": 30,
		},
		"budget_accelerator_hours": 6, "duration_hours": 4, "agents": []map[string]any{{"flavor": "hyperparameter-search"}, {}},
		"baseline_job_id": "job-1", "accelerator_type": "rtx-4090",
		"metric_reason": "Validation loss on held-out text is the goal itself.",
		"measurement":   "bench.py trains for a fixed number of steps and scores a split the training never reads.",
		"questions":     []string{"Is 4 hours enough?"},
	}
	for key, value := range more {
		body[key] = value
	}
	return body
}

func draftWithPlanner() *draftStore {
	return &draftStore{
		statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{{
			ID: "pe-draft", ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", MaxHosted: hostingMaxHosted, EndsAt: time.Now().Add(time.Minute),
		}}}},
		signups: map[string]domain.AgentSignup{"pe-draft": {AgentID: "ag-planner", Flavor: domain.PlannerFlavor, Hosted: true}},
	}
}

var thePlanner = auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-planner", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}

// The planner's proposal is the only way a draft gets metrics, a brief and a cost, and a person
// accepts it unread more often than not: what can be checked is checked before it is stored.
func TestAProposalIsStoredOnlyFromThePlannerAndOnlyWhenItHoldsTogether(t *testing.T) {
	store := draftWithPlanner()
	_, call := planningAPI(t, store, thePlanner)
	before := time.Now()
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil))
	if code != http.StatusOK {
		t.Fatalf("got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	var stored Proposal
	if err := json.Unmarshal(pe.Proposal, &stored); err != nil || stored.BudgetAcceleratorHours != 6 || len(stored.Agents) != 2 || stored.BaselineJobID != "job-1" {
		t.Fatalf("stored %s, %v", pe.Proposal, err)
	}
	if pe.ProposedAt == nil || out["proposed_at"] == nil || out["proposal"] == nil || pe.Status != domain.PlatformExpDraft {
		t.Errorf("the draft is not marked proposed: %+v", pe)
	}
	// From now on it waits for a person, far longer than the planner was given.
	if wait := pe.EndsAt.Sub(before); wait < DefaultPlanning.ProposedFor-time.Minute {
		t.Errorf("a proposed draft ends in %v, want %v", wait, DefaultPlanning.ProposedFor)
	}
	// The draft itself is unchanged: nothing of a proposal takes effect until a person accepts.
	if pe.Name != "" || len(pe.Metrics) != 0 || pe.BudgetAcceleratorHours != 0 {
		t.Errorf("the proposal changed the draft before it was accepted: %+v", pe)
	}

	experiment := func(more map[string]any) map[string]any {
		e := aProposal(nil)["experiment"].(map[string]any)
		for key, value := range more {
			e[key] = value
		}
		return map[string]any{"experiment": e}
	}
	for name, c := range map[string]struct {
		more   map[string]any
		reason string
	}{
		"an experiment that sets its own budget": {experiment(map[string]any{"budget": 3}), "invalid_experiment"},
		"no brief":                               {experiment(map[string]any{"description": " "}), "invalid_experiment"},
		"no ranked metric":                       {experiment(map[string]any{"metrics": []map[string]any{{"key": "val_loss", "direction": "minimize", "role": "constraint", "bound": 1}}}), "invalid_metrics"},
		"a ladder that is not one":               {experiment(map[string]any{"stages": []map[string]any{{"length_pct": 40, "evict_pct": 50}}}), "invalid_stages"},
		"no cost":                                {map[string]any{"budget_accelerator_hours": 0}, "invalid_cost"},
		"nobody to run it":                       {map[string]any{"agents": []map[string]any{}}, "invalid_agents"},
		"more machines than it gets":             {map[string]any{"agents": []map[string]any{{}, {}, {}, {}}}, "invalid_agents"},
		"a planner among the agents":             {map[string]any{"agents": []map[string]any{{"flavor": "planner"}}}, "invalid_agents"},
		"a type the platform has not":            {map[string]any{"accelerator_type": "h100"}, "unknown_accelerator_type"},
		"no reason for the metric":               {map[string]any{"metric_reason": ""}, "missing_explanation"},
		"a measurement that is a paper":          {map[string]any{"measurement": strings.Repeat("x", maxProposalTextBytes+1)}, "too_long"},
	} {
		store := draftWithPlanner()
		_, call := planningAPI(t, store, thePlanner)
		code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(c.more))
		if code != http.StatusUnprocessableEntity || out["reason"] != c.reason || store.created[0].ProposedAt != nil {
			t.Errorf("%s: got = %d %v, want 422 %s and nothing stored", name, code, out, c.reason)
		}
	}

	for name, caller := range map[string]auth.Principal{
		"a researcher of it":   {Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-researcher", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
		"the person who asked": planner,
	} {
		store := draftWithPlanner()
		_, call := planningAPI(t, store, caller)
		if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusForbidden || store.created[0].ProposedAt != nil {
			t.Errorf("%s: got = %d %v, want 403", name, code, out)
		}
	}

	accepted := draftWithPlanner()
	accepted.created[0].Status = domain.PlatformExpOpen
	_, call = planningAPI(t, accepted, thePlanner)
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusConflict || out["reason"] != "not_a_draft" {
		t.Errorf("proposing what is no draft: got = %d %v, want 409 not_a_draft", code, out)
	}
}

func (s *draftStore) AcceptDraft(_ context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	for i, stored := range s.created {
		if stored.ID != pe.ID {
			continue
		}
		if stored.Status != domain.PlatformExpDraft {
			return db.ErrNotDraft
		}
		if stored.ProposedAt == nil {
			return db.ErrNothingProposed
		}
		accepted := *pe
		accepted.Status, accepted.StartsAt = domain.PlatformExpOpen, time.Now().UTC()
		s.created[i] = &accepted
		s.acceptedAgents, s.acceptedSignups = create, signups
		return nil
	}
	return db.ErrNotDraft
}

// A person accepts in one call, and what the run costs is what they state: the proposal's own
// numbers were written with an agent's token.
func TestAcceptingADraftOpensTheProposedExperimentAtTheCostThePersonStates(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningAPI(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	_, call := planningAPI(t, store, planner)
	before := time.Now()
	code, out := call("POST", "/platform-experiments/pe-draft/accept", map[string]any{
		"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{"flavor": "architecture-search", "hyperparameters": map[string]any{"depth": 4}}, {}, {}},
	})
	if code != http.StatusOK {
		t.Fatalf("accept: got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpOpen || out["status"] != "open" || pe.Name != "Cosine or constant" || !strings.HasPrefix(pe.Description, "Train the small transformer") ||
		len(pe.Metrics) != 1 || pe.Metrics[0].Key != "val_loss" || pe.ReportIntervalSeconds != 30 || len(pe.Stages) != 1 || pe.Goal != "g" {
		t.Fatalf("accepted %+v, want the proposal's experiment", pe)
	}
	if pe.BudgetAcceleratorHours != 3 || pe.MaxAgents != 3 {
		t.Errorf("budget %v for %d agents, want what the person stated (3 and 3), not the proposal's 6 and 2", pe.BudgetAcceleratorHours, pe.MaxAgents)
	}
	if length := pe.EndsAt.Sub(before); length < 2*time.Hour-time.Minute || length > 2*time.Hour+time.Minute {
		t.Errorf("the run lasts %v, want 2h", length)
	}
	if !pe.CoordinatorHosted || pe.Coordinator == nil || pe.Coordinator.Launcher != "main" || pe.Coordinator.Model != "platform-model-1" {
		t.Errorf("coordinator %+v, want one the platform hosts", pe.Coordinator)
	}
	if len(store.acceptedAgents) != 3 || len(store.acceptedSignups) != 3 {
		t.Fatalf("signed up %d agents with %d signups, want 3", len(store.acceptedAgents), len(store.acceptedSignups))
	}
	for i, signup := range store.acceptedSignups {
		agent := store.acceptedAgents[i]
		if agent.Name != "Researcher "+string(rune('1'+i)) || signup.AgentID != agent.ID || !signup.Hosted || signup.Launcher != "main" || signup.Role != domain.SignupRoleCompetitor || signup.Model != "platform-model-1" {
			t.Errorf("researcher %d: %+v signed up as %+v", i, agent, signup)
		}
	}
	if first := store.acceptedSignups[0]; first.Flavor != "architecture-search" || first.Hyperparameters["depth"] != float64(4) || store.acceptedSignups[1].Flavor != domain.DefaultAgentFlavor {
		t.Errorf("signups %+v, want the flavors and hyperparameters the person stated", store.acceptedSignups)
	}

	// Accepting is refused by state, so a second press and a lost answer do no harm.
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}}}); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Errorf("accepting twice: got = %d %v, want 409 invalid_transition", code, out)
	}

	unproposed := draftWithPlanner()
	_, call = planningAPI(t, unproposed, planner)
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}}}); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Errorf("accepting before anything is proposed: got = %d %v, want 409 invalid_transition", code, out)
	}

	for name, c := range map[string]struct {
		body   map[string]any
		reason string
	}{
		"no budget":         {map[string]any{"budget_accelerator_hours": 0, "duration_hours": 2, "agents": []map[string]any{{}}}, "invalid_cost"},
		"nobody":            {map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{}}, "invalid_agents"},
		"too many machines": {map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}, {}, {}, {}}}, "invalid_agents"},
	} {
		store := draftWithPlanner()
		_, propose := planningAPI(t, store, thePlanner)
		propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil))
		_, call := planningAPI(t, store, planner)
		if code, out := call("POST", "/platform-experiments/pe-draft/accept", c.body); code != http.StatusUnprocessableEntity || out["reason"] != c.reason || store.created[0].Status != domain.PlatformExpDraft {
			t.Errorf("%s: got = %d %v, want 422 %s and still a draft", name, code, out, c.reason)
		}
	}

	// The planner proposes and a person accepts: its own token is of no kind that may.
	store = draftWithPlanner()
	_, propose = planningAPI(t, store, thePlanner)
	propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil))
	if code, _ := propose("POST", "/platform-experiments/pe-draft/accept", map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}}}); code != http.StatusForbidden || store.created[0].Status != domain.PlatformExpDraft {
		t.Errorf("the planner accepting its own proposal: got = %d, want 403", code)
	}
}
