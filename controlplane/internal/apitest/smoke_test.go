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
	// quota
	rq := chi.NewRouter()
	dq := apidocs.New(rq, "quota", "1.0.0", apidocs.PlatformRules)
	quota.RegisterHuma(dq, quota.NewHandler(nil, nil), quota.NewPlatformExperimentsHandler(nil, nil))
	dq.MountExplore(rq)

	// scheduler
	rs := chi.NewRouter()
	ds := apidocs.New(rs, "scheduler", "1.0.0", "")
	scheduler.RegisterHuma(ds, scheduler.NewHandler(nil))
	ds.MountExplore(rs)

	// registry
	rr := chi.NewRouter()
	dr := apidocs.New(rr, "registry", "1.0.0", "")
	registry.RegisterHuma(dr, registry.NewHandler(nil, nil))
	dr.MountExplore(rr)

	// cluster-agent
	rc := chi.NewRouter()
	dc := apidocs.New(rc, "cluster-agent", "1.0.0", "")
	clusteragentapi.RegisterHuma(dc, clusteragentapi.NewHandler(nil, 0, "", nil))
	dc.MountExplore(rc)

	for _, tc := range []struct {
		name string
		r    chi.Router
		want string
	}{
		{"quota", rq, "POST /agents"},
		{"scheduler", rs, "POST /experiments"},
		{"registry", rr, "POST /registry/hypotheses"},
		{"cluster", rc, "POST /{name}/reconcile"},
	} {
		if code, body := get(t, tc.r, "/openapi.json"); code != 200 || len(body) < 100 {
			t.Errorf("%s /openapi.json code=%d len=%d", tc.name, code, len(body))
		}
		code, body := get(t, tc.r, "/explore")
		if code != 200 {
			t.Errorf("%s /explore code=%d", tc.name, code)
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s /explore missing %q; got:\n%s", tc.name, tc.want, body)
		}
		t.Logf("%s /explore (%d bytes):\n%s", tc.name, len(body), body)
	}
	// /explore is one fixed output listing everything registered.
	if _, full := get(t, rs, "/explore"); !strings.Contains(full, "reprioritize") {
		t.Errorf("scheduler explore should list every operation including reprioritize")
	}
}
