package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

func measuredBaseline() *platformexperiments.Baseline {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	return &platformexperiments.Baseline{
		Job: &domain.Experiment{ID: "job-1", AgentID: "ag-baseline", PlatformExperimentID: "pe-1", Branch: "hl/pe-1/baseline", Status: domain.StatusCompleted, CodeRef: "git://seed@abc", Image: "registry.test/image@sha256:aa",
			AcceleratorType: "h100", AcceleratorCount: 2, EstimatedDurationHours: 5,
			// The machine aProposal plans on.
			Job: domain.JobSpec{AcceleratorType: "rtx-4090", AcceleratorCount: 1, CPU: "8", Memory: "32Gi", Storage: "100Gi"}},
		OwnerRole: domain.SignupRoleBaseline, Finding: true, Reported: map[string]float64{"val_loss": 1.8, "exact_match": 1}, ObservedHours: 0.2,
	}
}

// planningRouter is the planning routes and the ones a draft passes through, over store.
func planningRouter(t *testing.T, store platformexperiments.Store, caller auth.Principal) (*platformexperiments.Service, func(method, path string, body any) (int, map[string]any)) {
	t.Helper()
	svc := platformexperiments.NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute).WithServedModels(testModels)
	placement := authtest.Placement{
		Projects: map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{
			"pe-draft":   {PlatformExperimentID: "pe-draft", ProjectID: "prj-a", OrgID: "org-a"},
			"pe-prior":   {PlatformExperimentID: "pe-prior", ProjectID: "prj-a", OrgID: "org-a"},
			"pe-foreign": {PlatformExperimentID: "pe-foreign", ProjectID: "prj-b", OrgID: "org-b"},
		},
	}
	router := authtest.Router(authtest.As(caller))
	doc := NewDoc(router, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
	planningRepositories = &fakeRepositories{files: map[string]string{"prj-a": ""}}
	svc.WithRepositories(planningRepositories)
	handler := svc.WithRepositories(planningRepositories).WithPlanning(testPlanning).WithGitProxy("https://api.test/git").
		WithAgentLaunch(platformexperiments.AgentLaunch{AgentSandbox: true, DefaultModel: "platform-model-1", CoordinatorModel: "platform-coordinator-1", SigningSecret: agentsTestSecret, MaxAgentSandboxes: createMaxAgentSandboxes})
	if drafts, ok := store.(*draftStore); ok {
		svc.WithReachable(func(context.Context, string) (map[string][]domain.Cluster, error) { return drafts.reachable, nil })
	}
	registerPlatformExperimentsAndBudget(doc, handler)
	registerPlanning(doc, handler, zap.NewNop())
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
// planningRepositories is the git host as the last planningRouter sees it.
var planningRepositories *fakeRepositories

func TestADraftIsCreatedFromOneSentenceWithItsPlannerAndNothingOfARun(t *testing.T) {
	store := &draftStore{}
	svc, call := planningRouter(t, store, planner)
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
	if pe.BudgetAcceleratorHours != 0 || len(pe.Metrics) != 0 || pe.CoordinatorInAgentSandbox {
		t.Errorf("a draft holds %v AccH, %d metrics, coordinator %v: want no budget and nothing of a run", pe.BudgetAcceleratorHours, len(pe.Metrics), pe.CoordinatorInAgentSandbox)
	}
	// A planner read a made-up max_agents of 1 as a cap and proposed one agent.
	if most := platformexperiments.MaxProposedAgents(createMaxAgentSandboxes); pe.MaxAgents != most || out["max_agents"] != float64(most) {
		t.Errorf("a draft reads max_agents %d (%v answered), want %d: the most agents a proposal is accepted with", pe.MaxAgents, out["max_agents"], most)
	}
	if limit := pe.PlanningEndsAt.Sub(before); limit < testPlanning.Limit-time.Minute || limit > testPlanning.Limit+time.Minute {
		t.Errorf("a draft ends %v after it was created, want the planning limit", limit)
	}
	if out["planning_ends_at"] != pe.PlanningEndsAt.Format(time.RFC3339Nano) || out["ends_at"] != nil || out["proposal_expires_at"] != nil {
		t.Errorf("a draft being planned reads planning_ends_at %v, ends_at %v, proposal_expires_at %v: want its planning limit under that name alone", out["planning_ends_at"], out["ends_at"], out["proposal_expires_at"])
	}
	if !strings.HasSuffix(pe.Name, "…") || len([]rune(pe.Name)) > platformexperiments.DraftNameLength+1 || !strings.HasPrefix(goal, strings.TrimSuffix(pe.Name, "…")) {
		t.Errorf("name = %q, want the goal cut at a word", pe.Name)
	}
	if pe.Actor == nil || *pe.Actor != (domain.Actor{Kind: domain.ActorUser, ID: "u-1", Name: "Ada"}) {
		t.Errorf("actor = %+v, want the person who asked", pe.Actor)
	}
	// Its agents are the planner and the baseline agent, which the platform runs at once and
	// never ranks or cuts: the baseline agent starts on the planner's working plan.
	planner, signup := store.planners[pe.ID], store.signups[pe.ID]
	if planner == nil || planner.Name != "Planner" || signup.AgentID != planner.ID || !signup.InAgentSandbox ||
		signup.AgentType != domain.PlannerAgentType || signup.Role != domain.SignupRolePlanner || signup.Model != "platform-model-1" || out["signup_count"] != float64(2) {
		t.Errorf("planner %+v signed up as %+v, want a sandboxed planner of role planner on the platform's model", planner, signup)
	}
	if len(store.baselines) != 1 || store.baselines[0].Role != domain.SignupRoleBaseline || !store.baselines[0].InAgentSandbox || store.baselines[0].AgentType != "baseline" {
		t.Errorf("baseline agents %+v, want one sandboxed of role baseline", store.baselines)
	}
	// A draft has a repository: the one named, or a new one the repository pass makes after the
	// create answered.
	if pe.Repository != "" || len(planningRepositories.created) != 0 || out["status_reason"].(map[string]any)["reason"] != "repository_creating" {
		t.Errorf("repository = %q (created %v), status_reason %v: want none yet and repository_creating", pe.Repository, planningRepositories.created, out["status_reason"])
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pe.Repository != "acme/"+platformexperiments.RepositoryName(pe) || pe.RepositoryURL != "https://github.com/"+pe.Repository+".git" || pe.Branch != "main" || len(planningRepositories.created) != 1 {
		t.Errorf("repository = %q at %q (created %v), want one made for the draft from the experiment template at its default branch", pe.Repository, pe.Branch, planningRepositories.created)
	}
	if code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": goal, "repository": "acme/model", "branch": "main"}); code != http.StatusCreated || out["repository"] != testRepository || out["branch"] != "main" || len(planningRepositories.created) != 1 {
		t.Errorf("naming a repository: got = %d %v at %v, want it as GitHub spells it at the branch named and none created", code, out["repository"], out["branch"])
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

}

// "Plan it" froze for seven seconds while GitHub created the draft's repository inside the
// request. The create answers once the draft is stored, and a repository GitHub then refuses
// closes the draft with what GitHub said.
func TestADraftIsAnsweredBeforeItsRepositoryAndARefusedOneClosesIt(t *testing.T) {
	store := &draftStore{}
	svc, call := planningRouter(t, store, planner)
	planningRepositories.notCreated = refusal.Refuse(http.StatusConflict, "repository_exists", "the account already has a repository of that name")
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

func TestADraftIsAbandonedByClosingItAndByItsPlanningLimit(t *testing.T) {
	now := time.Now().UTC()
	draft := func(id string, ends time.Time) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", PlanningEndsAt: ends, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &draftStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		draft("pe-planning", now.Add(time.Hour)), draft("pe-late", now.Add(-time.Minute)), draft("pe-dropped", now.Add(time.Hour)),
	}}}}
	svc, _ := planningRouter(t, store, planner)
	// Closing a draft derives no standings: it never ran and has no metric to rank on.
	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	if _, err := svc.Close(context.Background(), "pe-dropped", &owner, "", nil); err != nil {
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

// statusStore is createdStore with the reads and the write a close and a sweep make. unsettled
// are the platform experiments a job of which is live or not yet settled.
type statusStore struct {
	createdStore
	unsettled map[string]bool
	records   map[string]string
	// closeAgents are the agent lines each took before its close.
	closeAgents map[string]string
}

func (s *statusStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, actor := range actors {
		actor.Name = "named " + actor.ID
	}
	return nil
}

func (s *statusStore) SetCloseAgents(_ context.Context, id, lines string) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status != domain.PlatformExpClosed {
			if s.closeAgents == nil {
				s.closeAgents = map[string]string{}
			}
			s.closeAgents[id] = lines
		}
	}
	return nil
}

func (s *statusStore) CloseAgents(_ context.Context, id string) (string, error) {
	return s.closeAgents[id], nil
}

func (s *statusStore) JobsSettled(_ context.Context, id string) (bool, error) {
	return !s.unsettled[id], nil
}

func (s *statusStore) ClosesToRecord(context.Context, time.Duration) ([]string, error) {
	var ids []string
	for _, pe := range s.created {
		if pe.Status == domain.PlatformExpClosed && s.records[pe.ID] == "" && !s.unsettled[pe.ID] {
			ids = append(ids, pe.ID)
		}
	}
	return ids, nil
}

func (s *statusStore) ListPlatformExperimentsByStatus(_ context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	var out []*domain.PlatformExperiment
	for _, pe := range s.created {
		if pe.Status == status {
			out = append(out, pe)
		}
	}
	return out, nil
}

func (s *statusStore) ClosePlatformExperiment(_ context.Context, id string, by *domain.Actor, message string) error {
	for _, pe := range s.created {
		if pe.ID == id {
			closedAt := time.Now().UTC()
			pe.CloseReason = domain.CloseByRequest
			if by == nil && pe.Status == domain.PlatformExpOpen {
				pe.CloseReason = domain.CloseBaselineLimitReached
			}
			pe.Status, pe.ClosedBy, pe.ClosedAt, pe.CloseMessage = domain.PlatformExpClosed, by, &closedAt, message
		}
	}
	return nil
}

func (s *statusStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	return map[string]db.AgentJobs{}, nil
}

// RecordClose keeps the first record and answers it after the summary, as the store does.
func (s *statusStore) RecordClose(_ context.Context, id, record string) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpClosed && s.records[id] == "" {
			if s.records == nil {
				s.records = map[string]string{}
			}
			s.records[id] = record
			pe.Summary = strings.TrimLeft(pe.Summary+"\n\n"+record, "\n")
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

	// active are the QUEUED, SUBMITTED and RUNNING jobs, and cancelled the ones cancelled.
	active    []*domain.Experiment
	cancelled []string
}

func (s *draftStore) ListActiveByPlatformExperiment(_ context.Context, id string) ([]*domain.Experiment, error) {
	var active []*domain.Experiment
	for _, job := range s.active {
		if job.PlatformExperimentID == id {
			active = append(active, job)
		}
	}
	return active, nil
}

func (s *draftStore) CancelExperiment(_ context.Context, id string) error {
	s.cancelled = append(s.cancelled, id)
	return nil
}

func (s *draftStore) CreateDraft(_ context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup, _ *domain.Agent, baseline domain.AgentSignup) error {
	if s.planners == nil {
		s.planners, s.signups = map[string]*domain.Agent{}, map[string]domain.AgentSignup{}
	}
	s.created = append(s.created, pe)
	s.planners[pe.ID], s.signups[pe.ID] = planner, signup
	baseline.Name = "Baseline 1"
	s.baselines = append(s.baselines, baseline)
	return nil
}

func (s *draftStore) SetWorkingPlan(_ context.Context, id string, goalVersion int, proposal []byte) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpDraft {
			if pe.GoalVersion != goalVersion {
				return db.ErrGoalChanged
			}
			pe.Proposal, pe.DeclinedReason = proposal, ""
			return nil
		}
	}
	return db.ErrNotDraft
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
	if agentID == "ag-executor" {
		return &domain.AgentSignup{AgentID: agentID, AgentType: domain.DefaultAgentType}, nil
	}
	return nil, nil
}

func (s *draftStore) ApplyProposal(_ context.Context, pe *domain.PlatformExperiment, goalVersion int, proposal []byte, create []*domain.Agent, signups []domain.AgentSignup) error {
	for i, stored := range s.created {
		if stored.ID != pe.ID {
			continue
		}
		if stored.Status != domain.PlatformExpDraft {
			return db.ErrNotDraft
		}
		if stored.GoalVersion != goalVersion {
			return db.ErrGoalChanged
		}
		applied := *pe
		now := time.Now().UTC()
		applied.Status, applied.Proposal, applied.DeclinedReason = domain.PlatformExpOpen, proposal, ""
		applied.SetAnswered(&now, true)
		s.created[i] = &applied
		s.acceptedAgents, s.acceptedSignups = create, signups
		for _, signup := range signups {
			if signup.Role == domain.SignupRoleBaseline {
				name, err := domain.AgentName(signup.Role, func(string) bool { return false })
				if err != nil {
					return err
				}
				signup.Name = name
				s.baselines = append(s.baselines, signup)
				continue
			}
			s.agentIDs = append(s.agentIDs, signup.AgentID)
		}
		return nil
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
		"goal_version":             1,
		"budget_accelerator_hours": 6, "duration_hours": 4, "baseline_minutes": 30, "agents": []map[string]any{{"agent_type": "hyperparameter-search"}, {}},
		"accelerator_type": "rtx-4090",
		"cpu":              "8",
		"memory":           "32Gi",
		"storage":          "100Gi",
		"metric_reason":    "Validation loss on held-out text is the goal itself.",
		"measurement":      "bench.py trains for a fixed number of steps and scores a split the training never reads.",
		"sizing_reason":    "An estimate, nothing has run: a trial of about 6 minutes at 1 an hour is 0.1, and 2 executors with 30 trials each need 6 and 4 hours.",
	}
	for key, value := range more {
		body[key] = value
	}
	return body
}

// draftMaxAgentSandboxes leaves a draft three agents beside its coordinator and the agent that
// measures its baseline.
const draftMaxAgentSandboxes = 5

func draftWithPlanner() *draftStore {
	return &draftStore{
		statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{{
			ID: "pe-draft", ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", GoalVersion: 1, MaxAgentSandboxes: draftMaxAgentSandboxes, PlanningEndsAt: time.Now().Add(time.Minute),
		}}}},
		signups:   map[string]domain.AgentSignup{"pe-draft": {AgentID: "ag-planner", Role: domain.SignupRolePlanner, AgentType: domain.PlannerAgentType, InAgentSandbox: true}},
		reachable: map[string][]domain.Cluster{"rtx-4090": {{Name: "rented"}}},
	}
}

var thePlanner = auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-planner", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}

// The planner's proposal is the only way a draft gets metrics, a brief and a cost, and a person
// accepts it unread more often than not: what can be checked is checked before it is stored.
func TestAProposalIsStoredOnlyFromThePlannerAndOnlyWhenItHoldsTogether(t *testing.T) {
	store := draftWithPlanner()
	_, call := planningRouter(t, store, thePlanner)
	before := time.Now()
	// What a job of the run asks of a node was prose in the brief, copied by hand into every job.
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"cpu": "8", "memory": " 32Gi ", "storage": "100Gi"}))
	if code != http.StatusOK {
		t.Fatalf("got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	var stored platformexperiments.Proposal
	if err := json.Unmarshal(pe.Proposal, &stored); err != nil || stored.BudgetAcceleratorHours != 6 || len(stored.Agents) != 2 || stored.AcceleratorType != "rtx-4090" ||
		stored.CPU != "8" || stored.Memory != "32Gi" || stored.Storage != "100Gi" {
		t.Fatalf("stored %s, %v", pe.Proposal, err)
	}
	if pe.ProposedAt == nil || out["proposed_at"] == nil || out["proposal"] == nil || pe.Status != domain.PlatformExpOpen {
		t.Errorf("the proposal did not open the draft: %+v", pe)
	}
	// It opens with the proposal's experiment, and nothing was measured, so it has no baseline.
	if pe.Name != "Cosine or constant" || len(pe.Metrics) != 1 || pe.BudgetAcceleratorHours != 6 || pe.BaselineJobID != "" || pe.JobEstimatedDurationHours != 0 || pe.StartsAt.Before(before) {
		t.Errorf("opened %+v, want the proposal's experiment and no baseline", pe)
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
		"an experiment that sets its own budget":  {experiment(map[string]any{"budget": 3}), "invalid_experiment"},
		"no brief":                                {experiment(map[string]any{"description": " "}), "invalid_experiment"},
		"no ranked metric":                        {experiment(map[string]any{"metrics": []map[string]any{{"key": "val_loss", "direction": "minimize", "role": "constraint", "bound": 1}}}), "invalid_metrics"},
		"a ladder that is not one":                {experiment(map[string]any{"stages": []map[string]any{{"length_pct": 40, "evict_pct": 50}}}), "invalid_stages"},
		"no cost":                                 {map[string]any{"budget_accelerator_hours": 0}, "invalid_cost"},
		"no length":                               {map[string]any{"duration_hours": 0}, "invalid_length"},
		"longer than the platform runs one":       {map[string]any{"duration_hours": 31 * 24}, "run_too_long"},
		"no time for the baseline":                {map[string]any{"baseline_minutes": 0}, "invalid_baseline_minutes"},
		"less time than the platform's floor":     {map[string]any{"baseline_minutes": 19}, "invalid_baseline_minutes"},
		"more time for the baseline than allowed": {map[string]any{"baseline_minutes": 91}, "invalid_baseline_minutes"},
		"nobody to run it":                        {map[string]any{"agents": []map[string]any{}}, "invalid_agents"},
		"more machines than it gets":              {map[string]any{"agents": []map[string]any{{}, {}, {}, {}}}, "invalid_agents"},
		"a planner among the agents":              {map[string]any{"agents": []map[string]any{{"agent_type": "planner"}}}, "invalid_agents"},
		"researchers and no executor":             {map[string]any{"agents": []map[string]any{{"agent_type": domain.ResearcherAgentType}}}, "invalid_agents"},
		"no reason for the metric":                {map[string]any{"metric_reason": ""}, "missing_explanation"},
		"no reason for the size":                  {map[string]any{"sizing_reason": " "}, "missing_explanation"},
		"a measurement that is a paper":           {map[string]any{"measurement": strings.Repeat("x", platformexperiments.MaxProposalTextBytes+1)}, "too_long"},
		"a question with no answer planned on":    {map[string]any{"questions": []map[string]any{{"question": "How big a win counts?", "answer": " "}}}, "too_long"},
		"a type nobody has now":                   {map[string]any{"accelerator_type": "a100"}, "accelerator_unreachable"},
		"no type":                                 {map[string]any{"accelerator_type": ""}, "accelerator_unreachable"},
		"memory that is no quantity":              {map[string]any{"memory": "lots"}, "invalid_resources"},
		"negative storage":                        {map[string]any{"storage": "-100Gi"}, "invalid_resources"},
	} {
		store := draftWithPlanner()
		_, call := planningRouter(t, store, thePlanner)
		code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(c.more))
		if code != http.StatusUnprocessableEntity || out["reason"] != c.reason || store.created[0].ProposedAt != nil {
			t.Errorf("%s: got = %d %v, want 422 %s and nothing stored", name, code, out, c.reason)
		}
	}

	for name, caller := range map[string]auth.Principal{
		"an executor of it":    {Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-executor", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
		"the person who asked": planner,
	} {
		store := draftWithPlanner()
		_, call := planningRouter(t, store, caller)
		if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusForbidden || store.created[0].ProposedAt != nil {
			t.Errorf("%s: got = %d %v, want 403", name, code, out)
		}
	}

	accepted := draftWithPlanner()
	accepted.created[0].Status = domain.PlatformExpOpen
	_, call = planningRouter(t, accepted, thePlanner)
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusConflict || out["reason"] != "not_a_draft" {
		t.Errorf("proposing what is no draft: got = %d %v, want 409 not_a_draft", code, out)
	}
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
	return domain.ErrNotSignedUp
}

// A person who accepted 10 s after the proposal still waited 30 minutes for the set-up, and one
// away from the page held the run up. A proposal opens the draft in the write that stores it, at
// its own cost and team, owned by the person who created the draft.
func TestAProposalOpensTheDraftAsProposed(t *testing.T) {
	store := draftWithPlanner()
	creator := domain.Actor{Kind: domain.ActorUser, ID: "u-creator"}
	store.created[0].Actor = &creator
	_, propose := planningRouter(t, store, thePlanner)
	before := time.Now()
	code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{
		"agents": []map[string]any{{"agent_type": "architecture-search", "hyperparameters": map[string]any{"depth": 4}}, {}},
	}))
	if code != http.StatusOK {
		t.Fatalf("propose: got = %d %v, want 200", code, out)
	}
	if why, _ := out["status_reason"].(map[string]any); why["reason"] != "baseline_measuring" {
		t.Errorf("propose: status_reason = %v, want baseline_measuring", out["status_reason"])
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpOpen || out["status"] != "open" || pe.Name != "Cosine or constant" || !strings.HasPrefix(pe.Description, "Train the small transformer") ||
		len(pe.Metrics) != 1 || pe.Metrics[0].Key != "val_loss" || pe.ReportIntervalSeconds != 30 || len(pe.Stages) != 1 || pe.Goal != "g" {
		t.Fatalf("opened %+v, want the proposal's experiment", pe)
	}
	if pe.BudgetAcceleratorHours != 6 || pe.MaxAgents != 2 || pe.DurationHours != 4 {
		t.Errorf("budget %v, %d executors, %v hours, want the proposal's 6, 2 and 4", pe.BudgetAcceleratorHours, pe.MaxAgents, pe.DurationHours)
	}
	// The run's hours count from its start: until then it has no end, and the baseline is due the
	// proposal's 30 minutes after the proposal.
	if pe.StartsAt.Before(before) || !pe.EndsAt.IsZero() || !pe.BaselineDueAt.Equal(pe.StartsAt.Add(30*time.Minute)) {
		t.Errorf("starts %v ending %v, baseline due %v: want no end yet and the baseline due 30 minutes after the proposal", pe.StartsAt, pe.EndsAt, pe.BaselineDueAt)
	}
	if !pe.CoordinatorInAgentSandbox || pe.Coordinator == nil || pe.Coordinator.Model != "" {
		t.Errorf("coordinator %+v, want one the platform hosts on its type's model", pe.Coordinator)
	}
	if len(store.acceptedAgents) != 3 || len(store.acceptedSignups) != 3 || len(store.baselines) != 1 {
		t.Fatalf("signed up %d agents with %d signups and %d baseline agents, want the 2 executors and the baseline agent", len(store.acceptedAgents), len(store.acceptedSignups), len(store.baselines))
	}
	for i, signup := range store.acceptedSignups[:2] {
		agent := store.acceptedAgents[i]
		if agent.Name != "" || signup.AgentID != agent.ID || !signup.InAgentSandbox || signup.Role != domain.SignupRoleExecutor || signup.Model != "" ||
			signup.Actor == nil || signup.Actor.ID != "u-creator" {
			t.Errorf("agent %d: %+v signed up as %+v, want a sandboxed executor added for the person who created the draft", i, agent, signup)
		}
	}
	if measures := store.baselines[0]; measures.Role != domain.SignupRoleBaseline || !measures.InAgentSandbox || measures.AgentType != "baseline" || len(measures.Hyperparameters) != 0 {
		t.Errorf("the baseline agent runs as %+v, want a sandboxed one of the built-in baseline type and no hyperparameters", measures)
	}
	if first := store.acceptedSignups[0]; first.AgentType != "architecture-search" || first.Hyperparameters["depth"] != float64(4) || store.acceptedSignups[1].AgentType != domain.DefaultAgentType {
		t.Errorf("signups %+v, want the agent types and hyperparameters proposed", store.acceptedSignups)
	}
	// The proposal stays to be read, with when it was applied and no person named as accepting.
	_, call := planningRouter(t, store, planner)
	_, out = call("GET", "/platform-experiments/pe-draft", nil)
	if proposal, _ := out["proposal"].(map[string]any); proposal["experiment"] == nil || out["accepted_at"] == nil || out["accepted_by"] != nil {
		t.Errorf("read after the proposal: proposal %v, accepted_at %v, accepted_by %v, want the proposal and accepted_at without accepted_by", out["proposal"], out["accepted_at"], out["accepted_by"])
	}
	// A second proposal is refused by state, so a retried one does no harm.
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusConflict || out["reason"] != "not_a_draft" {
		t.Errorf("proposing twice: got = %d %v, want 409 not_a_draft", code, out)
	}

	// A planner gave its baseline 25 minutes, and building its environment alone took 17.
	// A window under the platform's floor is refused with the floor and why it is there.
	short := draftWithPlanner()
	_, propose = planningRouter(t, short, thePlanner)
	code, out = propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"baseline_minutes": 19}))
	if message, _ := out["message"].(string); code != http.StatusUnprocessableEntity || !strings.Contains(message, "from 20 (planning.min_baseline_minutes) to 90") || short.created[0].Status != domain.PlatformExpDraft {
		t.Errorf("a window under the floor: got = %d %v, want 422 naming the floor and the draft left a draft", code, out)
	}
}

// A planner ran the baseline itself, which took most of its 16 to 20 minutes, and a proposal
// was refused unless that job held it up. A proposal defines the experiment and runs nothing:
// it names no job, and the fields the platform used to read from one are no fields of it.
// The team is one list: a researcher in it is signed up as a researcher, and max_agents is its
// executors.
func TestAProposalSignsUpTheResearchersOfItsTeam(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"agents": []map[string]any{{"agent_type": domain.ResearcherAgentType}, {}}})); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	pe := store.created[0]
	if pe.MaxAgents != 1 || len(pe.Agents) != 2 || !pe.Agents[0].Key().Researches() || pe.Agents.Executors() != 1 {
		t.Errorf("max_agents %d and agents %+v, want one executor counted and the researcher in the team", pe.MaxAgents, pe.Agents)
	}
	if len(store.acceptedSignups) != 3 || store.acceptedSignups[0].Role != domain.SignupRoleResearcher || store.acceptedSignups[0].AgentType != domain.ResearcherAgentType ||
		store.acceptedSignups[1].Role != domain.SignupRoleExecutor {
		t.Errorf("signups %+v, want a researcher, an executor and the baseline agent", store.acceptedSignups)
	}
}

func TestAProposalNamesNoBaselineJobAndStatesTheTypeItsJobsAreSizedFor(t *testing.T) {
	for _, field := range []string{"baseline_job_id", "job_estimated_duration_hours"} {
		store := draftWithPlanner()
		_, call := planningRouter(t, store, thePlanner)
		if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{field: "job-1"})); code != http.StatusUnprocessableEntity || store.created[0].ProposedAt != nil {
			t.Errorf("a proposal that states %s: got = %d %v, want 422 for a field the body does not have", field, code, out)
		}
	}
	cpu := draftWithPlanner()
	cpu.reachable = map[string][]domain.Cluster{string(domain.CPUType): {{Name: "k3s"}}}
	_, call := planningRouter(t, cpu, thePlanner)
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"accelerator_type": string(domain.CPUType)}))
	if proposal, _ := out["proposal"].(map[string]any); code != http.StatusOK || proposal["accelerator_type"] != string(domain.CPUType) || out["baseline_job_id"] != nil {
		t.Errorf("a proposal for work on cores: got = %d %v, want 200, the CPU type answered and no baseline", code, out)
	}
}

// A planner of a CPU-only brief was refused six times: accelerator_type was required, and "", cpu
// and the CPU type were each unreachable with no word of what was. A proposal states a type
// `hl capacity` lists, or none for work on cores, and a refusal names every type that is listed.
func TestAProposalStatesATypeCapacityListsOrNoneForCores(t *testing.T) {
	cpuAndGPU := map[string][]domain.Cluster{string(domain.CPUType): {{Name: "k3s"}}, "rtx-4090": {{Name: "rented"}}}
	gpuOnly := map[string][]domain.Cluster{"rtx-4090": {{Name: "rented"}}}
	for _, tc := range []struct {
		name      string
		reachable map[string][]domain.Cluster
		stated    any
		wantType  string
		wantNames []string
	}{
		{name: "no accelerator_type is work on cores", reachable: cpuAndGPU, stated: nil, wantType: string(domain.CPUType)},
		{name: "the CPU type as hl capacity prints it", reachable: cpuAndGPU, stated: string(domain.CPUType), wantType: string(domain.CPUType)},
		{name: "an accelerator hl capacity lists", reachable: cpuAndGPU, stated: "rtx-4090", wantType: "rtx-4090"},
		{name: "a type nobody lists", reachable: cpuAndGPU, stated: "cpu", wantNames: []string{`"cpu"`, `"rtx-4090"`, `"` + string(domain.CPUType) + `" (or no accelerator_type`}},
		{name: "an empty type is no accelerator_type", reachable: gpuOnly, stated: "", wantNames: []string{"work on CPU cores", `"rtx-4090"`}},
		{name: "cores where no cluster lists them", reachable: gpuOnly, stated: nil, wantNames: []string{"work on CPU cores", `"rtx-4090"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := draftWithPlanner()
			store.reachable = tc.reachable
			_, call := planningRouter(t, store, thePlanner)
			body := aProposal(map[string]any{"accelerator_type": tc.stated})
			if tc.stated == nil {
				delete(body, "accelerator_type")
			}
			code, out := call("PUT", "/platform-experiments/pe-draft/proposal", body)
			if tc.wantType != "" {
				if proposal, _ := out["proposal"].(map[string]any); code != http.StatusOK || proposal["accelerator_type"] != tc.wantType {
					t.Fatalf("got = %d %v, want 200 and accelerator_type %s stored", code, out, tc.wantType)
				}
				return
			}
			message, _ := out["message"].(string)
			if code != http.StatusUnprocessableEntity || out["reason"] != "accelerator_unreachable" || store.created[0].ProposedAt != nil {
				t.Fatalf("got = %d %v, want 422 accelerator_unreachable and nothing stored", code, out)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(message, name) {
					t.Errorf("message %q names no %s", message, name)
				}
			}
		})
	}
}

// A planner learned of one problem per propose, and to try a variant it proposed a copy, which was
// stored and accepted. check runs the same check, stores nothing, and a refusal holds every problem.
func TestACheckedProposalIsStoredNeverAndARefusalHoldsEveryProblem(t *testing.T) {
	store := draftWithPlanner()
	_, call := planningRouter(t, store, thePlanner)
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal?check=true", aProposal(nil)); code != http.StatusOK || store.created[0].ProposedAt != nil || store.created[0].Proposal != nil || out["proposal"] != nil {
		t.Fatalf("a checked proposal that passes: got = %d %v, want 200 with the draft as it stands and nothing stored", code, out)
	}

	broken := aProposal(map[string]any{"budget_accelerator_hours": 0, "accelerator_type": "cpu", "baseline_minutes": 1, "metric_reason": " ", "cpu": "lots"})
	for _, path := range []string{"/platform-experiments/pe-draft/proposal?check=true", "/platform-experiments/pe-draft/proposal"} {
		code, out := call("PUT", path, broken)
		if code != http.StatusUnprocessableEntity || store.created[0].ProposedAt != nil {
			t.Fatalf("%s: got = %d %v, want 422 and nothing stored", path, code, out)
		}
		problems, _ := out["problems"].([]any)
		var reasons []string
		for _, problem := range problems {
			reason, _ := problem.(map[string]any)["reason"].(string)
			reasons = append(reasons, reason)
		}
		for _, want := range []string{"invalid_baseline_minutes", "invalid_cost", "accelerator_unreachable", "invalid_resources", "missing_explanation"} {
			if !slices.Contains(reasons, want) {
				t.Errorf("%s: problems %v hold no %s", path, reasons, want)
			}
		}
		if message, _ := out["message"].(string); out["reason"] != reasons[0] || !strings.Contains(message, "5 problems") {
			t.Errorf("%s: reason %v and message %q, want the first problem's reason and every problem counted", path, out["reason"], message)
		}
	}

	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK || store.created[0].ProposedAt == nil {
		t.Fatalf("the proposal meant, without check: got = %d %v, want it stored", code, out)
	}
}

func (s *draftStore) SetGoal(_ context.Context, id, goal string, planUntil time.Time) error {
	for _, pe := range s.created {
		if pe.ID != id || pe.Status == domain.PlatformExpClosed || pe.Goal == "" {
			continue
		}
		pe.Goal, pe.GoalVersion = goal, pe.GoalVersion+1
		if pe.Status == domain.PlatformExpDraft {
			pe.SetAnswered(nil, false)
			pe.PlannerEndsAt = nil
			pe.SetClosesAt(planUntil)
		}
		return nil
	}
	return db.ErrNotEditable
}

func (s *draftStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, actor := range actors {
		actor.Name = "named " + actor.ID
	}
	return nil
}

func (s *draftStore) ListSignups(context.Context, string) ([]string, error) { return nil, nil }

// A person who sees the plan going the wrong way changes what they asked, and the draft goes back
// to the planner. A proposal for the goal before is never applied.
func TestChangingTheGoalGivesADraftBackToItsPlanner(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	_, call := planningRouter(t, store, planner)
	for name, c := range map[string]struct {
		body   map[string]any
		code   int
		reason string
	}{
		"nothing":            {map[string]any{"goal": "  "}, 422, "invalid_goal"},
		"an essay":           {map[string]any{"goal": strings.Repeat("x", domain.MaxGoalBytes+1)}, 422, "invalid_goal"},
		"the goal it has":    {map[string]any{"goal": " g "}, 422, "nothing_to_change"},
		"with another field": {map[string]any{"goal": "smaller", "name": "n"}, 422, "goal_alone"},
	} {
		if code, out := call("PATCH", "/platform-experiments/pe-draft", c.body); code != c.code || out["reason"] != c.reason || store.created[0].GoalVersion != 1 {
			t.Errorf("%s: got = %d %v, want %d %s and the goal left as it was", name, code, out, c.code, c.reason)
		}
	}
	before := time.Now()
	code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"goal": " It must fit one RTX 4090. "})
	if code != http.StatusOK {
		t.Fatalf("change the goal: got = %d %v, want 200", code, out)
	}
	pe := store.created[0]
	if pe.ProposedAt != nil || out["proposed_at"] != nil || out["planner_ends_at"] != nil || pe.Status != domain.PlatformExpDraft ||
		out["goal"] != "It must fit one RTX 4090." || out["goal_version"] != float64(2) || out["status_reason"].(map[string]any)["reason"] != "revising" {
		t.Errorf("goal changed: got %v, want the new goal at version 2, revising, no proposed_at and no planner_ends_at", out)
	}
	if left := pe.PlanningEndsAt.Sub(before); left < testPlanning.Limit-time.Minute || left > testPlanning.Limit+time.Minute {
		t.Errorf("the planner has %v to propose again, want the planning limit", left)
	}
	if editable, _ := out["editable"].([]any); len(editable) != 1 || editable[0] != "goal" {
		t.Errorf("a draft's editable: got %v, want goal alone", out["editable"])
	}
	// A proposal for the goal before is refused, one for the new goal opens it.
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusConflict || out["reason"] != "goal_changed" || store.created[0].ProposedAt != nil {
		t.Fatalf("proposing for the goal before: got = %d %v, want 409 goal_changed and nothing stored", code, out)
	}
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal?check=true", aProposal(nil)); code != http.StatusConflict || out["reason"] != "goal_changed" {
		t.Fatalf("checking a proposal for the goal before: got = %d %v, want 409 goal_changed", code, out)
	}
	// The planner changes no goal, and a closed draft takes none.
	if code, _ := propose("PATCH", "/platform-experiments/pe-draft", map[string]any{"goal": "m"}); code != http.StatusForbidden {
		t.Errorf("the planner changing the goal: got = %d, want 403", code)
	}
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"goal_version": 2, "budget_accelerator_hours": 4})); code != http.StatusOK || store.created[0].Status != domain.PlatformExpOpen {
		t.Fatalf("proposing for the new goal: %d %v", code, out)
	}
	store.created[0].Status = domain.PlatformExpClosed
	if code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"goal": "m"}); code != http.StatusConflict || out["reason"] != "not_editable" {
		t.Errorf("changing the goal of a closed draft: got = %d %v, want 409 not_editable", code, out)
	}
}

func (s *draftStore) Decline(_ context.Context, id string, goalVersion int, reason string, waitUntil time.Time, keepPlanner time.Duration) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpDraft {
			if pe.GoalVersion != goalVersion {
				return db.ErrGoalChanged
			}
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

// The planner's working plan is checked as a proposal and stored on the draft for its baseline
// agent to start from, and opens nothing.
func TestAWorkingPlanIsCheckedAndStoredAndOpensNothing(t *testing.T) {
	store := draftWithPlanner()
	_, call := planningRouter(t, store, thePlanner)
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal?working=true", aProposal(map[string]any{"budget_accelerator_hours": -1})); code != http.StatusUnprocessableEntity {
		t.Fatalf("a working plan that does not hold: got = %d %v, want 422", code, out)
	}
	code, out := call("PUT", "/platform-experiments/pe-draft/proposal?working=true", aProposal(nil))
	if code != http.StatusOK || out["status"] != "draft" {
		t.Fatalf("a working plan: got = %d %v, want 200 and the draft", code, out)
	}
	pe := store.created[0]
	var stored platformexperiments.Proposal
	if err := json.Unmarshal(pe.Proposal, &stored); err != nil || stored.BudgetAcceleratorHours != 6 || pe.Status != domain.PlatformExpDraft || pe.ProposedAt != nil || len(store.acceptedAgents) != 0 {
		t.Fatalf("stored %+v (%v) on a draft of status %s answered at %v with %d agents, want the plan stored and nothing opened", stored, err, pe.Status, pe.ProposedAt, len(store.acceptedAgents))
	}
	if code, out := call("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK || out["status"] != "open" {
		t.Fatalf("the proposal after a working plan: got = %d %v, want it open", code, out)
	}
}

// A goal the planner could not plan ended as a draft that closed at its limit with nothing on
// its page. The planner says why, and the person answers it by changing the goal.
func TestAPlannerDeclinesWithAReasonThePersonReadsAndCanAnswer(t *testing.T) {
	store := draftWithPlanner()
	_, asPlanner := planningRouter(t, store, thePlanner)
	_, call := planningRouter(t, store, planner)
	for name, reason := range map[string]string{"no reason": " ", "an essay": strings.Repeat("x", platformexperiments.MaxDeclineBytes+1)} {
		if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": reason, "goal_version": 1}); code != http.StatusUnprocessableEntity || store.created[0].DeclinedReason != "" {
			t.Errorf("%s: got = %d %v, want 422 and nothing stored", name, code, out)
		}
	}
	for name, caller := range map[string]auth.Principal{
		"an executor of it":    {Kind: auth.KindAgent, PlatformExperimentID: "pe-draft", AgentID: "ag-executor", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
		"the person who asked": planner,
	} {
		_, as := planningRouter(t, store, caller)
		if code, _ := as("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "r", "goal_version": 1}); code != http.StatusForbidden || store.created[0].DeclinedReason != "" {
			t.Errorf("%s declining: got = %d, want 403", name, code)
		}
	}

	before := time.Now()
	const why = "\"Make the model better\" names nothing to measure: say which model, on what data, and what better means."
	code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "  " + why + "\n", "goal_version": 1})
	if code != http.StatusOK || out["declined_reason"] != why || out["declined_at"] == nil || out["proposed_at"] != nil || out["proposal"] != nil || out["status"] != "draft" || out["status_reason"].(map[string]any)["reason"] != "declined" {
		t.Fatalf("decline: got = %d %v, want the reason, when it declined, and nothing proposed", code, out)
	}
	if _, read := call("GET", "/platform-experiments/pe-draft", nil); read["status_reason"].(map[string]any)["reason"] != "declined" {
		t.Errorf("a declined draft read: status_reason = %v, want declined", read["status_reason"])
	}
	if wait := store.created[0].ProposalExpiresAt.Sub(before); wait < testPlanning.ProposedFor-time.Minute {
		t.Errorf("a declined draft ends in %v, want it to wait for the person", wait)
	}
	// The person says what was missing in the goal; the reason stays until the planner answers again.
	code, out = call("PATCH", "/platform-experiments/pe-draft", map[string]any{"goal": "The small transformer in train.py, by validation loss."})
	if code != http.StatusOK || out["proposed_at"] != nil || out["declined_at"] != nil || out["declined_reason"] != why {
		t.Fatalf("changing the goal of a declined draft: got = %d %v, want it back with the planner and the reason kept", code, out)
	}
	if _, read := call("GET", "/platform-experiments/pe-draft", nil); read["status_reason"].(map[string]any)["reason"] != "revising" {
		t.Errorf("a declined draft planned again: status_reason = %v, want revising", read["status_reason"])
	}
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "r", "goal_version": 1}); code != http.StatusConflict || out["reason"] != "goal_changed" {
		t.Errorf("declining the goal before: got = %d %v, want 409 goal_changed", code, out)
	}
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"goal_version": 2})); code != http.StatusOK || out["declined_reason"] != nil || out["proposal"] == nil {
		t.Fatalf("proposing after a decline: got = %d %v, want the proposal and no reason", code, out)
	}

	store.created[0].Status = domain.PlatformExpOpen
	if code, out := asPlanner("PUT", "/platform-experiments/pe-draft/decline", map[string]any{"reason": "r", "goal_version": 2}); code != http.StatusConflict || out["reason"] != "not_a_draft" {
		t.Errorf("declining what is no draft: got = %d %v, want 409 not_a_draft", code, out)
	}
}

// An accepted draft waited for its coordinator three times before anything ran: to add the agent
// that measures the baseline, to notice it had finished, and to declare its job. One coordinator
// that ended its turn early left a paid experiment open until its baseline limit. From the accept
// to the running experiment nobody but the baseline agent acts.
func TestAnAcceptedDraftRunsOnceItsBaselineAgentFinishesWithNoCoordinatorAction(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	svc, _ := planningRouter(t, store, planner)
	if out := store.created[0]; out.Status != domain.PlatformExpOpen {
		t.Fatalf("proposed: %s, want it open with its baseline agent measuring", out.Status)
	}
	if len(store.baselines) != 1 || len(store.agentIDs) != 2 {
		t.Fatalf("accepted with %d baseline agents and %d agents, want 1 and 2", len(store.baselines), len(store.agentIDs))
	}
	measures := store.baselines[0].AgentID
	measured := measuredBaseline()
	measured.Job.AgentID, measured.Job.PlatformExperimentID, measured.Job.Branch, measured.Reported = measures, "pe-draft", "hl/pe-draft/baseline", map[string]float64{"val_loss": 1.8}
	svc.WithBaselineRead(func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*platformexperiments.Baseline, error) {
		if jobID != measured.Job.ID {
			return nil, nil
		}
		return measured, nil
	})
	ctx := context.Background()

	// A baseline agent that gives up leaves nothing to declare, and the experiment says whose move it is.
	gaveUp := *store
	gaveUp.baselines = append([]domain.AgentSignup{}, store.baselines...)
	gaveUp.created = []*domain.PlatformExperiment{store.created[0]}
	gaveUpSvc, _ := planningRouter(t, &gaveUp, planner)
	if _, err := gaveUpSvc.FinishAgent(ctx, "pe-draft", measures, "train.py does not run", "", ""); err != nil {
		t.Fatalf("a baseline agent finishing with no job: %v", err)
	}
	left := gaveUp.created[0]
	if err := gaveUpSvc.Describe(ctx, left, planner); err != nil || left.Status != domain.PlatformExpOpen || left.StatusReason.Reason != "baseline_required" ||
		!strings.Contains(left.StatusReason.Message, "train.py does not run") {
		t.Fatalf("after a finish that left no job: %s %+v, %v, want it open, baseline_required, with the reason the agent gave", left.Status, left.StatusReason, err)
	}

	// The one that measured: its finish declares its job and starts the run.
	store.left = []*domain.Experiment{measured.Job}
	finished, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured", "", "")
	if err != nil || finished.FinishedAt == nil {
		t.Fatalf("the baseline agent finishing: %+v, %v", finished, err)
	}
	pe := store.created[0]
	if pe.BaselineJobID != "job-1" || pe.Status != domain.PlatformExpRunning || store.started != 1 || pe.JobEstimatedDurationHours != platformexperiments.BaselineHeadroom*measured.ObservedHours ||
		!strings.Contains(pe.Description, platformexperiments.MeasuredBaselineHeader+"\njob: job-1") {
		t.Fatalf("after the finish: baseline %q, status %s, %d starts, estimate %v, brief %q: want job-1 declared as a coordinator's PATCH declares it and the run started",
			pe.BaselineJobID, pe.Status, store.started, pe.JobEstimatedDurationHours, pe.Description)
	}
	// Finishing again changes nothing.
	if _, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured", "", ""); err != nil || store.started != 1 {
		t.Fatalf("a second finish: %v with %d starts, want nothing changed", err, store.started)
	}
}

// A baseline agent reran the control after changing its environment and ran a check from a
// fresh checkout, so it left three jobs that report the ranking metric and nothing was declared.
// Its finish names the control, which is declared as a PATCH declares it; a job that could not
// be is refused before the finish is recorded, so the agent fixes it and finishes again.
func TestABaselineAgentNamesTheJobItsFinishDeclares(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	svc, _ := planningRouter(t, store, planner)
	measures := store.baselines[0].AgentID
	job := func(id, agent string, status domain.ExperimentStatus) *platformexperiments.Baseline {
		b := measuredBaseline()
		b.Job.ID, b.Job.AgentID, b.Job.PlatformExperimentID, b.Job.Branch, b.Job.Status, b.Reported = id, agent, "pe-draft", "hl/pe-draft/baseline", status, map[string]float64{"val_loss": 1.8}
		return b
	}
	jobs := map[string]*platformexperiments.Baseline{
		"job-first": job("job-first", measures, domain.StatusCompleted), "job-control": job("job-control", measures, domain.StatusCompleted),
		"job-check": job("job-check", measures, domain.StatusCompleted), "job-failed": job("job-failed", measures, domain.StatusFailed),
		"job-other": job("job-other", "ag-other", domain.StatusCompleted),
	}
	// A baseline agent's check ran from a branch beside the baseline's, and only the baseline's is declared.
	jobs["job-check"].Job.Branch = "hl/pe-draft/baseline-check"
	svc.WithBaselineRead(func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*platformexperiments.Baseline, error) {
		return jobs[jobID], nil
	})
	store.left = []*domain.Experiment{jobs["job-first"].Job, jobs["job-control"].Job, jobs["job-check"].Job}
	ctx := context.Background()

	for named, reason := range map[string]string{"job-failed": "baseline_not_completed", "job-other": "no_baseline", "job-missing": "no_baseline", "job-check": "baseline_not_on_branch"} {
		_, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured", "", named)
		var refused *refusal.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != http.StatusUnprocessableEntity || refused.Reason != reason {
			t.Errorf("a finish naming %s: %v, want 422 %s", named, err, reason)
		}
		if store.baselines[0].FinishedAt != nil || store.created[0].BaselineJobID != "" {
			t.Fatalf("a finish naming %s was refused and still recorded: finished %v, baseline %q", named, store.baselines[0].FinishedAt, store.created[0].BaselineJobID)
		}
	}
	var refused *refusal.Refusal
	if _, err := svc.FinishAgent(ctx, "pe-draft", "ag-executor", "done", "", "job-control"); !errors.As(err, &refused) || refused.Reason != "not_a_baseline_agent" {
		t.Errorf("an executor naming a baseline: %v, want 422 not_a_baseline_agent", err)
	}

	finished, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured by job-control", "", "job-control")
	if err != nil || finished.FinishedAt == nil {
		t.Fatalf("the finish naming the control: %+v, %v", finished, err)
	}
	pe := store.created[0]
	if pe.BaselineJobID != "job-control" || pe.Status != domain.PlatformExpRunning || store.started != 1 || !strings.Contains(pe.Description, platformexperiments.MeasuredBaselineHeader+"\njob: job-control") {
		t.Fatalf("after the finish: baseline %q, status %s, %d starts: want job-control declared and the run started", pe.BaselineJobID, pe.Status, store.started)
	}
	if _, err := svc.FinishAgent(ctx, "pe-draft", measures, "again", "", "job-control"); err != nil || store.started != 1 {
		t.Errorf("finishing again with the same job: %v with %d starts, want nothing changed", err, store.started)
	}
	if _, err := svc.FinishAgent(ctx, "pe-draft", measures, "again", "", "job-check"); !errors.As(err, &refused) || refused.Reason != "baseline_declared" {
		t.Errorf("finishing again naming another job: %v, want 409 baseline_declared", err)
	}
}

// A baseline agent declared the default configuration of the control while its own check,
// on the branch beside the baseline's, had measured a better one. A finish naming a job that
// another completed job of the same agent beat on the ranking metric, holding every constraint,
// is refused naming that job and its value. A job that broke a constraint, another agent's job and
// a tie do not count. The agent finishes naming the job of the stronger configuration.
func TestABaselineAgentDeclaresNoJobItsOwnJobsBeat(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	svc, _ := planningRouter(t, store, planner)
	measures := store.baselines[0].AgentID
	jobs := map[string]*platformexperiments.Baseline{}
	job := func(id, agent, branch string, valLoss float64) {
		b := measuredBaseline()
		b.Job.ID, b.Job.AgentID, b.Job.PlatformExperimentID, b.Job.Branch, b.Reported = id, agent, "pe-draft", "hl/pe-draft/"+branch, map[string]float64{"val_loss": valLoss}
		jobs[id] = b
		store.completed = append(store.completed, b.Job)
	}
	job("job-default", measures, "baseline", 1.8)
	job("job-tie", measures, "baseline", 1.8)
	job("job-check", measures, "baseline-check", 1.7)
	job("job-other", "ag-other", "baseline", 1.2)
	svc.WithBaselineRead(func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*platformexperiments.Baseline, error) {
		return jobs[jobID], nil
	})
	ctx := context.Background()

	_, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured", "", "job-default")
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.Reason != "baseline_weaker_than_own_job" || !strings.Contains(refused.Message, "job-check") || !strings.Contains(refused.Message, "val_loss = 1.7") {
		t.Fatalf("a finish naming the default its check beat: %v, want 422 baseline_weaker_than_own_job naming job-check and 1.7", err)
	}
	if store.baselines[0].FinishedAt != nil || store.created[0].BaselineJobID != "" {
		t.Fatalf("a refused finish was recorded")
	}

	job("job-strong", measures, "baseline", 1.7)
	finished, err := svc.FinishAgent(ctx, "pe-draft", measures, "the baseline is measured by job-strong", "", "job-strong")
	if err != nil || finished.FinishedAt == nil || store.created[0].BaselineJobID != "job-strong" {
		t.Fatalf("the finish naming the stronger configuration: %+v, %v, baseline %q", finished, err, store.created[0].BaselineJobID)
	}
}

// The platform recorded a baseline agent's finish and answered it 500, when declaring its job failed
// after the finish was stored. A stored finish is answered as one.
func TestAStoredFinishIsAnsweredAsOneWhenItsDeclarationFails(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	svc, _ := planningRouter(t, store, planner)
	measured := measuredBaseline()
	store.left = []*domain.Experiment{measured.Job}
	svc.WithBaselineRead(func(context.Context, *domain.PlatformExperiment, string) (*platformexperiments.Baseline, error) {
		return nil, errors.New("the metrics store does not answer")
	})
	finished, err := svc.FinishAgent(context.Background(), "pe-draft", store.baselines[0].AgentID, "the baseline is measured", "", "")
	if err != nil || finished == nil || finished.FinishedAt == nil || store.created[0].Status != domain.PlatformExpOpen {
		t.Fatalf("a finish whose declaration failed: %+v, %v, status %s: want it answered as finished and the experiment left open", finished, err, store.created[0].Status)
	}
	// Named, the same failure comes before anything is stored, and the finish is retried.
	store.baselines[0].FinishedAt = nil
	if _, err := svc.FinishAgent(context.Background(), "pe-draft", store.baselines[0].AgentID, "the baseline is measured", "", "job-1"); err == nil || store.baselines[0].FinishedAt != nil {
		t.Errorf("a named job that could not be read: %v, finished %v, want an error and nothing recorded", err, store.baselines[0].FinishedAt)
	}
}

// A planner listed what the goal left open ("no margin was stated, so I set 1.5x") and the run
// waited for a person to confirm it. The answers are the assumptions the run starts on: the
// proposal opens with them added to the brief, where a person changes them.
func TestAProposalWithQuestionsOpensWithItsAnswersInTheBrief(t *testing.T) {
	questions := []map[string]any{
		{"question": "How big a win counts?", "answer": "1.5x the control's throughput."},
		{"question": "Which batch sizes are held equal?", "answer": "32 only."},
	}
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(map[string]any{"questions": questions})); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpOpen {
		t.Fatalf("proposed with questions: status %s, want open", pe.Status)
	}
	want := "Assumptions the plan runs on:\n- How big a win counts? Answer: 1.5x the control's throughput.\n- Which batch sizes are held equal? Answer: 32 only."
	if !strings.HasPrefix(pe.Description, "Train the small transformer") || !strings.HasSuffix(pe.Description, "\n\n"+want) {
		t.Errorf("brief %q, want the proposal's brief followed by\n%s", pe.Description, want)
	}
	if pe.AcceptedBy != nil || pe.AcceptedAt == nil || !slices.Equal(pe.AcceptedAnswers, []string{"1.5x the control's throughput.", "32 only."}) {
		t.Errorf("applied by %+v at %v with %q, want nobody, now, and the planner's answers", pe.AcceptedBy, pe.AcceptedAt, pe.AcceptedAnswers)
	}

	// The proposal and its questions stay to be read by anyone who reads the experiment, and by
	// nobody of another org.
	reader := auth.Principal{Kind: auth.KindUser, UserID: "u-reader", OrgIDs: []string{"org-a"}}
	_, read := planningRouter(t, store, reader)
	code, out := read("GET", "/platform-experiments/pe-draft", nil)
	proposal, _ := out["proposal"].(map[string]any)
	asked, _ := proposal["questions"].([]any)
	if first, _ := asked[0].(map[string]any); code != http.StatusOK || len(asked) != 2 || first["answer"] != "1.5x the control's throughput." || fmt.Sprint(out["accepted_answers"]) != "[1.5x the control's throughput. 32 only.]" {
		t.Errorf("a reader of the org: got = %d, questions %v, accepted_answers %v, want the proposal's questions and the answers", code, proposal["questions"], out["accepted_answers"])
	}
	_, foreign := planningRouter(t, store, auth.Principal{Kind: auth.KindUser, UserID: "u-foreign", OrgIDs: []string{"org-b"}})
	if code, out := foreign("GET", "/platform-experiments/pe-draft", nil); code != http.StatusNotFound || out["proposal"] != nil {
		t.Errorf("a user of another org: got = %d %v, want 404", code, out)
	}
}

// A planner chose a lever a closed sibling run had refuted, because it read only the run its
// draft continues. The read of a platform experiment lists every prior run its caller reaches.
func TestTheReadOfAPlatformExperimentListsThePriorRunsItsCallerReaches(t *testing.T) {
	store := draftWithPlanner()
	closedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	store.created = append(store.created,
		&domain.PlatformExperiment{ID: "pe-prior", ProjectID: "prj-a", Name: "round 13", Status: domain.PlatformExpClosed, ClosedAt: &closedAt, CloseReason: domain.CloseByRequest},
		&domain.PlatformExperiment{ID: "pe-foreign", ProjectID: "prj-b", Name: "theirs", Status: domain.PlatformExpClosed, ClosedAt: &closedAt})
	store.prior = []string{"pe-prior", "pe-foreign"}
	reader := thePlanner
	reader.PriorRuns = []string{"pe-prior"}
	_, call := planningRouter(t, store, reader)
	code, out := call("GET", "/platform-experiments/pe-draft", nil)
	prior, _ := out["prior_runs"].([]any)
	if code != http.StatusOK || len(prior) != 1 {
		t.Fatalf("got = %d with prior_runs %v, want 200 and the one prior run the planner reaches", code, out["prior_runs"])
	}
	run := prior[0].(map[string]any)
	if run["id"] != "pe-prior" || run["name"] != "round 13" || run["status"] != "closed" || run["close_reason"] != "closed_by_request" ||
		run["closed_at"] != closedAt.Format(time.RFC3339) || run["best"] != nil {
		t.Errorf("prior run = %v, want pe-prior as stored, with no best where it ranked nothing", run)
	}
}

const createMaxAgentSandboxes = 4

// fakeRepositories is GitHub as the org of each project reaches it: files holds, by project, the
// experiment file of the one repository its org's installation covers, acme/model, which GitHub
// spells Acme/Model.
type fakeRepositories struct {
	files   map[string]string
	refusal error
	checked []string
	asked   []string
	// created holds each repository made from the experiment template, as "project repository".
	created    []string
	notCreated error
	// notOn is what refuses an account before anything is created.
	notOn error
}

func (f *fakeRepositories) Create(_ context.Context, projectID, repository, _ string) (string, string, error) {
	if f.notCreated != nil {
		return "", "", f.notCreated
	}
	f.created = append(f.created, projectID+" "+repository)
	return repository, "main", nil
}

func (f *fakeRepositories) HasBranch(context.Context, string, string, string) (bool, error) {
	return true, nil
}

// CreatesOn answers the account named, or acme, the only one the org connected, and notOn when
// that is set.
func (f *fakeRepositories) CreatesOn(_ context.Context, _, account string) (string, error) {
	if f.notOn != nil {
		return "", f.notOn
	}
	if account == "" {
		return "acme", nil
	}
	return account, nil
}

const testRepository = "Acme/Model"

func notReachable() error {
	return refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "the GitHub installation the org connected does not cover that repository")
}

func (f *fakeRepositories) Reachable(_ context.Context, projectID, repository string) (string, error) {
	f.checked = append(f.checked, projectID+" "+repository)
	if _, ok := f.files[projectID]; !ok || !strings.EqualFold(repository, testRepository) {
		return "", notReachable()
	}
	return testRepository, nil
}

func (f *fakeRepositories) URL(repository string) string {
	return "https://github.com/" + repository + ".git"
}

func (f *fakeRepositories) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	panic("the handler asked for a git credential")
}

func (f *fakeRepositories) Serves(string, string, string, string) bool { return false }

func (f *fakeRepositories) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, githost.ErrFileNotFound
}

func (f *fakeRepositories) File(_ context.Context, projectID, repository, _, _ string) ([]byte, error) {
	f.asked = append(f.asked, projectID+" "+repository)
	if f.refusal != nil {
		return nil, f.refusal
	}
	return []byte(f.files[projectID]), nil
}

type createdStore struct {
	fakeStore
	created []*domain.PlatformExperiment
	// prior are the prior runs of every platform experiment it holds.
	prior []string
	// messages are the messages sent to its agents.
	messages []domain.AgentMessage
	// agentIDs are the agents of the team that joined, and team their signups.
	agentIDs []string
	team     []domain.AgentSignup
	// baselines are the agents of role baseline that joined, and left the COMPLETED jobs on the
	// objective of those of them that finished.
	baselines []domain.AgentSignup
	left      []*domain.Experiment
	// objective are the jobs on the objective, oldest first.
	objective []*domain.Experiment
	// completed is every COMPLETED job, oldest first.
	completed []*domain.Experiment
	// atWork is the agents at work by agent type and model, as the store counts them.
	atWork map[domain.PoolKey]int
	// started counts the starts that reached the store.
	started int
	// answered is when a launcher last asked for its machines, by name. One not in it asked now.
	answered map[string]*time.Time
}

func (s *createdStore) awaiting(pe *domain.PlatformExperiment) bool {
	return pe.Status != domain.PlatformExpClosed && pe.AwaitsRepository()
}

func (s *createdStore) PriorRuns(context.Context, string) ([]string, error) {
	return s.prior, nil
}

func (s *createdStore) RunsToRank(_ context.Context, ids []string) ([]db.RunToRank, error) {
	var out []db.RunToRank
	for _, id := range ids {
		for _, pe := range s.created {
			if pe.ID == id {
				out = append(out, db.RunToRank{PE: pe})
			}
		}
	}
	return out, nil
}

func (s *createdStore) RepositoriesToCreate(context.Context) ([]db.RepositoryToCreate, error) {
	var out []db.RepositoryToCreate
	for _, pe := range s.created {
		if s.awaiting(pe) {
			out = append(out, db.RepositoryToCreate{ID: pe.ID, ProjectID: pe.ProjectID, Name: pe.Name, Account: pe.RepositoryAccount})
		}
	}
	return out, nil
}

func (s *createdStore) GiveRepository(_ context.Context, id, repository, url, branch string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Repository, pe.RepositoryURL, pe.Branch = repository, url, branch
			return nil
		}
	}
	return db.ErrRepositoryNotAwaited
}

func (s *createdStore) FailRepository(_ context.Context, id, answer string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Status, pe.CloseReason, pe.RepositoryError = domain.PlatformExpClosed, domain.CloseRepositoryNotCreated, answer
		}
	}
	return nil
}

func (s *createdStore) ListExperimentsTesting(context.Context, []string) ([]*domain.Experiment, error) {
	return s.objective, nil
}

func (s *createdStore) FinishedBaselineJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.left, nil
}

func (s *createdStore) CompletedJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.completed, nil
}

// StartPlatformExperimentTx starts the open platform experiment id with an executor at work.
func (s *createdStore) StartPlatformExperimentTx(_ context.Context, id string, _ time.Duration, quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	participants := make([]db.StartParticipant, len(s.agentIDs))
	for i, agentID := range s.agentIDs {
		participants[i] = db.StartParticipant{AgentID: agentID, Role: domain.SignupRoleExecutor, AgentExists: true, QuotaTier: domain.QuotaTierGuaranteed}
	}
	quotas, _, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
	}
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status, pe.StartedAt = domain.PlatformExpRunning, time.Now().UTC()
			pe.SetClosesAt(pe.StartedAt.Add(time.Hour))
		}
	}
	s.started++
	return true, quotas, nil
}

func (s *createdStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleExecutor {
		return nil, nil
	}
	return s.agentIDs, nil
}

func (s *createdStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, _ []*domain.Agent, signups []domain.AgentSignup) error {
	s.created = append(s.created, pe)
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			s.agentIDs = append(s.agentIDs, signup.AgentID)
			s.team = append(s.team, signup)
			continue
		}
		signup.Name = "Baseline 1"
		s.baselines = append(s.baselines, signup)
	}
	return nil
}

func (s *createdStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	for _, pe := range s.created {
		if pe.ID == id {
			out := *pe
			return &out, nil
		}
	}
	return nil, nil
}

func (s *createdStore) AgentsAtWork(context.Context, string) (map[domain.PoolKey]int, error) {
	return s.atWork, nil
}

func (s *createdStore) ListSignups(context.Context, string) ([]string, error) { return nil, nil }

func (s *createdStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	signups := make([]domain.AgentSignup, len(s.agentIDs))
	for i, agentID := range s.agentIDs {
		signups[i] = domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleExecutor}
	}
	return append(signups, s.baselines...), nil
}

// UpdatePlatformExperiment holds max_agents to the agents that joined, every one at work.
func (s *createdStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, pool domain.AgentPool) error {
	return s.update(pe, status, pool, len(s.agentIDs))
}

// update refuses a max_agents below atWork before the run starts; once it runs the store cuts the
// newest above it.
func (s *createdStore) update(pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, pool domain.AgentPool, atWork int) error {
	if pe.MaxAgents < atWork && status != domain.PlatformExpRunning {
		return &db.PoolBelowAtWorkError{Count: pe.MaxAgents, AtWork: atWork}
	}
	if pool != nil {
		pe.Agents = pool
	}
	for i, stored := range s.created {
		if stored.ID == pe.ID {
			s.created[i] = pe
		}
	}
	return nil
}

// run is a whole create body naming the repository acme/model, with more laid over it.
func run(more map[string]any) map[string]any {
	body := map[string]any{
		"project_id": "prj-a", "repository": "acme/model", "branch": "main", "name": "tune the model", "description": "beat the baseline in the README",
		"budget_accelerator_hours": 5, "max_agents": 3, "duration_hours": 48, "report_interval_seconds": 30,
		"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize"}},
		"stages":  []map[string]any{{"length_pct": 100, "evict_pct": 0}},
	}
	for key, value := range more {
		body[key] = value
	}
	return body
}

func (s *createdStore) RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error {
	raise.At = time.Now().UTC()
	pe.BaselineRaises = append(pe.BaselineRaises, raise)
	return s.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil)
}

func (s *createdStore) ListAgentMessages(context.Context, string, string) ([]domain.AgentMessage, error) {
	return s.messages, nil
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
	store.agentIDs = []string{"ag-1", "ag-2"}
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

// A proposal stored while proposals waited for a person, before this release, is applied once
// when the platform starts, as the platform would have accepted it.
func TestAStandingProposalIsAppliedOnce(t *testing.T) {
	store := draftWithPlanner()
	proposal, err := json.Marshal(aProposal(nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store.created[0].Proposal = proposal
	store.created[0].SetAnswered(&now, true)
	svc, _ := planningRouter(t, store, planner)
	svc.ApplyStanding(context.Background(), time.Minute)
	if pe := store.created[0]; pe.Status != domain.PlatformExpOpen || pe.BudgetAcceleratorHours != 6 || len(store.baselines) != 1 {
		t.Fatalf("after the pass: %s with budget %v and %d baseline agents, want it open as proposed", pe.Status, pe.BudgetAcceleratorHours, len(store.baselines))
	}
	declined := draftWithPlanner()
	declined.created[0].DeclinedReason = "nothing to measure"
	declined.created[0].SetAnswered(&now, false)
	svc, _ = planningRouter(t, declined, planner)
	svc.ApplyStanding(context.Background(), time.Minute)
	if pe := declined.created[0]; pe.Status != domain.PlatformExpDraft {
		t.Fatalf("a declined draft after the pass: %s, want a draft still", pe.Status)
	}
}

// The machine the planner chose is the plan's, which the person changes while the experiment is
// open: checked as a proposal's is, a change between an accelerator and CPU cores states the
// budget in its new unit, and the baseline is measured again in a fresh window.
func TestThePlansMachineChangesWhileItIsOpen(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	_, call := planningRouter(t, store, planner)
	if code, out := call("GET", "/platform-experiments/pe-draft", nil); code != http.StatusOK || !slices.Contains(out["editable"].([]any), "cpu") || slices.Contains(out["editable"].([]any), "max_agents") {
		t.Fatalf("editable of the open experiment: %d %v, want the machine and no max_agents", code, out["editable"])
	}
	due := store.created[0].BaselineDueAt
	if code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"cpu": "4"}); code != http.StatusOK {
		t.Fatalf("a machine of fewer cores: %d %v", code, out)
	}
	machine, ok := domain.PlanMachine(store.created[0].Proposal)
	if !ok || machine.CPU != "4" || machine.AcceleratorType != "rtx-4090" || !store.created[0].BaselineDueAt.After(due) {
		t.Fatalf("after the change: machine %+v, baseline_due_at %v from %v, want 4 cores on the same type and a later window", machine, store.created[0].BaselineDueAt, due)
	}
	for name, c := range map[string]struct {
		body   map[string]any
		reason string
	}{
		"CPU cores with the budget unstated": {map[string]any{"accelerator_type": ""}, "budget_unit_changed"},
		"a type nobody has":                  {map[string]any{"accelerator_type": "a100"}, "accelerator_unreachable"},
		"memory that is no quantity":         {map[string]any{"memory": "lots"}, "invalid_resources"},
	} {
		if code, out := call("PATCH", "/platform-experiments/pe-draft", c.body); code != http.StatusUnprocessableEntity || out["reason"] != c.reason {
			t.Errorf("%s: %d %v, want 422 %s", name, code, out, c.reason)
		}
	}
	store.reachable[string(domain.CPUType)] = []domain.Cluster{{Name: "rented"}}
	if code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"accelerator_type": "", "budget_accelerator_hours": 40}); code != http.StatusOK || out["budget_kind"] != "cpu" {
		t.Errorf("CPU cores with a budget of core-hours: %d %v, want 200 and a budget of cpu", code, out)
	}
}

// A change of the plan's machine cancels the baseline jobs that ask for the machine before, so
// nobody pays for work that can no longer count. An executor's job and a baseline job already
// on the new machine keep running.
func TestAMachineChangeCancelsTheBaselineJobsOnTheMachineBefore(t *testing.T) {
	store := draftWithPlanner()
	_, propose := planningRouter(t, store, thePlanner)
	if code, out := propose("PUT", "/platform-experiments/pe-draft/proposal", aProposal(nil)); code != http.StatusOK {
		t.Fatalf("propose: %d %v", code, out)
	}
	baseline := store.baselines[0].AgentID
	on := func(id, agent, cpu string) *domain.Experiment {
		return &domain.Experiment{ID: id, PlatformExperimentID: "pe-draft", AgentID: agent, Status: domain.StatusRunning,
			Job: domain.JobSpec{AcceleratorType: "rtx-4090", AcceleratorCount: 1, CPU: cpu, Memory: "32Gi", Storage: "100Gi"}}
	}
	store.active = []*domain.Experiment{on("job-before", baseline, "8"), on("job-new", baseline, "4"), on("job-executor", "ag-executor", "8")}
	svc, call := planningRouter(t, store, planner)
	svc.WithJobCancel(store)
	if code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"cpu": "4"}); code != http.StatusOK {
		t.Fatalf("a machine of fewer cores: %d %v", code, out)
	}
	if !slices.Equal(store.cancelled, []string{"job-before"}) {
		t.Errorf("cancelled %v, want only the baseline job on the machine before", store.cancelled)
	}
	store.cancelled = nil
	if code, out := call("PATCH", "/platform-experiments/pe-draft", map[string]any{"cpu": "4"}); code != http.StatusOK || len(store.cancelled) != 0 {
		t.Errorf("the machine it has: %d %v with %v cancelled, want nothing cancelled", code, out, store.cancelled)
	}
}
