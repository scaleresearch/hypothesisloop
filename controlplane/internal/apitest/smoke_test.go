package apitest

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/clusteragentapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

func get(t *testing.T, r chi.Router, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	r.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestSmoke(t *testing.T) {
	// The public API is one document: quota, scheduler and registry operations are registered
	// together exactly as control-service wires them, so /openapi.json and /explore describe
	// every operation a caller can reach at the single base URL.
	r := chi.NewRouter()
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", apidocs.PlatformRules)
	quota.RegisterHuma(doc, quota.NewHandler(nil, nil), quota.NewPlatformExperimentsHandler(nil, nil))
	scheduler.RegisterHuma(doc, scheduler.NewHandler(nil))
	registry.RegisterHuma(doc, registry.NewHandler(nil, nil))
	doc.MountExplore(r)

	// Cluster-agent traffic is a separate audience on its own prefix, so it keeps its own doc.
	rc := chi.NewRouter()
	dc := apidocs.New(rc, "cluster-agent", "1.0.0", "")
	clusteragentapi.RegisterHuma(dc, clusteragentapi.NewHandler(nil, 0, "", nil))
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
			"GET /experiments/{id}/lineage",
			"POST /hypotheses",
			"GET /platform-experiments",
			"reprioritize",
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
