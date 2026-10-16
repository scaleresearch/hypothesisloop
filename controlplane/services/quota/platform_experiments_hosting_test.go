package quota

import (
	"reflect"
	"strings"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const hostingMaxHosted = 4

type hostingStore struct {
	createdStore
	createErr error
	// unregistered is a launcher config names and no operator registered.
	unregistered bool
}

func (s *hostingStore) LauncherKnown(context.Context, string) (bool, error) {
	return !s.unregistered, nil
}

func (s *hostingStore) CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.createdStore.CreatePlatformExperiment(ctx, pe, create, signups)
}

func hostingAPI(t *testing.T, repos *fakeRepositories) (*hostingStore, func(body map[string]any) (int, map[string]any)) {
	t.Helper()
	store := &hostingStore{}
	svc := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType)
	placement := authtest.Placement{Projects: map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}}, PlatformExperiments: map[string]domain.Tenancy{}}
	router := authtest.Router(ownerOfA)
	doc := apidocs.New(router, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(svc, repos, testPlanning, zap.NewNop()).WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "model-default", CoordinatorModel: "coordinator-default", SigningSecret: "a-secret-only-this-test-knows-00", MaxHosted: hostingMaxHosted})
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

func TestCreateWithAHostedCoordinatorGivesItToTheConfiguredLauncher(t *testing.T) {
	store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	code, out := create(run(map[string]any{"coordinator_hosted": true}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	pe := store.created[0]
	if !pe.CoordinatorHosted || pe.Coordinator == nil || pe.Coordinator.Launcher != "main" || pe.Coordinator.Model != "coordinator-default" {
		t.Fatalf("created = %+v (coordinator %+v), want one on the configured launcher and default model", pe, pe.Coordinator)
	}
	if out["coordinator_hosted"] != true || out["signup_count"] != float64(1) || out["max_hosted"] != float64(hostingMaxHosted) {
		t.Errorf("answer = %v, want coordinator_hosted, its baseline agent and the bound on machines", out)
	}
	// What accepting a draft stands up for the time before the run, a create with a hosted
	// coordinator does too: nobody adds the agent that measures the baseline.
	owner := auth.Principal(ownerOfA).Actor()
	want := domain.AgentSignup{AgentID: store.baselines[0].AgentID, Name: "Baseline 1", Role: domain.SignupRoleBaseline, Hosted: true, Launcher: "main", Flavor: "generalist", Model: "model-default", Actor: &owner}
	if len(store.baselines) != 1 || len(store.researchers) != 0 || !reflect.DeepEqual(store.baselines[0], want) {
		t.Errorf("created with %+v and researchers %v, want one hosted baseline agent added by the creator and no researcher", store.baselines, store.researchers)
	}
	if why, _ := out["status_reason"].(map[string]any); why["reason"] != "baseline_measuring" || why["waits_for"] != "platform" {
		t.Errorf("status_reason = %v, want baseline_measuring, which waits for the platform", out["status_reason"])
	}

	store, create = hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	if code, out := create(run(map[string]any{"coordinator_hosted": true, "coordinator_model": "model-2"})); code != http.StatusCreated || store.created[0].Coordinator.Model != "model-2" {
		t.Fatalf("with a model: got = %d %v, created %+v", code, out, store.created)
	}
	store, create = hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	code, out = create(run(nil))
	if code != http.StatusCreated || store.created[0].CoordinatorHosted || store.created[0].Coordinator != nil || out["coordinator_hosted"] != false {
		t.Fatalf("without one: got = %d %v, created %+v", code, out, store.created[0])
	}
	// With no coordinator of the platform's a person coordinates: no agent is created, and the
	// first answer names the command that person runs next.
	why, _ := out["status_reason"].(map[string]any)
	message, _ := why["message"].(string)
	if out["signup_count"] != float64(0) || len(store.baselines) != 0 || why["reason"] != "baseline_required" || why["waits_for"] != "owner" ||
		!strings.Contains(message, "`hl platform-experiments agents add --hosted --role baseline --platform-experiment "+store.created[0].ID+"`") {
		t.Errorf("without one: %v agents, status_reason %v, want none and baseline_required of its owner with the command", out["signup_count"], why)
	}
	// How many may join is the creator's to say, and no setting stands in for it.
	store, create = hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	unbounded := run(nil)
	delete(unbounded, "max_agents")
	if code, out := create(unbounded); code != http.StatusUnprocessableEntity || len(store.created) != 0 {
		t.Fatalf("without max_agents: got = %d %v, created %+v, want 422 and nothing", code, out, store.created)
	}
}

// The file is pushed to by any agent's token, and a coordinator rents a machine.
func TestWhatThePlatformRunsIsNeverReadFromTheExperimentFile(t *testing.T) {
	file := experimentBase + "coordinator_hosted: true\ncoordinator_model: model-of-the-file\n"
	store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": file}})
	if code, out := create(run(nil)); code != http.StatusCreated || store.created[0].CoordinatorHosted {
		t.Fatalf("got = %d %v, created %+v: want nothing hosted", code, out, store.created)
	}
}

func TestCreateRefusesHostingItCannotStart(t *testing.T) {
	whole := map[string]any{
		"project_id": "prj-a", "name": "n", "description": "d", "budget_accelerator_hours": 5, "max_agents": 3, "duration_hours": 24,
		"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize"}}, "report_interval_seconds": 30,
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
		repos  *fakeRepositories
		status int
		reason string
	}{
		"a model without a coordinator": {map[string]any{"coordinator_model": "m", "repository": "acme/model"}, &fakeRepositories{files: map[string]string{"prj-a": ""}}, 400, "invalid_request"},
		"a model no name can be":        {map[string]any{"coordinator_hosted": true, "coordinator_model": "a b", "repository": "acme/model"}, &fakeRepositories{files: map[string]string{"prj-a": ""}}, 400, "invalid_request"},
	} {
		store, create := hostingAPI(t, tc.repos)
		code, out := create(with(tc.more))
		reason, _ := out["reason"].(string)
		if code != tc.status || reason != tc.reason || len(store.created) != 0 {
			t.Errorf("%s: got = %d %v with %d created, want %d %s and nothing created", name, code, out, len(store.created), tc.status, tc.reason)
		}
	}

	attached := &fakeRepositories{files: map[string]string{"prj-a": ""}}
	store, create := hostingAPI(t, attached)

	// Nothing is hosted while the launcher config names has no row: its token would be refused.
	store.unregistered = true
	code, out := create(with(map[string]any{"coordinator_hosted": true, "repository": "acme/model"}))
	if message, _ := out["message"].(string); code != http.StatusServiceUnavailable || out["reason"] != "launcher_not_registered" || !strings.Contains(message, "hl-admin launcher add main") || len(store.created) != 0 {
		t.Fatalf("a coordinator with no launcher registered: got = %d %v, want 503 launcher_not_registered", code, out)
	}
	if code, out := create(with(nil)); code != http.StatusCreated {
		t.Fatalf("nothing hosted and no launcher registered: got = %d %v, want 201", code, out)
	}

	store, create = hostingAPI(t, attached)
	store.createErr = errors.New("the database is away")
	if code, out := create(with(map[string]any{"coordinator_hosted": true, "repository": "acme/model"})); code != http.StatusInternalServerError || len(store.created) != 0 {
		t.Fatalf("a write that failed: got = %d %v with %d created, want 500 and nothing", code, out, len(store.created))
	}
}

// A coordinator's token is in the hands of a machine the platform rents. It keeps the brief, and
// what the run may spend and how long it lasts stay with whoever created it.
func TestACoordinatorsTokenChangesTheDescriptionAndNothingARunsCostFollowsFrom(t *testing.T) {
	starts, ends := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC)
	live := func() *domain.PlatformExperiment {
		return &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "prj-a", Name: "n", Description: "the brief", Status: domain.PlatformExpOpen,
			BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 30, StartsAt: starts, DurationHours: 24, BaselineDueAt: ends,
			Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize", Role: "ranking"}},
			Stages:  []domain.Stage{{LengthPct: 100}},
		}
	}
	put := func(caller auth.Principal, body map[string]any) (int, map[string]any, *domain.PlatformExperiment) {
		t.Helper()
		store := &createdStore{created: []*domain.PlatformExperiment{live()}}
		svc := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType)
		placement := authtest.Placement{
			Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}},
			PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"}},
		}
		router := authtest.Router(authtest.As(caller))
		doc := apidocs.New(router, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
		RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, testPlanning, zap.NewNop()).WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "m", CoordinatorModel: "c", SigningSecret: "a-secret-only-this-test-knows-00", MaxHosted: hostingMaxHosted}))
		var payload bytes.Buffer
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("PATCH", "/platform-experiments/pe-1", &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, store.created[0]
	}
	coordinator := auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1"}

	code, out, stored := put(coordinator, map[string]any{"description": "the brief, amended"})
	want := live()
	want.Description = "the brief, amended"
	if code != http.StatusOK || !reflect.DeepEqual(stored, want) {
		t.Fatalf("a coordinator amending the description: got = %d %v, stored %+v", code, out, stored)
	}
	for field, more := range map[string]map[string]any{
		"budget_accelerator_hours":    {"budget_accelerator_hours": 500},
		"max_agents":                  {"max_agents": 30},
		"duration_hours":              {"duration_hours": 48},
		"starts_at":                   {"starts_at": "2030-01-02T00:00:00Z"},
		"max_concurrent_accelerators": {"max_concurrent_accelerators": 8},
		"report_interval_seconds":     {"report_interval_seconds": 60},
		"metrics":                     {"metrics": []map[string]any{{"key": "loss", "direction": "minimize", "role": "ranking"}}},
		"name":                        {"name": "another"},
	} {
		// Even at its live value: the field is not a coordinator's to send.
		more["description"] = "the brief, amended"
		code, out, stored := put(coordinator, more)
		message, _ := out["message"].(string)
		if code != http.StatusForbidden || out["reason"] != "coordinator_changes_description_only" || !strings.Contains(message, field) {
			t.Errorf("a coordinator changing %s: got = %d %v, want 403 naming the field", field, code, out)
		}
		if !reflect.DeepEqual(stored, live()) {
			t.Errorf("a coordinator changing %s: stored %+v, want nothing changed, the description included", field, stored)
		}
		// The same body from the people who created it is theirs to send.
		if code, out, _ := put(auth.Principal(ownerOfA), more); code != http.StatusOK {
			t.Errorf("an owner changing %s: got = %d %v, want 200", field, code, out)
		}
	}
}
