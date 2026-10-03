package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
