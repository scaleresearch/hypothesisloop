package main

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/modelproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/clusteragentapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
)

// public is every route control-service answers without a token.
var public = map[string]bool{
	"GET /health": true, "GET /healthz": true,
	"GET /explore": true, "GET /explore/coordinator": true, "GET /explore/rules": true,
	"GET /openapi.json": true, "GET /openapi.yaml": true, "GET /openapi-3.0.json": true, "GET /openapi-3.0.yaml": true,
	"GET /docs": true, "GET /schemas/{schema}": true,
	"GET /internal/clusters/explore": true, "GET /internal/clusters/openapi.json": true, "GET /internal/clusters/openapi.yaml": true,
	"GET /internal/clusters/openapi-3.0.json": true, "GET /internal/clusters/openapi-3.0.yaml": true,
	"GET /internal/clusters/docs": true, "GET /internal/clusters/schemas/{schema}": true,
}

// The routes are registered and never served, so what stands behind them is empty.
type (
	noRepositories struct{ githost.Provider }
	noHypotheses   struct{ quota.Hypotheses }
)

func testRouter(t *testing.T) chi.Router {
	t.Helper()
	unreachable := authtest.As{Kind: auth.KindStaff}
	platformExperiments := quota.NewPlatformExperimentsHandler(nil, noRepositories{}, quota.Planning{Limit: time.Minute, ProposedFor: time.Minute, PlannerKept: time.Minute}, zap.NewNop()).
		WithAgentLaunch(quota.AgentLaunch{Hosting: true, DefaultModel: "model", CoordinatorModel: "model", SigningSecret: "secret", MaxHosted: 1})
	modelProxy, err := modelproxy.New(noOneHosted{}, modelproxy.MetricsDB{}, "https://model.example.invalid", "credential", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return newRouter(apiGate{tokens: unreachable, access: auth.NewAccess(authtest.Placement{})}, "http://ui.test", routes{
		quota:               &quota.Handler{},
		platformExperiments: platformExperiments,
		hypotheses:          noHypotheses{},
		dataUsage:           &quota.DataUsageHandler{},
		accounts:            &accounts.Handler{},
		agentTypes:          &accounts.AgentTypesHandler{},
		repositories:        &accounts.Repositories{},
		scheduler:           &scheduler.Handler{},
		registry:            &registry.Handler{},
		watch:               &registry.WatchHandler{},
		clusterAgent:        &clusteragentapi.Handler{},
		modelProxy:          modelProxy,
		modelUsage:          modelproxy.MetricsDB{},
	}, zap.NewNop())
}

type noOneHosted struct{}

func (noOneHosted) Hosted(context.Context, string, string) (bool, error) { return false, nil }

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// Every route of the API refuses a request with no token before its handler runs, but for the
// few the gate lets through on their own.
func TestEveryRouteRefusesARequestWithoutAToken(t *testing.T) {
	r := testRouter(t)
	seen := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodOptions {
			return nil
		}
		route = strings.TrimSuffix(strings.ReplaceAll(route, "/*/", "/"), "/")
		key := method + " " + route
		seen[key] = true
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(method, pathParam.ReplaceAllString(route, "x"), nil))
		if public[key] {
			if recorder.Code == http.StatusUnauthorized {
				t.Errorf("%s: got = 401, want it answered without a token", key)
			}
			return nil
		}
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"error":"unauthenticated: `) {
			t.Errorf("%s: got = %d %s, want = 401 unauthenticated", key, recorder.Code, recorder.Body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range public {
		if !seen[key] {
			t.Errorf("%s is allowed without a token and is no route", key)
		}
	}
	if len(seen) < 100 {
		t.Fatalf("walked %d routes, want every route of the API", len(seen))
	}
}

// /watch takes its token from the subprotocol, and a request offering none is refused before
// the upgrade: the handler behind it has nothing to serve with.
func TestTheWatchUpgradeIsRefusedWithoutAToken(t *testing.T) {
	r := testRouter(t)
	for name, headers := range map[string]map[string]string{
		"no subprotocol":            {},
		"the token as a header":     {"Authorization": "Bearer t"},
		"the subprotocol alone":     {"Sec-WebSocket-Protocol": registry.WatchSubprotocol},
		"another subprotocol first": {"Sec-WebSocket-Protocol": "other, t"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/watch?platform_experiment_id=pe-1", nil)
		for k, v := range map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="} {
			request.Header.Set(k, v)
		}
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Sec-WebSocket-Protocol: "+registry.WatchSubprotocol) {
			t.Errorf("%s: got = %d %s, want = 401 naming the subprotocol", name, recorder.Code, recorder.Body)
		}
	}
}

// A CORS preflight carries no token and is answered before the gate.
func TestAPreflightIsAnsweredWithoutAToken(t *testing.T) {
	recorder := httptest.NewRecorder()
	testRouter(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodOptions, "/platform-experiments", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("got = %d, want = 204", recorder.Code)
	}
}

var update = flag.Bool("update", false, "rewrite testdata with the documents the API publishes now")

// The documents the API publishes are its contract with every client: a change to any of them is
// a change of the API, never a side effect of moving code.
func TestThePublishedDocumentsAreUnchanged(t *testing.T) {
	r := testRouter(t)
	for path, file := range map[string]string{
		"/openapi.json":                   "openapi.json",
		"/explore":                        "explore.md",
		"/explore/coordinator":            "explore-coordinator.md",
		"/explore/rules":                  "explore-rules.md",
		"/internal/clusters/openapi.json": "clusters-openapi.json",
		"/internal/clusters/explore":      "clusters-explore.md",
	} {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: got = %d, want = 200", path, recorder.Code)
		}
		golden := filepath.Join("testdata", file)
		if *update {
			if err := os.WriteFile(golden, recorder.Body.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(recorder.Body.Bytes(), want) {
			t.Errorf("%s differs from %s", path, golden)
		}
	}
}
