package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
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
