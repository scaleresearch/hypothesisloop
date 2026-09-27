package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
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
