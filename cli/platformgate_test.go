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

	"github.com/scaleresearch/hypothesisloop/controlplane/services/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
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
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", "", authtest.As(caller), auth.NewAccess(authtest.Placement{}), logger)
	experiments := quota.NewPlatformExperimentsHandler(nil, struct{ githost.Provider }{}, quota.Planning{Limit: time.Minute, ProposedFor: time.Minute, PlannerKept: time.Minute}, logger).
		WithAgentLaunch(quota.AgentLaunch{Launcher: "launcher", DefaultModel: "model", SigningSecret: "secret", MaxHosted: 1})
	quota.RegisterHuma(doc, quota.NewHandler(nil, logger), experiments)
	quota.RegisterAgentsHuma(doc, experiments, struct{ quota.Hypotheses }{})
	quota.RegisterPlanningHuma(doc, experiments)
	quota.RegisterDataUsageHuma(doc, quota.NewDataUsageHandler(nil, nil, nil, 0, logger))
	quota.RegisterSessionsHuma(doc, nil, logger)
	accounts.RegisterHuma(doc, accounts.NewHandler(nil, nil, nil, nil, "secret", accounts.RuntimeAddresses{}, accounts.Limits{MaxOrgsPerUser: 1, MaxProjectsPerOrg: 1}, logger))
	scheduler.RegisterHuma(doc, scheduler.NewHandler(scheduler.NewService(nil, nil, nil, nil,
		struct{ scheduler.CodeHost }{}, struct{ scheduler.InstalledCapacity }{}, struct{ scheduler.QuotaSettler }{}, "http://metrics.invalid", logger).
		WithLoop(struct{ scheduler.Triggerable }{})))
	registry.RegisterHuma(doc, registry.NewHandler(registry.New(nil, nil, nil, logger, "http://metrics.invalid", nil), logger))
	registry.RegisterWatchHuma(doc)
	access := auth.NewAccess(authtest.Placement{})
	r.Get("/watch", registry.NewWatchHandler(nil, nil, authtest.As(caller), access, logger).ServeHTTP)
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

// admittedCommands is every command the tables list to a caller of kind, as "group command".
func admittedCommands(kind callerKind) []string {
	var names []string
	for _, command := range commands {
		if !command.who.admits(kind) {
			continue
		}
		if command.subs == nil {
			names = append(names, command.name)
		}
		for _, sub := range command.subs {
			switch {
			case !sub.who.admits(kind):
			case command.name == "platform-experiments" && sub.name == "agents":
				for _, agents := range agentsCommands {
					if agents.who.admits(kind) {
						names = append(names, "platform-experiments agents "+agents.name)
					}
				}
			default:
				names = append(names, command.name+" "+sub.name)
			}
		}
	}
	return names
}

// unsent is each listed command no test below sends to the platform's check, with why.
var unsent = map[string]string{
	"git-credential": "git runs it, with a request on standard input",
}

// requireEveryListedCommandSent fails for a command the tables list to kind that ran holds no
// invocation of: a command is listed to a caller only with the proof that the platform admits it.
func requireEveryListedCommandSent(t *testing.T, kind callerKind, ran [][]string) {
	t.Helper()
	for _, name := range admittedCommands(kind) {
		sent := unsent[name] != ""
		for _, command := range ran {
			if line := strings.Join(command, " ") + " "; strings.HasPrefix(line, name+" ") {
				sent = true
			}
		}
		if !sent {
			t.Errorf("hl %s is listed to this caller and never sent to the platform's check here", name)
		}
	}
}

// A researcher was listed platform-experiments create, update, close and agents add, and a
// coordinator hl job submit and hl hypothesis checkout, each of which it is refused. The listing
// and the refusal are one table: what a caller is not listed it is refused before any request,
// and what it is refused no listing it reads names.
func TestACallerIsListedTheCommandsItRunsAndRefusedTheOthersBeforeAnyRequest(t *testing.T) {
	calls := recordingServer(t, func(int, recorded) any { return map[string]any{} })
	for scope, kind := range map[string]callerKind{"agent:pe-1/ag-1": callerAgent, "coordinator:pe-1": callerCoordinator} {
		t.Setenv("HL_TOKEN", derivedToken(scope))
		admitted := admittedCommands(kind)
		var all []string
		for _, name := range admittedCommands(callerOther) {
			all = append(all, name)
		}
		for _, name := range all {
			listed := false
			for _, ok := range admitted {
				listed = listed || ok == name
			}
			words := strings.Fields(name)
			var listing, none bytes.Buffer
			if len(words) > 1 {
				run(append(words[:len(words)-1:len(words)-1], "--help"), &listing, &none)
			} else {
				run([]string{"--help"}, &listing, &none)
			}
			last := words[len(words)-1]
			inListing := strings.Contains(listing.String(), "\n  "+last+" ") || strings.Contains(listing.String(), "\n  "+last+"\n")
			if inListing != listed {
				t.Errorf("%s: hl %s is in the listing its group prints = %v, admitted by the table = %v", scope, name, inListing, listed)
			}
			if listed {
				continue
			}
			*calls = nil
			var stdout, stderr bytes.Buffer
			if code := run(words, &stdout, &stderr); code != 1 || len(*calls) != 0 || !strings.Contains(stderr.String(), "HL_TOKEN is a") || strings.Count(stderr.String(), "\n") != 1 {
				t.Errorf("%s: hl %s = exit %d after %d requests, stderr %q, want it refused in one line before any request", scope, name, code, len(*calls), stderr.String())
			}
		}
		top := func() string {
			var listing, none bytes.Buffer
			run([]string{"--help"}, &listing, &none)
			return listing.String()
		}()
		wantJobs := "submit | list | status | metrics | summary | logs | data | cost | cancel | stats"
		wantRun := "list | get | propose | decline | agents"
		if kind == callerCoordinator {
			wantJobs = "list | status | metrics | summary | logs | data | cost | cancel | stats"
			wantRun = "list | get | update | close | summary | agents"
		}
		for _, line := range []string{"\n  job                   " + wantJobs + "\n", "\n  platform-experiments  " + wantRun + "\n"} {
			if !strings.Contains(top, line) {
				t.Errorf("%s: hl --help has no line %q:\n%s", scope, line, top)
			}
		}
	}
}

// hl platform-experiments summary sent POST where the platform serves PUT, and nothing failed
// until a coordinator's summary was refused in a run. Each command a coordinator is briefed to
// run is sent here as a coordinator, with the method and the path the platform serves.
func TestNoCommandACoordinatorRunsCallsARouteACoordinatorMayNot(t *testing.T) {
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
		{"platform-experiments", "agents", "list"},
		{"platform-experiments", "agents", "session", "coordinator"},
		{"platform-experiments", "agents", "add", "--hosted", "--flavor", "generalist"},
		{"platform-experiments", "agents", "add", "--hosted", "--role", "baseline", "--name", "Baseline"},
		{"platform-experiments", "agents", "remove", "ag-1"},
		{"platform-experiments", "update", "--description-file", brief},
		{"platform-experiments", "update", "--baseline-job", "job-1"},
		{"platform-experiments", "summary", "--file", brief},
		{"platform-experiments", "close"},
		{"job", "list", "--status", "QUEUED"},
		{"job", "list", "--agent", "ag-1"},
		{"job", "status", "job-1"},
		{"job", "logs", "job-1"},
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
		{"watch", "--all", "--decisions", "--wake", "agent.finished,job.held!=summary_gate,experiment.status=FAILED", "--no-wait"},
	}
	requireEveryListedCommandSent(t, callerCoordinator, sent)
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

	sent := [][]string{
		{"platform-experiments", "list"},
		{"platform-experiments", "get"},
		{"platform-experiments", "agents", "session", "ag-1"},
		{"platform-experiments", "agents", "finish", "--reason", "nothing left"},
		{"platform-experiments", "propose", proposal},
		{"platform-experiments", "decline", "--reason", "nothing to measure"},
		{"job", "submit", "job.yaml", "--hypothesis", "h1", "--theory", "t"},
		{"job", "list"},
		{"job", "list", "--needs-summary"},
		{"job", "status", "job-1"},
		{"job", "metrics", "job-1"},
		{"job", "summary", "job-1", "--text", "what it found"},
		{"job", "logs", "job-1"},
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
		{"data-key"},
		{"watch", "--wake", "experiment.status", "--no-wait"},
		{"repo", "init"},
	}
	requireEveryListedCommandSent(t, callerAgent, sent)
	for _, command := range sent {
		requests = 0
		var stdout, stderr bytes.Buffer
		run(command, &stdout, &stderr)
		if requests == 0 || strings.Contains(stderr.String(), "may not call") || strings.Contains(stderr.String(), "the platform serves no") {
			t.Errorf("hl %s as an agent: %d requests let through, stderr = %s", strings.Join(command, " "), requests, stderr.String())
		}
	}
}
