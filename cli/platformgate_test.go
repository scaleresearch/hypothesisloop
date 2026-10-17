package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// principalOf is the caller a test token stands for: the agent or the coordinator a derived
// token names, and an API key for any other.
func principalOf(token string) auth.Principal {
	switch kind, pe, agent := scopeOf(token); kind {
	case callerAgent:
		return auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: pe, AgentID: agent}
	case callerCoordinator:
		return auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: pe}
	}
	return auth.Principal{Kind: auth.KindKey}
}

// platformRoutes is every route the platform serves an agent, a coordinator or a person, each
// behind the platform's own check of who may call it, and no service behind any.
func platformRoutes(caller auth.Principal) http.Handler {
	logger := zap.NewNop()
	r := chi.NewRouter()
	r.Use(auth.Gate(authtest.As(caller), logger, map[string]string{"/watch": registry.WatchSubprotocol}))
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", "", auth.NewAccess(authtest.Placement{}), logger)
	experiments := platformexperiments.NewPlatformExperimentsHandler(nil, struct{ githost.Provider }{}, platformexperiments.Planning{Limit: time.Minute, ProposedFor: time.Minute, PlannerKept: time.Minute}, logger).
		WithAgentLaunch(platformexperiments.AgentLaunch{Hosting: true, DefaultModel: "model", SigningSecret: "secret", MaxHosted: 1})
	httpapi.RegisterAgents(doc, nil)
	platformexperiments.RegisterHuma(doc, experiments)
	httpapi.RegisterSituation(doc, nil, logger)
	platformexperiments.RegisterBudgetHuma(doc, experiments)
	httpapi.RegisterMessages(doc, nil, logger)
	platformexperiments.RegisterAgentsHuma(doc, experiments, struct{ platformexperiments.Hypotheses }{})
	platformexperiments.RegisterPlanningHuma(doc, experiments)
	httpapi.RegisterExperimentData(doc, nil, logger)
	httpapi.RegisterSessions(doc, nil, logger)
	accounts.RegisterHuma(doc, accounts.NewHandler(nil, nil, nil, nil, nil, "secret", accounts.RuntimeAddresses{}, accounts.Limits{MaxOrgsPerUser: 1, MaxProjectsPerOrg: 1}, logger))
	scheduler.RegisterHuma(doc, scheduler.NewHandler(scheduler.NewService(nil, nil, nil,
		struct{ scheduler.CodeHost }{}, struct{ scheduler.InstalledCapacity }{}, struct{ scheduler.QuotaSettler }{}, "http://metrics.invalid", logger).
		WithLoop(struct{ scheduler.Triggerable }{})))
	registry.RegisterHuma(doc, registry.NewHandler(registry.New(nil, nil, nil, logger, "http://metrics.invalid", nil), logger))
	registry.RegisterWatchHuma(doc)
	access := auth.NewAccess(authtest.Placement{})
	r.Get("/watch", registry.NewWatchHandler(nil, nil, access, logger).ServeHTTP)
	return r
}

// asThePlatformAdmits answers with handler what the platform lets the caller of HL_TOKEN ask,
// and refuses the rest as the platform does: a route its kind of token may not call with the
// platform's 403, and a route the platform does not serve with 404. So a command of hl that
// calls a route its caller may not is refused in its test as it is in a run.
func asThePlatformAdmits(t *testing.T, token string, handler http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	routes := platformRoutes(principalOf(token))
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		probe := httptest.NewRequest(r.Method, r.URL.String(), bytes.NewReader(body))
		probe.Header = r.Header.Clone()
		if status, answer := admitted(routes, probe); status != 0 {
			t.Logf("refused by the platform: %s %s -> %d %s", r.Method, r.URL.Path, status, answer)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(w, r)
	}
}

// admitted is zero for a request the platform's check lets through to its handler, and the
// refusal otherwise. A handler here has no service, so being reached it panics or answers
// anything at all, and neither is a refusal of the caller.
func admitted(routes http.Handler, r *http.Request) (status int, answer string) {
	if rctx := chi.NewRouteContext(); !routes.(chi.Routes).Match(rctx, r.Method, r.URL.Path) {
		return http.StatusNotFound, `{"error":"the platform serves no ` + r.Method + " " + r.URL.Path + `"}`
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		routes.ServeHTTP(rec, r)
	}()
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "may not call") {
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	return 0, ""
}

// hl platform-experiments summary sent POST where the platform serves PUT, and nothing failed
// until a coordinator's summary was refused in a run. Each command a coordinator is briefed to
// run is sent here as a coordinator, with the method and the path the platform serves.
func TestNoCommandACoordinatorRunsCallsARouteACoordinatorMayNot(t *testing.T) {
	pushedClone(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	writeJob(t, brief, "the brief\n")
	token := derivedToken("coordinator:pe-1")
	requests := 0
	setAPIURL(t, withServer(t, asThePlatformAdmits(t, token, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, "{}")
	})))
	t.Setenv("HL_TOKEN", token)
	t.Setenv("HL_STATE_DIR", t.TempDir())

	sent := [][]string{
		{"platform-experiments", "list"},
		{"platform-experiments", "get"},
		{"platform-experiments", "health"},
		{"platform-experiments", "agents", "list"},
		{"platform-experiments", "agents", "session", "coordinator"},
		{"platform-experiments", "agents", "add", "--hosted", "--flavor", "generalist"},
		{"platform-experiments", "agents", "add", "--hosted", "--role", "baseline", "--name", "Baseline"},
		{"platform-experiments", "agents", "remove", "ag-1"},
		{"platform-experiments", "agents", "message", "ag-1", "--text", "try warmup"},
		{"platform-experiments", "agents", "message", "--all", "--text", "queue more trials"},
		{"platform-experiments", "agents", "messages"},
		{"platform-experiments", "update", "--description-file", brief},
		{"platform-experiments", "update", "--baseline-job", "job-1"},
		{"platform-experiments", "summary", "--file", brief},
		{"platform-experiments", "close"},
		{"job", "list", "--status", "QUEUED"},
		{"job", "list", "--agent", "ag-1"},
		{"job", "status", "job-1"},
		{"job", "logs", "job-1"},
		{"job", "wait", "job-1", "--timeout", "1"},
		{"job", "metrics", "job-1"},
		{"job", "data", "job-1"},
		{"job", "cost", "job-1"},
		{"job", "summary", "job-1", "--text", "what it found"},
		{"job", "cancel", "job-1"},
		{"job", "stats"},
		{"hypothesis", "query", "MATCH (h:Hypothesis) RETURN h.id"},
		{"hypothesis", "comment", "h1", "--text", "a note"},
		{"hypothesis", "submit", "--text", "an idea", "--builds-on", "root"},
		{"hypothesis", "link", "h1", "--type", "related", "--to", "h2"},
		{"hypothesis", "status", "h1", "inconclusive"},
		{"hypothesis", "list"},
		{"hypothesis", "get", "h1"},
		{"repo", "check"},
		{"repo", "init"},
		{"quota", "--all"},
		{"results"},
		{"capacity"},
		{"situation"},
		{"watch", "--all", "--decisions", "--wake", "agent.finished,job.held,experiment.status=FAILED", "--no-wait"},
	}
	for _, command := range sent {
		requests = 0
		var stdout, stderr bytes.Buffer
		run(command, &stdout, &stderr)
		if requests == 0 || strings.Contains(stderr.String(), "may not call") || strings.Contains(stderr.String(), "the platform serves no") {
			t.Errorf("hl %s as a coordinator: %d requests let through, stderr = %s", strings.Join(command, " "), requests, stderr.String())
		}
	}
}

// hl hypothesis checkout read the roster, which an agent's token may not call, and its tests
// answered the roster to any token: every researcher of two runs was refused its own branch.
// Each command an agent is briefed to run is sent here as an agent, and none may ask the
// platform for what an agent's token is refused or the platform does not serve.
func TestNoCommandAnAgentRunsCallsARouteAnAgentMayNot(t *testing.T) {
	pushedClone(t)
	proposal := filepath.Join(t.TempDir(), "proposal.yaml")
	writeJob(t, proposal, "budget_accelerator_hours: 1\n")
	token := derivedToken("agent:pe-1/ag-1")
	requests := 0
	setAPIURL(t, withServer(t, asThePlatformAdmits(t, token, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, "{}")
	})))
	t.Setenv("HL_TOKEN", token)
	t.Setenv("HL_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	sent := [][]string{
		{"platform-experiments", "list"},
		{"platform-experiments", "get"},
		{"platform-experiments", "health"},
		{"platform-experiments", "agents", "session", "ag-1"},
		{"platform-experiments", "agents", "finish", "--reason", "nothing left"},
		{"platform-experiments", "agents", "messages"},
		{"platform-experiments", "propose", proposal},
		{"platform-experiments", "decline", "--reason", "nothing to measure"},
		{"job", "submit", "job.yaml", "--hypothesis", "h1", "--theory", "t"},
		{"env", "build", "job.yaml"},
		{"env", "status", "job.yaml"},
		{"env", "stage", "job-1"},
		{"job", "list"},
		{"job", "status", "job-1"},
		{"job", "metrics", "job-1"},
		{"job", "summary", "job-1", "--text", "what it found"},
		{"job", "logs", "job-1"},
		{"job", "wait", "job-1", "--timeout", "1"},
		{"job", "data", "job-1"},
		{"job", "cost", "job-1"},
		{"job", "cancel", "job-1"},
		{"job", "stats"},
		{"hypothesis", "submit", "--text", "an idea", "--builds-on", "root"},
		{"hypothesis", "checkout", "h1"},
		{"hypothesis", "link", "h1", "--type", "related", "--to", "h2"},
		{"hypothesis", "comment", "h1", "--text", "a note"},
		{"hypothesis", "status", "h1", "inconclusive"},
		{"hypothesis", "list"},
		{"hypothesis", "get", "h1"},
		{"hypothesis", "query", "MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN h.id"},
		{"repo", "check"},
		{"quota"},
		{"quota", "--all"},
		{"results"},
		{"capacity"},
		{"situation"},
		{"data-key"},
		{"watch", "--wake", "experiment.status", "--no-wait"},
		{"repo", "init"},
	}
	for _, command := range sent {
		requests = 0
		var stdout, stderr bytes.Buffer
		run(command, &stdout, &stderr)
		if requests == 0 || strings.Contains(stderr.String(), "may not call") || strings.Contains(stderr.String(), "the platform serves no") {
			t.Errorf("hl %s as an agent: %d requests let through, stderr = %s", strings.Join(command, " "), requests, stderr.String())
		}
	}
}
