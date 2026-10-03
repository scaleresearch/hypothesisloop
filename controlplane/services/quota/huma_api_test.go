package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type noExperimentStore struct{ PlatformExperimentsStore }

func (noExperimentStore) GetPlatformExperiment(context.Context, string) (*domain.PlatformExperiment, error) {
	return nil, nil
}

func (noExperimentStore) ListAgentQuotas(context.Context, string) ([]*domain.AgentQuota, error) {
	return nil, nil
}

func TestQuotasOfAnUnknownPlatformExperimentAreNotFound(t *testing.T) {
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	svc := newSignupTestService(t, noExperimentStore{})
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, testPlanning, zap.NewNop()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/platform-experiments/unknown/quotas", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// takenAgents is an agent store in which every id in it is taken.
type takenAgents struct {
	Store
	taken map[string]bool
}

func (s takenAgents) CreateAgent(_ context.Context, agent *domain.Agent) error {
	if s.taken[agent.ID] {
		return fmt.Errorf("%w: %s", db.ErrAgentExists, agent.ID)
	}
	return nil
}

// A taken id was told from the database's error text, and an agent registered without a name was
// silently named after its id.
func TestRegisteringAnAgentRefusesATakenIDAndRequiresAName(t *testing.T) {
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(waitingPerson), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	agents := NewHandler(NewService(takenAgents{taken: map[string]bool{"veteran": true}}, zap.NewNop()), zap.NewNop())
	RegisterHuma(doc, agents, NewPlatformExperimentsHandler(newSignupTestService(t, noExperimentStore{}), &fakeRepositories{}, testPlanning, zap.NewNop()))

	if code, out := call(t, r, "POST", "/agents", `{"id":"veteran","name":"Veteran"}`); code != http.StatusConflict || out["reason"] != "agent_exists" {
		t.Errorf("a taken id: got = %d %v, want 409 agent_exists", code, out)
	}
	if code, out := call(t, r, "POST", "/agents", `{"id":"rookie","name":"Rookie"}`); code != http.StatusCreated || out["name"] != "Rookie" {
		t.Errorf("a free id: got = %d %v, want 201 with its name", code, out)
	}
	if code, out := call(t, r, "POST", "/agents", `{"id":"nameless"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("no name: got = %d %v, want 422", code, out)
	}
}

// summarizedStore is statusStore with the write of a summary, which fails while broken is set.
type summarizedStore struct {
	statusStore
	broken error
}

func (s *summarizedStore) SetPlatformExperimentSummary(_ context.Context, id, summary string) error {
	if s.broken != nil {
		return s.broken
	}
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Summary = summary
			return nil
		}
	}
	return db.ErrPlatformExperimentNotFound
}

func (s *summarizedStore) NameActors(context.Context, []*domain.Actor) error { return nil }

// Closing and the summary answered every failure 400 with the error's own text, a database
// error among them, and a second close read as bad input. Each answers the platform experiment
// it changed, a refusal by its status and reason, and a failure of the platform says nothing.
func TestCloseAndSummaryAnswerThePlatformExperimentAndRefuseByReason(t *testing.T) {
	store := &summarizedStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		{ID: "pe-1", ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", Stages: []domain.Stage{{LengthPct: 100}}},
	}}}}
	svc := newSignupTestService(t, store)
	placement := authtest.Placement{PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"}}}
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(waitingPerson), auth.NewAccess(placement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, testPlanning, zap.NewNop()))

	if code, out := call(t, r, "PUT", "/platform-experiments/pe-1/summary", `{"summary":"what was found"}`); code != http.StatusOK || out["id"] != "pe-1" || out["summary"] != "what was found" {
		t.Errorf("a summary: got = %d %v, want 200 and the platform experiment with it", code, out)
	}
	if code, _ := call(t, r, "POST", "/platform-experiments/pe-1/summary", `{"summary":"again"}`); code != http.StatusMethodNotAllowed {
		t.Errorf("a summary sent with POST: got = %d, want 405", code)
	}
	store.broken = errors.New("pq: connection refused at 10.0.0.7")
	if code, out := call(t, r, "PUT", "/platform-experiments/pe-1/summary", `{"summary":"lost"}`); code != http.StatusInternalServerError || strings.Contains(fmt.Sprint(out), "10.0.0.7") {
		t.Errorf("a summary the store fails: got = %d %v, want 500 with no internal text", code, out)
	}
	store.broken = nil

	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/close", ""); code != http.StatusOK || out["id"] != "pe-1" || out["status"] != "closed" {
		t.Errorf("a close: got = %d %v, want 200 and the platform experiment as closed", code, out)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/close", ""); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Errorf("a second close: got = %d %v, want 409 invalid_transition", code, out)
	}
	for name, err := range map[string]error{
		"closing one that does not exist":    func() error { _, err := svc.Close(context.Background(), "pe-none", nil, nil); return err }(),
		"a summary of one that does not exist": func() error { _, err := svc.SetSummary(context.Background(), "pe-none", "s"); return err }(),
	} {
		var refused *api.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != http.StatusNotFound || refused.Reason != "not_found" {
			t.Errorf("%s: got = %v, want 404 not_found", name, err)
		}
	}
}

type constraintResultsStore struct{ PlatformExperimentsStore }

func (constraintResultsStore) GetPlatformExperiment(context.Context, string) (*domain.PlatformExperiment, error) {
	bound := 0.0
	return &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, Metrics: []domain.MetricDefinition{
		{Key: "digest_mismatches", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}, nil
}

func (constraintResultsStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// The metrics store refusing a query is the platform's failure. Answered as 404, it told agents
// the platform experiment had no results and sent its internal error text along.
func TestResultsTheMetricsStoreRefusesAreAnInternalErrorAndNotNotFound(t *testing.T) {
	metricsDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"invalid promql query","errorType":"InvalidArguments"}`))
	}))
	defer metricsDB.Close()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	svc := newSignupTestService(t, constraintResultsStore{})
	svc.metricsDBURL = metricsDB.URL
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, testPlanning, zap.NewNop()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/platform-experiments/pe-1/results", nil)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "promql") || strings.Contains(rec.Body.String(), "metricsdb") {
		t.Fatalf("the answer carries internal text: %s", rec.Body)
	}
}
