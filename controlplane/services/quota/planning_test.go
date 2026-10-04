package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/cli/repotemplate"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// planningAPI is the planning routes and the ones a draft passes through, over store.
func planningAPI(t *testing.T, store PlatformExperimentsStore, caller auth.Principal) (*PlatformExperimentsService, func(method, path string, body any) (int, map[string]any)) {
	t.Helper()
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{"pe-draft": {PlatformExperimentID: "pe-draft", ProjectID: "prj-a", OrgID: "org-a"}},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
	planningRepositories = &fakeRepositories{files: map[string]string{"prj-a": ""}}
	svc.WithRepositories(planningRepositories)
	handler := NewPlatformExperimentsHandler(svc, planningRepositories, testPlanning, zap.NewNop()).
		WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "platform-model-1", SigningSecret: agentsTestSecret, MaxHosted: hostingMaxHosted}).
		WithJobImages([]domain.JobImage{{Name: "torch", Image: "registry.example.com/hl/torch:1"}})
	if drafts, ok := store.(*draftStore); ok {
		svc.reachable = func(context.Context, string) (map[string][]domain.Cluster, error) { return drafts.reachable, nil }
	}
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
// planningRepositories is the git host as the last planningAPI sees it.
var planningRepositories *fakeRepositories

func TestADraftIsCreatedFromOneSentenceWithItsPlannerAndNothingOfARun(t *testing.T) {
	store := &draftStore{}
	svc, call := planningAPI(t, store, planner)
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
	if pe.BudgetAcceleratorHours != 0 || len(pe.Metrics) != 0 || pe.CoordinatorHosted {
		t.Errorf("a draft holds %v AccH, %d metrics, coordinator %v: want no budget and nothing of a run", pe.BudgetAcceleratorHours, len(pe.Metrics), pe.CoordinatorHosted)
	}
	// A planner read a made-up max_agents of 1 as a cap and proposed one researcher.
	if most := maxProposedResearchers(hostingMaxHosted); pe.MaxAgents != most || out["max_agents"] != float64(most) {
		t.Errorf("a draft reads max_agents %d (%v answered), want %d: the most researchers a proposal is accepted with", pe.MaxAgents, out["max_agents"], most)
	}
	if limit := pe.PlanningEndsAt.Sub(before); limit < testPlanning.Limit-time.Minute || limit > testPlanning.Limit+time.Minute {
		t.Errorf("a draft ends %v after it was created, want the planning limit", limit)
	}
	if out["planning_ends_at"] != pe.PlanningEndsAt.Format(time.RFC3339Nano) || out["ends_at"] != nil || out["proposal_expires_at"] != nil {
		t.Errorf("a draft being planned reads planning_ends_at %v, ends_at %v, proposal_expires_at %v: want its planning limit under that name alone", out["planning_ends_at"], out["ends_at"], out["proposal_expires_at"])
	}
	if !strings.HasSuffix(pe.Name, "…") || len([]rune(pe.Name)) > draftNameLength+1 || !strings.HasPrefix(goal, strings.TrimSuffix(pe.Name, "…")) {
		t.Errorf("name = %q, want the goal cut at a word", pe.Name)
	}
	if pe.Actor == nil || *pe.Actor != (domain.Actor{Kind: domain.ActorUser, ID: "u-1", Name: "Ada"}) {
		t.Errorf("actor = %+v, want the person who asked", pe.Actor)
	}
	// Its one agent is the planner, which the platform runs and never ranks or cuts.
	planner, signup := store.planners[pe.ID], store.signups[pe.ID]
	if planner == nil || planner.Name != "Planner" || signup.AgentID != planner.ID || !signup.Hosted || signup.Launcher != "main" ||
		signup.Flavor != domain.PlannerFlavor || signup.Role != domain.SignupRolePlanner || signup.Model != "platform-model-1" || out["signup_count"] != float64(1) {
		t.Errorf("planner %+v signed up as %+v, want a hosted planner of role baseline on the platform's model", planner, signup)
	}
	// A draft has a repository: the one named, or a new one the repository pass makes after the
	// create answered.
	if pe.Repository != "" || len(planningRepositories.created) != 0 || out["status_reason"].(map[string]any)["reason"] != "repository_creating" {
		t.Errorf("repository = %q (created %v), status_reason %v: want none yet and repository_creating", pe.Repository, planningRepositories.created, out["status_reason"])
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pe.Repository != "acme/"+RepositoryName(pe) || pe.RepositoryURL != "https://github.com/"+pe.Repository+".git" || len(planningRepositories.created) != 1 ||
		!maps.Equal(planningRepositories.committed, repotemplate.Files()) || planningRepositories.committed[domain.ExperimentFile] == "" {
		t.Errorf("repository = %q (created %v), want one made for the draft from the template, which holds %s", pe.Repository, planningRepositories.created, domain.ExperimentFile)
	}
	if code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": goal, "repository": "acme/model"}); code != http.StatusCreated || out["repository"] != testRepository || len(planningRepositories.created) != 1 {
		t.Errorf("naming a repository: got = %d %v, want it as GitHub spells it and none created", code, out["repository"])
	}
	store.created = store.created[:1]

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

// "Plan it" froze for seven seconds while GitHub created the draft's repository inside the
// request. The create answers once the draft is stored, and a repository GitHub then refuses
// closes the draft with what GitHub said.
func TestADraftIsAnsweredBeforeItsRepositoryAndARefusedOneClosesIt(t *testing.T) {
	store := &draftStore{}
	svc, call := planningAPI(t, store, planner)
	planningRepositories.notCreated = api.Refuse(http.StatusConflict, "repository_exists", "the account already has a repository of that name")
	code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": "Find out whether warmup helps"})
	if code != http.StatusCreated || out["status_reason"].(map[string]any)["reason"] != "repository_creating" {
		t.Fatalf("got = %d %v, want 201 with the repository still being created", code, out)
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	pe := store.created[0]
	if err := svc.Describe(context.Background(), pe, planner); err != nil || pe.Status != domain.PlatformExpClosed || pe.StatusReason.Reason != string(domain.CloseRepositoryNotCreated) ||
		!strings.Contains(pe.StatusReason.Message, "already has a repository of that name") {
		t.Fatalf("after the refusal: %s %+v, %v, want it closed repository_not_created with GitHub's answer", pe.Status, pe.StatusReason, err)
	}
	// Nothing is created again for a draft that is closed.
	planningRepositories.notCreated = nil
	if err := svc.ProvideRepositories(context.Background()); err != nil || len(planningRepositories.created) != 0 {
		t.Fatalf("a second pass created %v, %v: want nothing", planningRepositories.created, err)
	}
}

// A draft read "being planned" for half an hour while no launcher ran to start its planner. A
// machine the platform wants of a launcher that has stopped asking says so, until it asks again.
func TestADraftWhosePlannersLauncherIsSilentSaysItWaitsForTheLauncher(t *testing.T) {
	store := draftWithPlanner()
	store.created[0].Repository = "acme/model"
	planner := store.signups["pe-draft"]
	planner.Launcher = "main"
	store.signups["pe-draft"] = planner
	svc, _ := planningAPI(t, plannerListed{store}, thePlanner)
	pe := store.created[0]

	if err := svc.Describe(context.Background(), pe, auth.Principal{Kind: auth.KindUser}); err != nil || pe.StatusReason.Reason != "planning" {
		t.Fatalf("with its launcher asking: %+v, %v, want planning", pe.StatusReason, err)
	}
	long := time.Now().Add(-10 * time.Minute)
	store.answered = map[string]*time.Time{"main": &long}
	if err := svc.Describe(context.Background(), pe, auth.Principal{Kind: auth.KindUser}); err != nil || pe.StatusReason.Reason != "launcher_unreachable" ||
		pe.StatusReason.WaitsFor != domain.WaitsForPlatform || !strings.Contains(pe.StatusReason.Message, "launcher main last asked for its machines 10m0s ago") {
		t.Fatalf("with its launcher silent: %+v, %v, want launcher_unreachable naming main and how long", pe.StatusReason, err)
	}
	store.answered = map[string]*time.Time{"main": nil}
	if err := svc.Describe(context.Background(), pe, auth.Principal{Kind: auth.KindUser}); err != nil || !strings.Contains(pe.StatusReason.Message, "launcher main has not asked") {
		t.Fatalf("with a launcher that never asked: %+v, %v", pe.StatusReason, err)
	}
	// A proposal waits for a person, and no machine is wanted for it.
	now := time.Now()
	pe.SetAnswered(&now, true)
	if err := svc.Describe(context.Background(), pe, auth.Principal{Kind: auth.KindUser}); err != nil || pe.StatusReason.Reason != "proposed" {
		t.Fatalf("a proposed draft with its launcher silent: %+v, %v, want proposed", pe.StatusReason, err)
	}
}

// plannerListed is a draftStore whose roster is the draft's planner.
type plannerListed struct{ *draftStore }

func (s plannerListed) ListAgentSignups(_ context.Context, id string) ([]domain.AgentSignup, error) {
	return []domain.AgentSignup{s.signups[id]}, nil
}

// draftRepositories answers the git credential service from the drafts a store holds.
type draftRepositories struct{ store *draftStore }

func (d draftRepositories) PlatformExperimentRepository(ctx context.Context, id string) (string, string, error) {
	pe, err := d.store.GetPlatformExperiment(ctx, id)
	if err != nil || pe == nil {
		return "", "", db.ErrPlatformExperimentNotFound
	}
	return pe.ProjectID, pe.Repository, nil
}

// A planner clones its draft's repository before it proposes, so the token of a planner at work
// is handed the git credential of that repository, and of no other.
func TestThePlannerOfADraftIsGivenAGitCredentialForItsRepository(t *testing.T) {
	store := &draftStore{}
	svc, call := planningAPI(t, store, planner)
	if code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": "Find out whether warmup helps"}); code != http.StatusCreated {
		t.Fatalf("got = %d %v, want a draft", code, out)
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	pe := store.created[0]
	host, err := githost.NewPlain("https://git.test", "platform", "secret")
	if err != nil {
		t.Fatal(err)
	}
	atWork := authtest.As{Kind: auth.KindAgent, PlatformExperimentID: pe.ID, AgentID: store.planners[pe.ID].ID, ProjectID: pe.ProjectID, OrgIDs: []string{"org-a"}}
	access := auth.NewAccess(authtest.Placement{})
	router := chi.NewRouter()
	accounts.RegisterRepositoriesHuma(apidocs.New(router, "test", "1.0.0", "", atWork, access, zap.NewNop()),
		accounts.NewRepositories(struct{ accounts.InstallationStore }{}, draftRepositories{store}, nil, host, access, zap.NewNop()))
	for repository, want := range map[string]int{pe.Repository: http.StatusCreated, "acme/another": http.StatusNotFound} {
		code, out := call2(t, router, "POST", "/git-credentials", `{"protocol":"https","host":"git.test","path":"`+repository+`.git"}`)
		if code != want || (want == http.StatusCreated && (out["username"] != "platform" || out["password"] != "secret")) {
			t.Errorf("a credential for %s: got = %d %v, want %d", repository, code, out, want)
		}
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
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", PlanningEndsAt: ends, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &draftStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		draft("pe-planning", now.Add(time.Hour)), draft("pe-late", now.Add(-time.Minute)), draft("pe-dropped", now.Add(time.Hour)),
	}}}}
	svc, _ := planningAPI(t, store, planner)
	// Closing a draft derives no standings: it never ran and has no metric to rank on.
	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	if _, err := svc.Close(context.Background(), "pe-dropped", &owner, nil); err != nil {
		t.Fatalf("closing a draft: %v", err)
	}
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := map[string]domain.PlatformExperimentStatus{}
	closedBy := map[string]*domain.Actor{}
	for _, pe := range store.created {
		status[pe.ID], closedBy[pe.ID] = pe.Status, pe.ClosedBy
	}
	// Who closed it is what tells a person's close from the end time: a coordinator once
	// reported "closed by owner" for a run that had simply reached its end.
	if closedBy["pe-late"] != nil || closedBy["pe-dropped"] == nil || *closedBy["pe-dropped"] != owner {
		t.Fatalf("closed by = %v, want nobody for the one past its limit and the caller for the dropped one", closedBy)
	}
	if status["pe-planning"] != domain.PlatformExpDraft || status["pe-late"] != domain.PlatformExpClosed || status["pe-dropped"] != domain.PlatformExpClosed {
		t.Fatalf("statuses = %v, want the draft past its limit and the dropped one closed", status)
	}
}

// An accepted draft waited for a baseline nobody could measure, with its coordinator's machine
// rented until the run's end time, and a run of one hour closed end_time_reached while its
// baseline was still being prepared. An open experiment has until its baseline_due_at to be
// running and is closed then, with a baseline or without. The run's length is no part of it.
func TestAnOpenExperimentNotRunningByItsBaselineDeadlineIsClosed(t *testing.T) {
	now := time.Now().UTC()
	open := func(id string, due time.Time, baseline string) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-48 * time.Hour),
			DurationHours: 1, BaselineDueAt: due, BaselineJobID: baseline, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		open("pe-late", now.Add(-time.Minute), ""),
		open("pe-declared-late", now.Add(-time.Minute), "job-1"),
		open("pe-in-time", now.Add(time.Minute), ""),
		open("pe-declared-in-time", now.Add(time.Minute), "job-1"),
	}}}
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, pe := range store.created {
		want := domain.PlatformExpOpen
		if strings.HasSuffix(pe.ID, "-late") {
			want = domain.PlatformExpClosed
		}
		if pe.Status != want || (want == domain.PlatformExpClosed && pe.ClosedBy != nil) {
			t.Errorf("%s: status %s closed by %v, want %s by the platform", pe.ID, pe.Status, pe.ClosedBy, want)
		}
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

func (s *statusStore) ClosePlatformExperiment(_ context.Context, id string, by *domain.Actor) error {
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status, pe.ClosedBy = domain.PlatformExpClosed, by
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

	// reachable are the accelerator types the org's jobs can be placed on, each with the
	// clusters that have it.
	reachable map[string][]domain.Cluster

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
	for _, signup := range s.baselines {
		if signup.AgentID == agentID {
			return &signup, nil
		}
	}
	if agentID == "ag-researcher" {
		return &domain.AgentSignup{AgentID: agentID, Flavor: domain.DefaultAgentFlavor}, nil
	}
	return nil, nil
}

func (s *draftStore) SetProposal(_ context.Context, id string, proposal []byte, waitUntil time.Time, keepPlanner time.Duration) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpDraft {
			now := time.Now().UTC()
			kept := now.Add(keepPlanner)
			pe.Proposal, pe.DeclinedReason, pe.PlannerEndsAt = proposal, "", &kept
			pe.SetAnswered(&now, true)
			pe.SetClosesAt(waitUntil)
			return nil
		}
	}
	return db.ErrNotDraft
}

// aProposal is one the check takes, with more laid over it.
func aProposal(more map[string]any) map[string]any {
	body := map[string]any{
		"experiment": map[string]any{
			"name": "Cosine or constant", "description": "Train the small transformer in train.py and report val_loss. BASELINE config: constant lr 3e-4 measured: not yet established",
			"metrics": []map[string]any{{"key": "val_loss", "direction": "minimize"}},
		},
		"budget_accelerator_hours": 6, "duration_hours": 4, "agents": []map[string]any{{"flavor": "hyperparameter-search"}, {}},
		"accelerator_type": "rtx-4090",
		"image":            "torch",
		"metric_reason":    "Validation loss on held-out text is the goal itself.",
		"measurement":      "bench.py trains for a fixed number of steps and scores a split the training never reads.",
		"sizing_reason":    "An estimate, nothing has run: a trial of about 6 minutes at 1 an hour is 0.1, and 2 researchers with 30 trials each need 6 and 4 hours.",
		"questions":        []string{"Is 4 hours enough?"},
	}
	for key, value := range more {
		body[key] = value
	}
	return body
}

// draftMaxHosted leaves a draft three researchers beside its coordinator and the agent that
// measures its baseline.
const draftMaxHosted = 5

func draftWithPlanner() *draftStore {
	return &draftStore{
		statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{{
			ID: "pe-draft", ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", MaxHosted: draftMaxHosted, PlanningEndsAt: time.Now().Add(time.Minute),
		}}}},
		signups:   map[string]domain.AgentSignup{"pe-draft": {AgentID: "ag-planner", Role: domain.SignupRolePlanner, Flavor: domain.PlannerFlavor, Hosted: true}},
		reachable: map[string][]domain.Cluster{"rtx-4090": {{Name: "rented"}}},
	}
}

var thePlanner = auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-planner", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}

// The planner's proposal is the only way a draft gets metrics, a brief and a cost, and a person
// accepts it unread more often than not: what can be checked is checked before it is stored.
func TestAProposalIsStoredOnlyFromThePlannerAndOnlyWhenItHoldsTogether(t *testing.T) {
	store := draftWithPlanner()
	_, call := planningAPI(t, store, thePlanner)
	before := time.Now()
	// What a job of the run asks of a node was prose in the brief, copied by hand into every job.
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"cpu": "8", "memory": " 32Gi ", "storage": "100Gi"}))
	if code != http.StatusOK {
		t.Fatalf("got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	var stored Proposal
	if err := json.Unmarshal(pe.Proposal, &stored); err != nil || stored.BudgetAcceleratorHours != 6 || len(stored.Agents) != 2 || stored.AcceleratorType != "rtx-4090" ||
		stored.CPU != "8" || stored.Memory != "32Gi" || stored.Storage != "100Gi" {
		t.Fatalf("stored %s, %v", pe.Proposal, err)
	}
	if pe.ProposedAt == nil || out["proposed_at"] == nil || out["proposal"] == nil || pe.Status != domain.PlatformExpDraft {
		t.Errorf("the draft is not marked proposed: %+v", pe)
	}
	// From now on it waits for a person, far longer than the planner was given.
	if wait := pe.ProposalExpiresAt.Sub(before); wait < testPlanning.ProposedFor-time.Minute {
		t.Errorf("a proposed draft ends in %v, want %v", wait, testPlanning.ProposedFor)
	}
	// A revision right after the proposal waited for a machine to boot: the planner's is kept for
	// the setting's time, and the draft says until when.
	if pe.PlannerEndsAt == nil || out["planner_ends_at"] == nil || pe.PlannerEndsAt.Sub(before) < testPlanning.PlannerKept-time.Minute || pe.PlannerEndsAt.Sub(before) > testPlanning.PlannerKept+time.Minute {
		t.Errorf("the planner's machine is kept until %v (%v answered), want %v from the proposal", pe.PlannerEndsAt, out["planner_ends_at"], testPlanning.PlannerKept)
	}
	if out["proposal_expires_at"] != pe.ProposalExpiresAt.Format(time.RFC3339Nano) || out["ends_at"] != nil || out["planning_ends_at"] != nil {
		t.Errorf("a proposed draft reads proposal_expires_at %v, ends_at %v, planning_ends_at %v: want how long the proposal waits under that name alone", out["proposal_expires_at"], out["ends_at"], out["planning_ends_at"])
	}
	// The draft itself is unchanged: nothing of a proposal takes effect until a person accepts,
	// and nothing was measured, so it has no baseline.
	if pe.Name != "" || len(pe.Metrics) != 0 || pe.BudgetAcceleratorHours != 0 || pe.BaselineJobID != "" || pe.JobEstimatedDurationHours != 0 {
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
		"no length":                              {map[string]any{"duration_hours": 0}, "invalid_length"},
		"longer than the platform runs one":      {map[string]any{"duration_hours": 31 * 24}, "run_too_long"},
		"nobody to run it":                       {map[string]any{"agents": []map[string]any{}}, "invalid_agents"},
		"more machines than it gets":             {map[string]any{"agents": []map[string]any{{}, {}, {}, {}}}, "invalid_agents"},
		"a planner among the agents":             {map[string]any{"agents": []map[string]any{{"flavor": "planner"}}}, "invalid_agents"},
		"no reason for the metric":               {map[string]any{"metric_reason": ""}, "missing_explanation"},
		"no reason for the size":                 {map[string]any{"sizing_reason": " "}, "missing_explanation"},
		"a measurement that is a paper":          {map[string]any{"measurement": strings.Repeat("x", maxProposalTextBytes+1)}, "too_long"},
		"a type nobody has now":                  {map[string]any{"accelerator_type": "a100"}, "accelerator_unreachable"},
		"no type":                                {map[string]any{"accelerator_type": ""}, "accelerator_unreachable"},
		"an image by a word of its own":          {map[string]any{"image": "pytorch"}, "unknown_job_image"},
		"no image":                               {map[string]any{"image": ""}, "unknown_job_image"},
		"memory that is no quantity":             {map[string]any{"memory": "lots"}, "invalid_resources"},
		"negative storage":                       {map[string]any{"storage": "-100Gi"}, "invalid_resources"},
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

	// A brief named an image the one cluster of its type did not run, and three baseline jobs
	// were refused for it after a person had accepted.
	ownImage := draftWithPlanner()
	ownImage.reachable = map[string][]domain.Cluster{"rtx-4090": {{Name: "box", JobImage: "registry.example.com/hl/box:1"}}}
	_, call = planningAPI(t, ownImage, thePlanner)
	code, out = call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil))
	if message, _ := out["message"].(string); code != http.StatusUnprocessableEntity || out["reason"] != "no_cluster_for_image" || !strings.Contains(message, "box runs every job in registry.example.com/hl/box:1") || ownImage.created[0].ProposedAt != nil {
		t.Errorf("an image no cluster of the type runs: got = %d %v, want 422 no_cluster_for_image naming the cluster's image and nothing stored", code, out)
	}
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"image": "registry.example.com/hl/box:1"})); code != http.StatusOK {
		t.Errorf("the image the cluster runs: got = %d %v, want 200", code, out)
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
		accepted.Status = domain.PlatformExpOpen
		s.created[i] = &accepted
		s.acceptedAgents, s.acceptedSignups = create, signups
		for _, signup := range signups {
			if signup.Role == domain.SignupRoleBaseline {
				signup.Name = domain.AgentName(signup.Role, func(string) bool { return false })
				s.baselines = append(s.baselines, signup)
				continue
			}
			s.researchers = append(s.researchers, signup.AgentID)
		}
		return nil
	}
	return db.ErrNotDraft
}

// FinishAgent finishes a baseline agent of an open experiment, as the store does.
func (s *draftStore) FinishAgent(_ context.Context, _, agentID, reason string) error {
	for i, signup := range s.baselines {
		if signup.AgentID == agentID {
			now := time.Now().UTC()
			s.baselines[i].FinishedAt, s.baselines[i].FinishedReason = &now, reason
			return nil
		}
	}
	return db.ErrNotSignedUp
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
	// The answer says what the open experiment waits for, as every later read of it does.
	if why, _ := out["status_reason"].(map[string]any); why["reason"] != "baseline_measuring" {
		t.Errorf("accept: status_reason = %v, want baseline_measuring", out["status_reason"])
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpOpen || out["status"] != "open" || pe.Name != "Cosine or constant" || !strings.HasPrefix(pe.Description, "Train the small transformer") ||
		len(pe.Metrics) != 1 || pe.Metrics[0].Key != "val_loss" || pe.ReportIntervalSeconds != 30 || len(pe.Stages) != 1 || pe.Goal != "g" {
		t.Fatalf("accepted %+v, want the proposal's experiment", pe)
	}
	if pe.BudgetAcceleratorHours != 3 || pe.MaxAgents != 3 {
		t.Errorf("budget %v for %d agents, want what the person stated (3 and 3), not the proposal's 6 and 2", pe.BudgetAcceleratorHours, pe.MaxAgents)
	}
	// Its two hours are its whole wall clock from the accept, and the baseline has half of them.
	if pe.StartsAt.Before(before) || pe.DurationHours != 2 || !pe.EndsAt.Equal(pe.StartsAt.Add(2*time.Hour)) || !pe.BaselineDueAt.Equal(pe.StartsAt.Add(time.Hour)) {
		t.Errorf("starts %v, %v hours ending %v, baseline due %v: want 2 hours from the accept and the baseline due halfway", pe.StartsAt, pe.DurationHours, pe.EndsAt, pe.BaselineDueAt)
	}
	if !pe.CoordinatorHosted || pe.Coordinator == nil || pe.Coordinator.Launcher != "main" || pe.Coordinator.Model != "platform-model-1" {
		t.Errorf("coordinator %+v, want one the platform hosts", pe.Coordinator)
	}
	// A coordinator had to boot, take a turn and add the agent that measures the baseline before
	// anything was measured: the accept signs it up beside the researchers.
	if len(store.acceptedAgents) != 4 || len(store.acceptedSignups) != 4 {
		t.Fatalf("signed up %d agents with %d signups, want the 3 researchers and the baseline agent", len(store.acceptedAgents), len(store.acceptedSignups))
	}
	for i, signup := range store.acceptedSignups {
		agent, role := store.acceptedAgents[i], domain.SignupRoleCompetitor
		if i == 3 {
			role = domain.SignupRoleBaseline
		}
		if agent.Name != "" || signup.AgentID != agent.ID || !signup.Hosted || signup.Launcher != "main" || signup.Role != role || signup.Model != "platform-model-1" ||
			signup.Actor == nil || signup.Actor.ID != "u-1" {
			t.Errorf("agent %d: %+v signed up as %+v, want a hosted one of role %s added by the person who accepted", i, agent, signup, role)
		}
	}
	if measures := store.acceptedSignups[3]; measures.Flavor != domain.DefaultAgentFlavor || len(measures.Hyperparameters) != 0 {
		t.Errorf("the baseline agent runs as %+v, want the default flavor and no hyperparameters", measures)
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
		"too long":          {map[string]any{"budget_accelerator_hours": 3, "duration_hours": 31 * 24, "agents": []map[string]any{{}}}, "run_too_long"},
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

// A planner ran the baseline itself, which took most of its 16 to 20 minutes, and a proposal
// was refused unless that job held it up. A proposal defines the experiment and runs nothing:
// it names no job, and the fields the platform used to read from one are no fields of it.
func TestAProposalNamesNoBaselineJobAndStatesTheTypeItsJobsAreSizedFor(t *testing.T) {
	for _, field := range []string{"baseline_job_id", "job_estimated_duration_hours"} {
		store := draftWithPlanner()
		_, call := planningAPI(t, store, thePlanner)
		if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{field: "job-1"})); code != http.StatusUnprocessableEntity || store.created[0].ProposedAt != nil {
			t.Errorf("a proposal that states %s: got = %d %v, want 422 for a field the body does not have", field, code, out)
		}
	}
	cpu := draftWithPlanner()
	cpu.reachable = map[string][]domain.Cluster{string(domain.CPUType): {{Name: "k3s"}}}
	_, call := planningAPI(t, cpu, thePlanner)
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"accelerator_type": string(domain.CPUType)}))
	if proposal, _ := out["proposal"].(map[string]any); code != http.StatusOK || proposal["accelerator_type"] != string(domain.CPUType) || out["baseline_job_id"] != nil {
		t.Errorf("a proposal for work on cores: got = %d %v, want 200, the CPU type answered and no baseline", code, out)
	}
}

func (s *draftStore) Revise(_ context.Context, id, message string, by domain.Actor, planUntil time.Time) error {
	for _, pe := range s.created {
		if pe.ID != id || pe.Status != domain.PlatformExpDraft {
			continue
		}
		if !pe.Answered() {
			return db.ErrNothingProposed
		}
		pe.SetAnswered(nil, false)
		pe.PlannerEndsAt = nil
		pe.SetClosesAt(planUntil)
		pe.Revisions = append(pe.Revisions, domain.PlanningRequest{Message: message, Actor: by, At: time.Now().UTC()})
		return nil
	}
	return db.ErrNotDraft
}

func (s *draftStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, actor := range actors {
		actor.Name = "named " + actor.ID
	}
	return nil
}

func (s *draftStore) ListSignups(context.Context, string) ([]string, error) { return nil, nil }

// A person who reads a proposal and wants it otherwise says so in a sentence, and the draft
// goes back to the planner with the proposal still there to read.
func TestRevisingGivesAProposedDraftBackToItsPlannerWithWhatWasAsked(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningAPI(t, store, thePlanner)
	_, call := planningAPI(t, store, planner)
	if code, out := call("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": "too early"}); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Fatalf("revising before anything is proposed: got = %d %v, want 409 invalid_transition", code, out)
	}
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	for name, message := range map[string]string{"nothing": "  ", "an essay": strings.Repeat("x", maxRevisionBytes+1)} {
		if code, _ := call("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": message}); code != http.StatusUnprocessableEntity || store.created[0].ProposedAt == nil {
			t.Errorf("%s: got = %d, want 422 and the proposal left standing", name, code)
		}
	}
	before := time.Now()
	code, out := call("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": " It must fit one RTX 4090. "})
	if code != http.StatusOK {
		t.Fatalf("revise: got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	if pe.ProposedAt != nil || out["proposed_at"] != nil || out["planner_ends_at"] != nil || len(pe.Proposal) == 0 || out["proposal"] == nil || pe.Status != domain.PlatformExpDraft {
		t.Errorf("revised draft %+v, want no proposed_at, no planner_ends_at and the proposal kept", pe)
	}
	if len(pe.Revisions) != 1 || pe.Revisions[0].Message != "It must fit one RTX 4090." || pe.Revisions[0].Actor.Kind != domain.ActorUser || pe.Revisions[0].Actor.ID != "u-1" {
		t.Errorf("revisions = %+v, want the sentence by the person who asked", pe.Revisions)
	}
	if left := pe.PlanningEndsAt.Sub(before); left < testPlanning.Limit-time.Minute || left > testPlanning.Limit+time.Minute {
		t.Errorf("the planner has %v to propose again, want the planning limit", left)
	}
	// Read back, whoever asked is named.
	code, out = call("GET", "/platform-experiments/pe-draft", nil)
	revisions, _ := out["revisions"].([]any)
	if code != http.StatusOK || len(revisions) != 1 || revisions[0].(map[string]any)["actor"].(map[string]any)["name"] != "named u-1" {
		t.Errorf("read back: got = %d %v, want the revision with its author named", code, out["revisions"])
	}
	// The planner proposes again, and the draft waits for the person again.
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"budget_accelerator_hours": 4})); code != http.StatusOK || store.created[0].ProposedAt == nil {
		t.Fatalf("proposing again: %d %v", code, out)
	}
	// The planner cannot ask itself for a revision, and an accepted draft takes none.
	if code, _ := propose("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": "m"}); code != http.StatusForbidden {
		t.Errorf("the planner revising: got = %d, want 403", code)
	}
	store.created[0].Status = domain.PlatformExpOpen
	if code, out := call("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": "m"}); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Errorf("revising what is no draft: got = %d %v, want 409 invalid_transition", code, out)
	}
}

func (s *draftStore) Decline(_ context.Context, id, reason string, waitUntil time.Time, keepPlanner time.Duration) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpDraft {
			now := time.Now().UTC()
			kept := now.Add(keepPlanner)
			pe.DeclinedReason, pe.Proposal, pe.PlannerEndsAt = reason, nil, &kept
			pe.SetAnswered(&now, false)
			pe.SetClosesAt(waitUntil)
			return nil
		}
	}
	return db.ErrNotDraft
}

// A goal the planner could not plan ended as a draft that closed at its limit with nothing on
// its page. The planner says why, and the person answers that as they answer a proposal.
func TestAPlannerDeclinesWithAReasonThePersonReadsAndCanAnswer(t *testing.T) {
	store := draftWithPlanner()
	_, asPlanner := planningAPI(t, store, thePlanner)
	_, call := planningAPI(t, store, planner)
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	for name, reason := range map[string]string{"no reason": " ", "an essay": strings.Repeat("x", maxDeclineBytes+1)} {
		if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": reason}); code != http.StatusUnprocessableEntity || store.created[0].DeclinedReason != "" {
			t.Errorf("%s: got = %d %v, want 422 and nothing stored", name, code, out)
		}
	}
	for name, caller := range map[string]auth.Principal{
		"a researcher of it":   {Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-researcher", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
		"the person who asked": planner,
	} {
		_, as := planningAPI(t, store, caller)
		if code, _ := as("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "r"}); code != http.StatusForbidden || store.created[0].DeclinedReason != "" {
			t.Errorf("%s declining: got = %d, want 403", name, code)
		}
	}

	before := time.Now()
	const why = "\"Make the model better\" names nothing to measure: say which model, on what data, and what better means."
	code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "  " + why + "\n"})
	if code != http.StatusOK || out["declined_reason"] != why || out["declined_at"] == nil || out["proposed_at"] != nil || out["proposal"] != nil || out["status"] != "draft" || out["status_reason"].(map[string]any)["reason"] != "declined" {
		t.Fatalf("decline: got = %d %v, want the reason, when it declined, and nothing proposed", code, out)
	}
	if _, read := call("GET", "/platform-experiments/pe-draft", nil); read["status_reason"].(map[string]any)["reason"] != "declined" {
		t.Errorf("a declined draft read: status_reason = %v, want declined", read["status_reason"])
	}
	if wait := store.created[0].ProposalExpiresAt.Sub(before); wait < testPlanning.ProposedFor-time.Minute {
		t.Errorf("a declined draft ends in %v, want it to wait for the person as a proposal does", wait)
	}
	// The earlier proposal no longer stands, so nothing is accepted.
	accept := map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}}}
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", accept); code != http.StatusConflict || out["reason"] != "invalid_transition" || !strings.Contains(out["message"].(string), "declined") {
		t.Errorf("accepting a declined draft: got = %d %v, want 409 invalid_transition that says it declined", code, out)
	}
	// The person says what was missing; the reason stays until the planner answers again.
	code, out = call("POST", "/platform-experiments/pe-draft/revise", map[string]any{"message": "The small transformer in train.py, by validation loss."})
	if code != http.StatusOK || out["proposed_at"] != nil || out["declined_at"] != nil || out["declined_reason"] != why {
		t.Fatalf("revising a declined draft: got = %d %v, want it back with the planner and the reason kept", code, out)
	}
	if _, read := call("GET", "/platform-experiments/pe-draft", nil); read["status_reason"].(map[string]any)["reason"] != "revising" {
		t.Errorf("a declined draft being revised: status_reason = %v, want revising", read["status_reason"])
	}
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK || out["declined_reason"] != nil || out["proposal"] == nil {
		t.Fatalf("proposing after a decline: got = %d %v, want the proposal and no reason", code, out)
	}

	store.created[0].Status = domain.PlatformExpOpen
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "r"}); code != http.StatusConflict || out["reason"] != "not_a_draft" {
		t.Errorf("declining what is no draft: got = %d %v, want 409 not_a_draft", code, out)
	}
}

// An accepted draft waited for its coordinator three times before anything ran: to add the agent
// that measures the baseline, to notice it had finished, and to declare its job. One coordinator
// that ended its turn early left a paid experiment open until its baseline limit. From the accept
// to the running experiment nobody but the baseline agent acts.
func TestAnAcceptedDraftRunsOnceItsBaselineAgentFinishesWithNoCoordinatorAction(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningAPI(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	svc, call := planningAPI(t, store, planner)
	accept := map[string]any{"budget_accelerator_hours": 6, "duration_hours": 2, "agents": []map[string]any{{}, {}}}
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", accept); code != http.StatusOK || out["status"] != "open" || out["status_reason"].(map[string]any)["reason"] != "baseline_measuring" {
		t.Fatalf("accept: got = %d %v, want it open with its baseline agent measuring", code, out)
	}
	if len(store.baselines) != 1 || len(store.researchers) != 2 {
		t.Fatalf("accepted with %d baseline agents and %d researchers, want 1 and 2", len(store.baselines), len(store.researchers))
	}
	measures := store.baselines[0].AgentID
	measured := measuredBaseline()
	measured.Job.AgentID, measured.Job.PlatformExperimentID, measured.Reported = measures, "pe-draft", map[string]float64{"val_loss": 1.8}
	svc.baseline = func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*Baseline, error) {
		if jobID != measured.Job.ID {
			return nil, nil
		}
		return measured, nil
	}
	ctx := context.Background()

	// A baseline agent that gives up leaves nothing to declare, and the experiment says whose move it is.
	gaveUp := *store
	gaveUp.baselines = append([]domain.AgentSignup{}, store.baselines...)
	gaveUp.created = []*domain.PlatformExperiment{store.created[0]}
	gaveUpSvc, _ := planningAPI(t, &gaveUp, planner)
	if _, err := gaveUpSvc.FinishAgent(ctx, "pe-draft", measures, "train.py does not run"); err != nil {
		t.Fatalf("a baseline agent finishing with no job: %v", err)
	}
	left := gaveUp.created[0]
	if err := gaveUpSvc.Describe(ctx, left, planner); err != nil || left.Status != domain.PlatformExpOpen || left.StatusReason.Reason != "baseline_required" ||
		!strings.Contains(left.StatusReason.Message, "train.py does not run") {
		t.Fatalf("after a finish that left no job: %s %+v, %v, want it open, baseline_required, with the reason the agent gave", left.Status, left.StatusReason, err)
	}

	// The one that measured: its finish declares its job and starts the run.
	store.left = []*domain.Experiment{measured.Job}
	finished, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured")
	if err != nil || finished.FinishedAt == nil {
		t.Fatalf("the baseline agent finishing: %+v, %v", finished, err)
	}
	pe := store.created[0]
	if pe.BaselineJobID != "job-1" || pe.Status != domain.PlatformExpRunning || store.started != 1 || pe.JobEstimatedDurationHours != baselineHeadroom*measured.ObservedHours ||
		!strings.Contains(pe.Description, measuredBaselineHeader+"\njob: job-1") {
		t.Fatalf("after the finish: baseline %q, status %s, %d starts, estimate %v, brief %q: want job-1 declared as a coordinator's PATCH declares it and the run started",
			pe.BaselineJobID, pe.Status, store.started, pe.JobEstimatedDurationHours, pe.Description)
	}
	// Finishing again changes nothing.
	if _, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured"); err != nil || store.started != 1 {
		t.Fatalf("a second finish: %v with %d starts, want nothing changed", err, store.started)
	}
}

// A declaration the platform makes is refused as a coordinator's is. The finish still stands, the
// experiment says what the declaration was refused for, and the owner's change that clears it is
// the request that declares the job and starts the run.
func TestADeclarationThePlatformIsRefusedWaitsForTheOwnerAndIsMadeByTheirChange(t *testing.T) {
	finished := time.Now().UTC()
	pe := openExperiment()
	pe.BudgetAcceleratorHours, pe.BaselineDueAt = 1, time.Now().UTC().Add(time.Hour)
	measured := measuredBaseline()
	store, patch := declareAPI(t, pe, measured, auth.Principal(ownerOfA))
	store.researchers = []string{"ag-1", "ag-2"}
	store.baselines = []domain.AgentSignup{{AgentID: "ag-baseline", Name: "Baseline 1", Role: domain.SignupRoleBaseline, FinishedAt: &finished}}
	store.left = []*domain.Experiment{measured.Job}

	code, out := patch(map[string]any{"description": "Train train.py and report val_loss, on the small split."})
	why, _ := out["status_reason"].(map[string]any)
	if code != http.StatusOK || out["status"] != "open" || store.created[0].BaselineJobID != "" || why["reason"] != "budget_below_baseline" ||
		!strings.Contains(why["message"].(string), "the platform did not declare job job-1") {
		t.Fatalf("a change that clears nothing: got = %d %v, want it open, nothing declared and budget_below_baseline", code, out["status_reason"])
	}
	code, out = patch(map[string]any{"budget_accelerator_hours": 8})
	if code != http.StatusOK || out["status"] != "running" || store.created[0].BaselineJobID != "job-1" || store.started != 1 {
		t.Fatalf("the owner raising the budget: got = %d %v with baseline %q and %d starts, want job-1 declared and the run started", code, out, store.created[0].BaselineJobID, store.started)
	}
}

// A person who wanted another name for the experiment had to ask the planner for a revision and
// wait for it. The accept takes the name they typed, held to the rule of every name, and nothing
// else of the proposal's experiment.
func TestAcceptingADraftTakesTheNameThePersonGaveIt(t *testing.T) {
	proposed := func() (*draftStore, func(method, path string, body any) (int, map[string]any)) {
		store := draftWithPlanner()
		_, propose := planningAPI(t, store, thePlanner)
		if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
			t.Fatalf("propose: %d %v", code, out)
		}
		_, call := planningAPI(t, store, planner)
		return store, call
	}
	accept := func(more map[string]any) map[string]any {
		body := map[string]any{"budget_accelerator_hours": 3, "duration_hours": 2, "agents": []map[string]any{{}}}
		for key, value := range more {
			body[key] = value
		}
		return body
	}

	store, call := proposed()
	code, out := call("POST", "/platform-experiments/pe-draft/accept", accept(map[string]any{"name": "  Cosine against constant, small transformer \n"}))
	pe := store.created[0]
	if code != http.StatusOK || pe.Name != "Cosine against constant, small transformer" || out["name"] != pe.Name {
		t.Fatalf("accepting under another name: got = %d %v, stored %q, want the name given, trimmed", code, out, pe.Name)
	}
	if !strings.HasPrefix(pe.Description, "Train the small transformer") || len(pe.Metrics) != 1 || pe.Metrics[0].Key != "val_loss" {
		t.Errorf("accepted %+v, want the proposal's brief and metrics unchanged", pe)
	}

	store, call = proposed()
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", accept(nil)); code != http.StatusOK || store.created[0].Name != "Cosine or constant" {
		t.Fatalf("accepting with no name: got = %d %v, stored %q, want the proposal's", code, out, store.created[0].Name)
	}

	for name, given := range map[string]string{"two lines": "Cosine\nor constant", "nothing but spaces": "   "} {
		store, call = proposed()
		code, out := call("POST", "/platform-experiments/pe-draft/accept", accept(map[string]any{"name": given}))
		if code != http.StatusUnprocessableEntity || out["reason"] != "invalid_experiment" || store.created[0].Status != domain.PlatformExpDraft {
			t.Errorf("a name of %s: got = %d %v with the draft %s, want 422 invalid_experiment and the draft left a draft", name, code, out, store.created[0].Status)
		}
	}
	// Nothing else of the experiment is the accept's to change.
	store, call = proposed()
	if code, out := call("POST", "/platform-experiments/pe-draft/accept", accept(map[string]any{"description": "another brief"})); code != http.StatusUnprocessableEntity || store.created[0].Status != domain.PlatformExpDraft {
		t.Errorf("an accept that states a brief: got = %d %v, want 422 and nothing accepted", code, out)
	}
}
