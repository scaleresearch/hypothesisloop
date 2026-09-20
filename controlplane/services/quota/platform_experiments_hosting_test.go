package quota

import (
	"reflect"
	"strings"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const hostingMaxHosted = 4

type hostingStore struct {
	createdStore
	spawned  []domain.AgentSignup
	spawnErr error
	// unregistered is a launcher config names and no operator registered.
	unregistered bool
}

func (s *hostingStore) LauncherKnown(context.Context, string) (bool, error) {
	return !s.unregistered, nil
}

func (s *hostingStore) CreatePlatformExperimentWithAgents(ctx context.Context, pe *domain.PlatformExperiment, agents []*domain.Agent, signups []domain.AgentSignup) error {
	if s.spawnErr != nil {
		return s.spawnErr
	}
	if len(agents) != len(signups) {
		return errors.New("agents and signups differ in number")
	}
	hosted := len(signups)
	if pe.CoordinatorHosted {
		hosted++
	}
	if hosted > pe.MaxHosted {
		return db.ErrMaxHostedReached
	}
	s.spawned = append(s.spawned, signups...)
	return s.createdStore.CreatePlatformExperimentWithAgents(ctx, pe, agents, signups)
}

func (s *hostingStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	return s.spawned, nil
}

func hostingAPI(t *testing.T, repos *fakeProjectRepos) (*hostingStore, func(body map[string]any) (int, map[string]any)) {
	t.Helper()
	store := &hostingStore{}
	svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{Projects: map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}}, PlatformExperiments: map[string]domain.Tenancy{}}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", ownerOfA, auth.NewAccess(placement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(svc, repos, zap.NewNop()).WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "model-default", SigningSecret: "a-secret-only-this-test-knows-00", MaxHosted: hostingMaxHosted})
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), handler)
	return store, func(body map[string]any) (int, map[string]any) {
		t.Helper()
		var payload bytes.Buffer
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("POST", "/platform-experiments", &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("answer %d %q is not JSON: %v", rec.Code, rec.Body, err)
		}
		return rec.Code, out
	}
}

func TestCreateWithAHostedCoordinatorAndAgentsAsksTheConfiguredLauncherForThem(t *testing.T) {
	store, create := hostingAPI(t, &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}})
	code, out := create(run(map[string]any{"coordinator_hosted": true, "hosted_agents": 2}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	pe := store.created[0]
	if !pe.CoordinatorHosted || pe.Coordinator == nil || pe.Coordinator.Launcher != "main" || pe.Coordinator.Model != "model-default" {
		t.Fatalf("created = %+v (coordinator %+v), want one on the configured launcher and default model", pe, pe.Coordinator)
	}
	if out["coordinator_hosted"] != true || out["signup_count"] != float64(2) {
		t.Errorf("answer = %v, want coordinator_hosted and two agents", out)
	}
	if len(store.spawned) != 2 || store.spawned[0].AgentID == "" || store.spawned[0].AgentID == store.spawned[1].AgentID {
		t.Fatalf("spawned = %+v, want two agents of their own ids", store.spawned)
	}
	for _, signup := range store.spawned {
		if !signup.Hosted || signup.Launcher != "main" || signup.Model != "model-default" || signup.Flavor != domain.DefaultAgentFlavor {
			t.Errorf("spawned = %+v, want a hosted agent of the configured launcher", signup)
		}
	}

	store, create = hostingAPI(t, &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}})
	if code, out := create(run(map[string]any{"coordinator_hosted": true, "coordinator_model": "model-2"})); code != http.StatusCreated || store.created[0].Coordinator.Model != "model-2" || len(store.spawned) != 0 {
		t.Fatalf("with a model: got = %d %v, created %+v", code, out, store.created)
	}
	store, create = hostingAPI(t, &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}})
	if code, out := create(run(nil)); code != http.StatusCreated || store.created[0].CoordinatorHosted || store.created[0].Coordinator != nil || out["coordinator_hosted"] != false {
		t.Fatalf("without one: got = %d %v, created %+v", code, out, store.created[0])
	}
}

// The file is pushed to by any agent's token, and a coordinator or an agent rents a machine.
func TestWhatThePlatformRunsIsNeverReadFromTheExperimentFile(t *testing.T) {
	file := experimentBase + "coordinator_hosted: true\ncoordinator_model: model-of-the-file\nhosted_agents: 3\n"
	store, create := hostingAPI(t, &fakeProjectRepos{files: map[string]string{"prj-a": file}})
	if code, out := create(run(nil)); code != http.StatusCreated || store.created[0].CoordinatorHosted || len(store.spawned) != 0 {
		t.Fatalf("got = %d %v, created %+v, spawned %d: want nothing hosted", code, out, store.created, len(store.spawned))
	}
}

func TestCreateRefusesHostingItCannotStart(t *testing.T) {
	whole := map[string]any{
		"project_id": "prj-a", "name": "n", "description": "d", "budget_accelerator_hours": 5, "max_agents": 3, "ends_at": "2027-01-01T00:00:00Z",
		"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize"}}, "report_interval_seconds": 30, "starts_at": "2026-01-01T00:00:00Z",
	}
	with := func(more map[string]any) map[string]any {
		body := map[string]any{}
		for key, value := range whole {
			body[key] = value
		}
		for key, value := range more {
			body[key] = value
		}
		return body
	}
	for name, tc := range map[string]struct {
		more   map[string]any
		repos  *fakeProjectRepos
		status int
		reason string
	}{
		"a coordinator without a repository": {map[string]any{"coordinator_hosted": true}, &fakeProjectRepos{}, 409, "no_repository"},
		"agents without a repository":        {map[string]any{"hosted_agents": 1}, &fakeProjectRepos{}, 409, "no_repository"},
		"a model without a coordinator":      {map[string]any{"coordinator_model": "m"}, &fakeProjectRepos{files: map[string]string{"prj-a": ""}}, 400, "invalid_request"},
		"a model no name can be":             {map[string]any{"coordinator_hosted": true, "coordinator_model": "a b"}, &fakeProjectRepos{files: map[string]string{"prj-a": ""}}, 400, "invalid_request"},
		"more agents than the field holds":   {map[string]any{"hosted_agents": 4}, &fakeProjectRepos{files: map[string]string{"prj-a": ""}}, 400, ""},
		"a negative count":                   {map[string]any{"hosted_agents": -1}, &fakeProjectRepos{files: map[string]string{"prj-a": ""}}, 400, ""},
	} {
		store, create := hostingAPI(t, tc.repos)
		code, out := create(with(tc.more))
		reason, _ := out["reason"].(string)
		if code != tc.status || reason != tc.reason || len(store.created) != 0 || len(store.spawned) != 0 {
			t.Errorf("%s: got = %d %v with %d created, want %d %s and nothing created", name, code, out, len(store.created), tc.status, tc.reason)
		}
	}

	// The coordinator is a machine as each hosted agent is, and the bound is on all of them.
	attached := &fakeProjectRepos{files: map[string]string{"prj-a": ""}}
	store, create := hostingAPI(t, attached)
	code, out := create(with(map[string]any{"max_agents": 10, "coordinator_hosted": true, "hosted_agents": hostingMaxHosted}))
	if message, _ := out["message"].(string); code != http.StatusBadRequest || out["reason"] != "max_hosted_reached" || !strings.Contains(message, "at most 4 machines") || len(store.created) != 0 {
		t.Fatalf("a coordinator and %d agents: got = %d %v, want 400 max_hosted_reached and nothing created", hostingMaxHosted, code, out)
	}
	if code, out := create(with(map[string]any{"max_agents": 10, "coordinator_hosted": true, "hosted_agents": hostingMaxHosted - 1})); code != http.StatusCreated || out["max_hosted"] != float64(hostingMaxHosted) {
		t.Fatalf("a coordinator and %d agents: got = %d %v, want 201 showing the bound", hostingMaxHosted-1, code, out)
	}

	// Nothing is hosted while the launcher config names has no row: its token would be refused.
	for _, more := range []map[string]any{{"coordinator_hosted": true}, {"hosted_agents": 1}} {
		store, create = hostingAPI(t, attached)
		store.unregistered = true
		code, out := create(with(more))
		if message, _ := out["message"].(string); code != http.StatusServiceUnavailable || out["reason"] != "launcher_not_registered" || !strings.Contains(message, "hl-admin launcher add main") || len(store.created) != 0 {
			t.Fatalf("%v with no launcher registered: got = %d %v, want 503 launcher_not_registered", more, code, out)
		}
		if code, out := create(with(nil)); code != http.StatusCreated {
			t.Fatalf("nothing hosted and no launcher registered: got = %d %v, want 201", code, out)
		}
	}

	// Without hosting, a project with no repository creates as before.
	store, create = hostingAPI(t, &fakeProjectRepos{})
	if code, out := create(with(nil)); code != http.StatusCreated || len(store.created) != 1 {
		t.Fatalf("nothing hosted and no repository: got = %d %v, want 201", code, out)
	}

	// The experiment and its agents are one write: when it fails neither exists, and the answer
	// is the platform's own failure.
	store, create = hostingAPI(t, &fakeProjectRepos{files: map[string]string{"prj-a": ""}})
	store.spawnErr = errors.New("the database is away")
	if code, out := create(with(map[string]any{"hosted_agents": 2})); code != http.StatusInternalServerError || len(store.created) != 0 || len(store.spawned) != 0 {
		t.Fatalf("a write that failed: got = %d %v with %d created and %d agents, want 500 and nothing", code, out, len(store.created), len(store.spawned))
	}
}

// A coordinator's token is in the hands of a machine the platform rents. It keeps the brief, and
// what the run may spend and how long it lasts stay with whoever created it.
func TestACoordinatorsTokenChangesTheDescriptionAndNothingARunsCostFollowsFrom(t *testing.T) {
	starts, ends := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)
	live := func() *domain.PlatformExperiment {
		return &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "prj-a", Name: "n", Description: "the brief", Status: domain.PlatformExpOpen,
			BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 30, StartsAt: starts, EndsAt: ends,
			Metrics:                []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize", Role: "ranking"}},
			Stages:                 []domain.Stage{{LengthPct: 100}},
			HypothesisSubmitPolicy: domain.SubmitterPolicyMixed, JobSubmitPolicy: domain.SubmitterPolicyMixed,
		}
	}
	whole := func(more map[string]any) map[string]any {
		body := map[string]any{
			"name": "n", "description": "the brief, amended", "budget_accelerator_hours": 5, "max_agents": 3, "report_interval_seconds": 30,
			"starts_at": "2030-01-01T00:00:00Z", "ends_at": "2030-02-01T00:00:00Z",
			"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize", "role": "ranking"}},
		}
		for key, value := range more {
			body[key] = value
		}
		return body
	}
	put := func(caller auth.Principal, body map[string]any) (int, map[string]any, *domain.PlatformExperiment) {
		t.Helper()
		store := &createdStore{created: []*domain.PlatformExperiment{live()}}
		svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
		placement := authtest.Placement{
			Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}},
			PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"}},
		}
		router := chi.NewRouter()
		doc := apidocs.New(router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
		RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeProjectRepos{}, zap.NewNop()).WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "m", SigningSecret: "a-secret-only-this-test-knows-00", MaxHosted: hostingMaxHosted}))
		var payload bytes.Buffer
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("PUT", "/platform-experiments/pe-1", &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, store.created[0]
	}
	coordinator := auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1"}

	code, out, stored := put(coordinator, whole(nil))
	if code != http.StatusOK || stored.Description != "the brief, amended" || stored.BudgetAcceleratorHours != 5 || !stored.EndsAt.Equal(ends) {
		t.Fatalf("a coordinator amending the description: got = %d %v, stored %+v", code, out, stored)
	}
	for field, more := range map[string]map[string]any{
		"budget_accelerator_hours":    {"budget_accelerator_hours": 500},
		"max_agents":                  {"max_agents": 30},
		"ends_at":                     {"ends_at": "2031-01-01T00:00:00Z"},
		"starts_at":                   {"starts_at": "2030-01-02T00:00:00Z"},
		"stages":                      {"stages": []map[string]any{{"length_pct": 50, "evict_pct": 50}, {"length_pct": 50, "evict_pct": 0}}},
		"max_concurrent_accelerators": {"max_concurrent_accelerators": 8},
		"report_interval_seconds":     {"report_interval_seconds": 60},
		"metrics":                     {"metrics": []map[string]any{{"key": "loss", "direction": "minimize", "role": "ranking"}}},
		"name":                        {"name": "another"},
	} {
		code, out, stored := put(coordinator, whole(more))
		message, _ := out["message"].(string)
		if code != http.StatusForbidden || out["reason"] != "coordinator_changes_description_only" || !strings.Contains(message, field) {
			t.Errorf("a coordinator changing %s: got = %d %v, want 403 naming the field", field, code, out)
		}
		if !reflect.DeepEqual(stored, live()) {
			t.Errorf("a coordinator changing %s: stored %+v, want nothing changed, the description included", field, stored)
		}
		// The same body from the people who created it is theirs to send.
		if code, out, _ := put(auth.Principal(ownerOfA), whole(more)); code != http.StatusOK {
			t.Errorf("an owner changing %s: got = %d %v, want 200", field, code, out)
		}
	}
}
