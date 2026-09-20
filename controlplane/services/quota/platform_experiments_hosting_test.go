package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type hostingStore struct {
	createdStore
	spawned  []domain.AgentSignup
	spawnErr error
}

func (s *hostingStore) CreatePlatformExperimentWithAgents(ctx context.Context, pe *domain.PlatformExperiment, agents []*domain.Agent, signups []domain.AgentSignup) error {
	if s.spawnErr != nil {
		return s.spawnErr
	}
	if len(agents) != len(signups) {
		return errors.New("agents and signups differ in number")
	}
	s.spawned = append(s.spawned, signups...)
	return s.CreatePlatformExperiment(ctx, pe)
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
	handler := NewPlatformExperimentsHandler(svc, repos, zap.NewNop()).WithAgentLaunch(AgentLaunch{Launcher: "main", DefaultModel: "model-default", SigningSecret: "a-secret-only-this-test-knows-00"})
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

	// Without hosting, a project with no repository creates as before.
	store, create := hostingAPI(t, &fakeProjectRepos{})
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
