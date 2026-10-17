package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/modelproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/watch"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
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
	noHypotheses   struct{ platformexperiments.Hypotheses }
)

func testRouter(t *testing.T) chi.Router {
	t.Helper()
	unreachable := authtest.As{Kind: auth.KindStaff}
	launch := platformexperiments.AgentLaunch{AgentSandbox: true, DefaultModel: "model", CoordinatorModel: "model", SigningSecret: "secret", MaxAgentSandboxes: 1}
	platformExperiments := platformexperiments.NewService(nil, domain.QuotaConfig{}, zap.NewNop(), nil, time.Minute).WithRepositories(noRepositories{}).
		WithPlanning(platformexperiments.Planning{Limit: time.Minute, ProposedFor: time.Minute, PlannerKept: time.Minute}).WithAgentLaunch(launch)
	modelProxy, err := modelproxy.New(noneInAgentSandbox{}, metricsdb.New(""), "https://model.example.invalid", "credential", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return httpapi.NewRouter(httpapi.Handlers{
		Tokens:              unreachable,
		Access:              auth.NewAccess(authtest.Placement{}),
		UIOrigin:            "http://ui.test",
		Logger:              zap.NewNop(),
		PlatformExperiments: platformExperiments,
		Hypotheses:          noHypotheses{},
		Launch:              launch,
		Watch:               &watch.Handler{},
		ModelProxy:          modelProxy,
	})
}

type noneInAgentSandbox struct{}

func (noneInAgentSandbox) InAgentSandbox(context.Context, string, string) (bool, error) {
	return false, nil
}

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
		"the subprotocol alone":     {"Sec-WebSocket-Protocol": platformapi.WatchSubprotocol},
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
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Sec-WebSocket-Protocol: "+platformapi.WatchSubprotocol) {
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

// harnessVocabularyFile is the Python module the agent harness reads the platform's vocabulary
// from, rendered from the control plane's own definitions.
var harnessVocabularyFile = filepath.Join("..", "..", "..", "agents", "harness", "src", "hypothesisloop_agent", "platform.py")

// The values the agent harness branches on are the control plane's own: a status, a kind or a
// field renamed here renames it in the harness, and a harness that kept the old one fails here.
func TestTheHarnessVocabularyIsUnchanged(t *testing.T) {
	got := harnessVocabulary()
	if *update {
		if err := os.WriteFile(harnessVocabularyFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(harnessVocabularyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the vocabulary the control plane defines: run this test with -update", harnessVocabularyFile)
	}
}

// vocabulary is a Python module of plain constants and frozensets, in the order written.
type vocabulary struct{ strings.Builder }

var nonIdentifier = regexp.MustCompile(`[^A-Z0-9]+`)

func (v *vocabulary) section(comment string) {
	fmt.Fprintf(v, "\n# %s\n", comment)
}

func (v *vocabulary) number(name string, value int) { fmt.Fprintf(v, "%s = %d\n", name, value) }

func (v *vocabulary) text(name, value string) {
	fmt.Fprintf(v, "%s = %s\n", name, strconv.Quote(value))
}

// set writes values as the frozenset name, sorted.
func (v *vocabulary) set(name string, values []string) {
	sorted := slices.Sorted(slices.Values(values))
	quoted := make([]string, len(sorted))
	for i, value := range sorted {
		quoted[i] = strconv.Quote(value)
	}
	fmt.Fprintf(v, "%s = frozenset({%s})\n", name, strings.Join(quoted, ", "))
}

// each writes every value as a constant of its own, named prefix and the value, sorted.
func (v *vocabulary) each(prefix string, values []string) {
	for _, value := range slices.Sorted(slices.Values(values)) {
		v.text(prefix+strings.Trim(nonIdentifier.ReplaceAllString(strings.ToUpper(value), "_"), "_"), value)
	}
}

func strs[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}

// jsonFields is the json name of every field of T.
func jsonFields[T any]() []string {
	var out []string
	typ := reflect.TypeFor[T]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}

func harnessVocabulary() []byte {
	var v vocabulary
	v.WriteString(`"""The platform's vocabulary the agent harness reads, rendered from the control plane's own
definitions by TestTheHarnessVocabularyIsUnchanged (controlplane/cmd/control-service, go test -run
TestTheHarnessVocabularyIsUnchanged -update). Change those definitions, never this file."""
`)
	v.section("The statuses of a job (platformapi.ExperimentStatus), and those it ends in.")
	v.set("EXPERIMENT_STATUSES", strs(platformapi.ExperimentStatuses))
	v.set("ENDED_STATUSES", strs(platformapi.EndedStatuses))
	v.each("STATUS_", strs(platformapi.ExperimentStatuses))

	v.section("The statuses of a platform experiment (domain.PlatformExperimentStatus).")
	v.set("PLATFORM_EXPERIMENT_STATUSES", strs(domain.PlatformExperimentStatuses))
	v.each("PLATFORM_EXPERIMENT_", strs(domain.PlatformExperimentStatuses))

	v.section("The roles the platform serves instructions for (domain.AgentRole).")
	v.set("AGENT_ROLES", strs(domain.AgentRoles))
	v.each("ROLE_", strs(domain.AgentRoles))

	v.section("The kinds of change GET /watch carries (domain.EventKinds), and the line hl watch prints in place of changes no longer held.")
	kinds := make([]string, len(domain.EventKinds))
	for i, k := range domain.EventKinds {
		kinds[i] = k.Kind
	}
	v.set("EVENT_KINDS", kinds)
	v.each("EVENT_", kinds)
	v.text("WATCH_RESET", platformapi.ResetKind)

	v.section("A token the platform derives, and the kinds of scope it names that run a harness (scope).")
	v.text("DERIVED_TOKEN_PREFIX", scope.Prefix)
	v.text("SCOPE_AGENT", string(scope.KindAgent))
	v.text("SCOPE_COORDINATOR", string(scope.KindCoordinator))

	v.section("The lines the launcher reads from a sandboxed harness's output, and the environment it starts the harness with (agentsandbox).")
	v.text("PROGRESS_PREFIX", agentsandbox.ProgressPrefix)
	v.text("ACTIVITY_PREFIX", agentsandbox.ActivityPrefix)
	v.text("STATE_PREFIX", agentsandbox.StatePrefix)
	v.number("MAX_ACTIVITY_LENGTH", agentsandbox.MaxActivityLength)
	v.number("MAX_STATE_LENGTH", agentsandbox.MaxStateLength)
	v.number("PROGRESS_INTERVAL_S", agentsandbox.ProgressIntervalSeconds)
	v.text("ENV_API_URL", agentsandbox.EnvAPIURL)
	v.text("ENV_TOKEN", agentsandbox.EnvToken)

	v.section("How long a token works and a machine stays after its run ended (domain.AgentWindDown, domain.CoordinatorWindDown).")
	v.number("AGENT_WIND_DOWN_S", int(domain.AgentWindDown/time.Second))
	v.number("COORDINATOR_WIND_DOWN_S", int(domain.CoordinatorWindDown/time.Second))

	v.section("Why a platform experiment closed (domain.AnsweredCloseReasons), why it waits (domain.StatusReasons), why an agent's run ended (domain.RunEnd) and why it was cut.")
	v.set("CLOSE_REASONS", strs(domain.AnsweredCloseReasons))
	v.each("CLOSE_", strs(domain.AnsweredCloseReasons))
	v.set("STATUS_REASONS", domain.StatusReasons())
	v.each("STATUS_REASON_", domain.StatusReasons())
	v.each("RUN_END_", strs([]domain.RunEnd{domain.RunEndClosed, domain.RunEndCut, domain.RunEndFinished, domain.RunEndPlanned}))
	v.text("CUT_MAX_AGENTS_LOWERED", db.CutMaxAgentsLowered)

	v.section("The fields of an agent's instructions (platformexperiments.Instructions) and of their settings (domain.AgentTypeSettings).")
	v.set("INSTRUCTIONS_FIELDS", jsonFields[platformexperiments.Instructions]())
	v.set("AGENT_TYPE_SETTINGS_FIELDS", jsonFields[domain.AgentTypeSettings]())

	v.section("The keys of the state line and the values of its turn (agentsandbox).")
	v.text("STATE_SUBAGENTS", agentsandbox.StateSubagents)
	v.text("STATE_TURN", agentsandbox.StateTurn)
	v.text("STATE_SINCE", agentsandbox.StateSince)
	v.text("STATE_INSTRUCTIONS_DIGEST", agentsandbox.StateInstructionsDigest)
	v.text("STATE_VERSION", agentsandbox.StateVersion)
	v.text("STATE_MODEL_STATUS", agentsandbox.StateModelStatus)
	v.text("STATE_MODEL_TYPE", agentsandbox.StateModelType)
	v.text("STATE_MODEL_UTILIZATION", agentsandbox.StateModelUtilization)
	v.text("STATE_MODEL_RESETS_AT", agentsandbox.StateModelResetsAt)
	v.text("TURN_WORKING", agentsandbox.TurnWorking)
	v.text("TURN_WAITING", agentsandbox.TurnWaiting)

	v.section("The placeholders the harness fills, the session it stores and the characters of the instructions digest its state line carries (domain).")
	v.text("PLACEHOLDER_HL_COMMANDS", domain.PlaceholderHLCommands)
	v.text("PLACEHOLDER_WORKDIR", domain.PlaceholderWorkdir)
	v.number("MAX_SESSION_BYTES", domain.MaxAgentSessionBytes)
	v.number("INSTRUCTIONS_DIGEST_LENGTH", domain.InstructionsDigestLength)

	v.section("The kinds of actor (domain.ActorKinds), who a message of a person or a key is from (domain.MessageFrom) and the value of the first notice of an idle stretch (domain.IdleNew).")
	v.each("ACTOR_", strs(domain.ActorKinds))
	v.text("MESSAGE_FROM_OWNER", domain.MessageFrom(domain.Actor{Kind: domain.ActorUser}))
	v.text("IDLE_NEW", domain.IdleNew)
	return []byte(v.String())
}
