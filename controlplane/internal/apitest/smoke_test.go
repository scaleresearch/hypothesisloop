package apitest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// answering serves r and checks each successful answer of an operation of api against the schema
// its document gives that answer, as a client generated from the document reads it.
func answering(t *testing.T, api huma.API, r http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		route := chi.NewRouteContext()
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route)))
		for k, v := range recorder.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())

		item := api.OpenAPI().Paths[route.RoutePattern()]
		if item == nil || recorder.Code >= 300 || recorder.Body.Len() == 0 {
			return
		}
		op := map[string]*huma.Operation{"GET": item.Get, "POST": item.Post, "PUT": item.Put, "PATCH": item.Patch, "DELETE": item.Delete}[req.Method]
		response := op.Responses[strconv.Itoa(recorder.Code)]
		if response == nil {
			t.Errorf("%s %s answered %d, which its document does not describe", req.Method, req.URL, recorder.Code)
			return
		}
		var body any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s answered no JSON: %v", req.Method, req.URL, err)
			return
		}
		result := &huma.ValidateResult{}
		content := response.Content["application/json"]
		if content == nil {
			t.Errorf("%s %s answered %d with a body its document does not describe", req.Method, req.URL, recorder.Code)
			return
		}
		huma.Validate(api.OpenAPI().Components.Schemas, content.Schema, huma.NewPathBuffer(nil, 0), huma.ModeReadFromServer, body, result)
		for _, err := range result.Errors {
			t.Errorf("%s %s answered what its document does not allow: %v", req.Method, req.URL, err)
		}
	})
}

func get(t *testing.T, r http.Handler, path string) (int, string) {
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
	doc := httpapi.NewDoc(r, "hypothesisloop API", "1.0.0", agenttypes.PlatformRules, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	peHandler := platformexperiments.NewService(nil, domain.QuotaConfig{}, zap.NewNop(), nil, time.Minute).WithRepositories(worldRepositories{}).WithPlanning(worldPlanning)
	httpapi.Register(doc, httpapi.Handlers{
		Logger:              zap.NewNop(),
		PlatformExperiments: peHandler,
		Hypotheses:          struct{ platformexperiments.Hypotheses }{},
		Launch:              platformexperiments.AgentLaunch{AgentSandbox: true, DefaultModel: "model-1", CoordinatorModel: "model-2", SigningSecret: "contract-test-signing-secret", MaxAgentSandboxes: 20},
		Scheduler:           scheduler.NewService(nil, nil, nil, worldRepositories{}, contractCapacity{}, contractSettler{}, metricsdb.New("http://metrics.invalid"), zap.NewNop()).WithEnvironments(noEnvironments{}).WithLoop(noopLoop{}),
	})
	doc.MountExplore(r)

	// Cluster-agent traffic is a separate audience on its own prefix, so it keeps its own doc.
	rc := authtest.Router(authtest.Staff)
	dc := httpapi.NewDoc(rc, "cluster-agent", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	httpapi.RegisterClusterAgent(dc, clusterexchange.NewService(nil, 0, nil, 0, nil, 0, nil, 0, "", "https://api.test/git", nil, nil))
	dc.MountExplore(rc)

	for _, tc := range []struct {
		name string
		r    http.Handler
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
