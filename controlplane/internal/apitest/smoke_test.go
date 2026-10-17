package apitest

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

func get(t *testing.T, r chi.Router, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := authtest.Authorize(httptest.NewRequest("GET", path, nil))
	r.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestSmoke(t *testing.T) {
	// The public API is one document: quota, scheduler and registry operations are registered
	// together exactly as control-service wires them, so /openapi.json and /explore describe
	// every operation a caller can reach at the single base URL.
	r := authtest.Router(authtest.Staff)
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", agenttypes.PlatformRules, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	peHandler := platformexperiments.NewPlatformExperimentsHandler(nil, worldRepositories{}, worldPlanning, nil)
	platformexperiments.RegisterHuma(doc, platformexperiments.NewHandler(nil, nil), peHandler)
	httpapi.RegisterSituation(doc, nil, nil)
	platformexperiments.RegisterBudgetHuma(doc, peHandler)
	scheduler.RegisterHuma(doc, scheduler.NewHandler(
		scheduler.NewService(nil, nil, nil, worldRepositories{}, contractCapacity{}, contractSettler{}, "http://metrics.invalid", zap.NewNop()).WithEnvironments(noEnvironments{}).WithLoop(noopLoop{})))
	registry.RegisterHuma(doc, registry.NewHandler(nil, nil))
	httpapi.RegisterExperimentData(doc, nil, zap.NewNop())
	httpapi.RegisterSessions(doc, nil, zap.NewNop())
	httpapi.RegisterExperimentDrafts(doc, nil)
	doc.MountExplore(r)

	// Cluster-agent traffic is a separate audience on its own prefix, so it keeps its own doc.
	rc := authtest.Router(authtest.Staff)
	dc := apidocs.New(rc, "cluster-agent", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	clusterexchange.RegisterHuma(dc, clusterexchange.NewHandler(nil, 0, "", 0, nil, 0, nil, 0, "", "https://api.test/git", nil, nil))
	dc.MountExplore(rc)

	for _, tc := range []struct {
		name string
		r    chi.Router
		want []string
	}{
		{"api", r, []string{
			"POST /agents",
			"POST /experiments",
			"GET /experiments/{id}/metrics",
			"GET /experiments/{id}/data",
			"GET /platform-experiments/{id}/data-usage",
			"POST /platform-experiments/{id}/data-key",
			"POST /hypotheses",
			"POST /hypotheses/{id}/links",
			"POST /hypotheses/query",
			"GET /hypotheses/graph",
			"GET /platform-experiments",
		}},
		{"cluster", rc, []string{"POST /{name}/reconcile"}},
	} {
		if code, body := get(t, tc.r, "/openapi.json"); code != 200 || len(body) < 100 {
			t.Errorf("%s /openapi.json code=%d len=%d", tc.name, code, len(body))
		}
		code, body := get(t, tc.r, "/explore")
		if code != 200 {
			t.Errorf("%s /explore code=%d", tc.name, code)
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s /explore missing %q", tc.name, want)
			}
		}
	}

	// Nothing may still advertise the retired per-service prefix.
	if _, body := get(t, r, "/explore"); strings.Contains(body, "/registry/") {
		t.Errorf("/explore still advertises a /registry/ path:\n%s", body)
	}
}
