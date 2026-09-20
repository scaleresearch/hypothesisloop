package launcherapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	testSecret = "a-signing-secret-only-this-test-knows"
	testImage  = "registry.example.invalid/agent:1"
	testAPIURL = "https://api.example.invalid"
)

// launcherStore answers with what the test set, and records what it was asked.
type launcherStore struct {
	byLauncher map[string][]db.LauncherAgent
	err        error
	asked      []string
	windDown   time.Duration
}

func (s *launcherStore) ListLauncherAgents(_ context.Context, launcher string, windDown time.Duration) ([]db.LauncherAgent, error) {
	s.asked = append(s.asked, launcher)
	s.windDown = windDown
	return s.byLauncher[launcher], s.err
}

func hosted(pe, agent string) domain.AgentSignup {
	return domain.AgentSignup{
		PlatformExperimentID: pe, AgentID: agent, Hosted: true, Launcher: "main",
		Flavor: "data-search", Model: "claude-sonnet-5", Hyperparameters: map[string]any{"batch_size": float64(64), "a": "b"},
	}
}

func reconcile(t *testing.T, store Store, caller auth.Principal, launcher string, logger *zap.Logger) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterHuma(doc, NewHandler(store, testImage, testAPIURL, testSecret, logger))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodPost, "/"+launcher+"/reconcile", nil)))
	return rec.Code, rec.Body.Bytes()
}

func decode(t *testing.T, raw []byte) Desired {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("answer %q: %v", raw, err)
	}
	for _, key := range []string{"instances", "ending"} {
		if list, ok := keys[key]; !ok || !strings.HasPrefix(string(list), "[") {
			t.Fatalf("answer has no %s list: %s", key, raw)
		}
	}
	var desired Desired
	if err := json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	return desired
}

var mainLauncher = auth.Principal{Kind: auth.KindLauncher, LauncherName: "main"}

func TestTheAnswerIsTheLaunchersRunningInstancesAndItsEndingOnes(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{
		"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: hosted("pe-1", "ag-cut"), Ending: true},
			{AgentSignup: hosted("pe-2", "ag-closed"), Ending: true},
			{AgentSignup: hosted("pe-3", "ag-open")},
		},
		"other": {{AgentSignup: hosted("pe-9", "ag-elsewhere")}},
	}}
	code, raw := reconcile(t, store, mainLauncher, "main", zap.NewNop())
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if !reflect.DeepEqual(store.asked, []string{"main"}) || store.windDown != domain.AgentWindDown {
		t.Fatalf("store asked for %v with wind-down %s", store.asked, store.windDown)
	}
	desired := decode(t, raw)
	if len(desired.Instances) != 2 || len(desired.Ending) != 2 {
		t.Fatalf("instances = %d, ending = %d, want 2 and 2: %s", len(desired.Instances), len(desired.Ending), raw)
	}

	running := desired.Instances[0]
	token, err := auth.Mint(testSecret, auth.AgentScope("pe-1", "ag-running"))
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		"API_URL":                testAPIURL,
		"AGENT_ID":               "ag-running",
		"PLATFORM_EXPERIMENT_ID": "pe-1",
		"AGENT_FLAVOR":           "data-search",
		"AGENT_HYPERPARAMETERS":  `{"a":"b","batch_size":64}`,
		"ANTHROPIC_MODEL":        "claude-sonnet-5",
		"HL_TOKEN":               token,
	}
	if running.Image != testImage || !reflect.DeepEqual(running.Env, wantEnv) {
		t.Errorf("instance = %+v\nwant env %v", running, wantEnv)
	}
	if !strings.HasPrefix(running.Name, "pe1-agrunning-") || !strings.HasPrefix(desired.Instances[1].Name, "pe3-agopen-") {
		t.Errorf("names = %q, %q", running.Name, desired.Instances[1].Name)
	}
	if !strings.HasPrefix(desired.Ending[0], "pe1-agcut-") || !strings.HasPrefix(desired.Ending[1], "pe2-agclosed-") {
		t.Errorf("ending = %v", desired.Ending)
	}
	if strings.Contains(string(raw), "ag-elsewhere") {
		t.Error("another launcher's agent is in the answer")
	}
}

func TestALauncherWithNothingToRunIsAnsweredWithTwoEmptyLists(t *testing.T) {
	code, raw := reconcile(t, &launcherStore{}, mainLauncher, "main", zap.NewNop())
	if code != http.StatusOK {
		t.Fatalf("got = %d %s", code, raw)
	}
	// decode refuses an answer in which either list is missing or null.
	if desired := decode(t, raw); len(desired.Instances) != 0 || len(desired.Ending) != 0 {
		t.Fatalf("got = %s, want two empty lists", raw)
	}
}

func TestAnUnsetFlavorOrModelLeavesTheHarnessDefaultInPlace(t *testing.T) {
	agent := hosted("pe-1", "ag-1")
	agent.Flavor, agent.Model, agent.Hyperparameters = "", "", nil
	instance, err := instanceFor(agent, testImage, testAPIURL, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, unset := range []string{"AGENT_FLAVOR", "ANTHROPIC_MODEL"} {
		if _, set := instance.Env[unset]; set {
			t.Errorf("%s is set to %q", unset, instance.Env[unset])
		}
	}
	if instance.Env["AGENT_HYPERPARAMETERS"] != "{}" {
		t.Errorf("AGENT_HYPERPARAMETERS = %q, want {}", instance.Env["AGENT_HYPERPARAMETERS"])
	}
}

func TestTheReconcileEndpointAdmitsItsOwnLauncherAndStaffOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		caller auth.Principal
		want   int
	}{
		"its launcher":                  {mainLauncher, http.StatusOK},
		"staff":                         {auth.Principal{Kind: auth.KindStaff}, http.StatusOK},
		"another launcher":              {auth.Principal{Kind: auth.KindLauncher, LauncherName: "other"}, http.StatusForbidden},
		"runtime of a cluster so named": {auth.Principal{Kind: auth.KindRuntime, ClusterName: "main", OrgIDs: []string{"org"}}, http.StatusForbidden},
		"user":                          {auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org"}}, http.StatusForbidden},
		"api key":                       {auth.Principal{Kind: auth.KindKey, OrgIDs: []string{"org"}}, http.StatusForbidden},
		"coordinator":                   {auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1"}, http.StatusForbidden},
		"agent it launched":             {auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: "ag-1"}, http.StatusForbidden},
		"job":                           {auth.Principal{Kind: auth.KindJob, ExperimentID: "main"}, http.StatusForbidden},
	} {
		store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
		code, raw := reconcile(t, store, tc.caller, "main", zap.NewNop())
		if code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, code, raw, tc.want)
		}
		if tc.want != http.StatusOK && (len(store.asked) != 0 || strings.Contains(string(raw), "hls_")) {
			t.Errorf("%s: a refused caller reached the store or was shown a token: %s", name, raw)
		}
	}
}

func TestARequestWithNoTokenIsRefused(t *testing.T) {
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(mainLauncher), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	store := &launcherStore{}
	RegisterHuma(doc, NewHandler(store, testImage, testAPIURL, testSecret, zap.NewNop()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/main/reconcile", nil))
	if rec.Code != http.StatusUnauthorized || len(store.asked) != 0 {
		t.Fatalf("got = %d, store asked %v", rec.Code, store.asked)
	}
}

func TestAStoreFailureIsAnErrorAndNeverAnEmptyList(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{
		byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		err:        errors.New("connection refused to 10.0.0.5"),
	}
	code, raw := reconcile(t, store, mainLauncher, "main", zap.New(core))
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", code, raw)
	}
	if strings.Contains(string(raw), "instances") || strings.Contains(string(raw), "10.0.0.5") {
		t.Errorf("the failure answer reads as a list or leaks its cause: %s", raw)
	}
	if logs.Len() != 1 {
		t.Fatalf("logged %d entries, want 1", logs.Len())
	}
}

func TestNoTokenIsLogged(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	if code, raw := reconcile(t, store, mainLauncher, "main", zap.New(core)); code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	store.err = errors.New("down")
	reconcile(t, store, mainLauncher, "main", zap.New(core))
	for _, entry := range logs.All() {
		line, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(entry.Message+string(line), "hls_") {
			t.Errorf("a token was logged: %s %s", entry.Message, line)
		}
	}
}

var providerNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func TestAnInstanceNameIsStableReadableAndWithinEveryProvidersRules(t *testing.T) {
	env := map[string]string{"AGENT_ID": "x"}
	for _, ids := range [][2]string{
		{"pe-1a2b3c4d", "ag-0192f3a1b2c37def8123456789abcdef"},
		{strings.Repeat("p", 63), strings.Repeat("a", 63)},
		{"PE_Upper.Case", "Agent/With Spaces*"},
		{"-", "--"},
		{"пример", "代理"},
		{"", ""},
	} {
		name, err := instanceName(ids[0], ids[1], testImage, env)
		if err != nil {
			t.Fatal(err)
		}
		if !providerNamePattern.MatchString(name) || len(name) > MaxInstanceNameLength || len(strings.Split(name, "-")) != 3 {
			t.Errorf("name %q of %q: not three parts of a-z, 0-9 within %d characters", name, ids, MaxInstanceNameLength)
		}
		again, _ := instanceName(ids[0], ids[1], testImage, map[string]string{"AGENT_ID": "x"})
		if again != name {
			t.Errorf("name of %q changed between two calls: %q, %q", ids, name, again)
		}
	}
	// The launcher's prefix is at most hl-agent- and a 16 character namespace and a dash, and a
	// provider holds 128 characters.
	if len("hl-agent-")+16+1+MaxInstanceNameLength > 128 {
		t.Fatalf("a name of %d characters leaves no room for the launcher's prefix", MaxInstanceNameLength)
	}
}

func TestTwoAgentsNeverShareANameHoweverAlikeTheirIDsRead(t *testing.T) {
	seen := map[string]string{}
	long := strings.Repeat("a", 40)
	for _, ids := range [][2]string{
		{"pe-1", "agent-a"}, {"pe-1", "agenta"}, {"pe-1", "agent_a"}, {"pe1", "agent-a"}, {"pe-1a", "gent-a"},
		{"pe-1", long + "1"}, {"pe-1", long + "2"}, {"pe", "1-agent-a"},
	} {
		name, err := instanceName(ids[0], ids[1], testImage, nil)
		if err != nil {
			t.Fatal(err)
		}
		if other, taken := seen[name]; taken {
			t.Errorf("%q and %q share the name %q", ids, other, name)
		}
		seen[name] = ids[0] + "/" + ids[1]
	}
}

func TestANameChangesWithWhatTheInstanceRunsAndNotWithTheSecret(t *testing.T) {
	base := hosted("pe-1", "ag-1")
	name := func(agent domain.AgentSignup, image, apiURL, secret string) string {
		t.Helper()
		instance, err := instanceFor(agent, image, apiURL, secret)
		if err != nil {
			t.Fatal(err)
		}
		return instance.Name
	}
	original := name(base, testImage, testAPIURL, testSecret)

	changed := map[string]string{"image": name(base, "registry.example.invalid/agent:2", testAPIURL, testSecret)}
	changed["api url"] = name(base, testImage, "https://other.example.invalid", testSecret)
	for what, edit := range map[string]func(*domain.AgentSignup){
		"flavor":          func(a *domain.AgentSignup) { a.Flavor = "generalist" },
		"model":           func(a *domain.AgentSignup) { a.Model = "another-model" },
		"hyperparameters": func(a *domain.AgentSignup) { a.Hyperparameters = map[string]any{"batch_size": float64(32)} },
	} {
		agent := base
		edit(&agent)
		changed[what] = name(agent, testImage, testAPIURL, testSecret)
	}
	for what, got := range changed {
		if got == original {
			t.Errorf("a changed %s left the name %q as it was", what, got)
		}
	}

	// A rotated secret changes the token inside the environment. The name is derived from no
	// secret, so it stays, and the agent whose old token stops working is replaced as any
	// crashed one is.
	rotated, err := instanceFor(base, testImage, testAPIURL, "another-secret-of-the-same-length-xx")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := instanceFor(base, testImage, testAPIURL, testSecret)
	if rotated.Name != original || rotated.Env["HL_TOKEN"] == first.Env["HL_TOKEN"] {
		t.Errorf("after a secret rotation: name %q (was %q), token changed = %v", rotated.Name, original, rotated.Env["HL_TOKEN"] != first.Env["HL_TOKEN"])
	}
	if strings.Contains(original, first.Env["HL_TOKEN"]) {
		t.Error("the name holds the token")
	}
}

func TestAHandlerCannotBeBuiltWithoutWhatEveryInstanceNeeds(t *testing.T) {
	for name, build := range map[string]func(){
		"no store":  func() { NewHandler(nil, testImage, testAPIURL, testSecret, zap.NewNop()) },
		"no image":  func() { NewHandler(&launcherStore{}, "", testAPIURL, testSecret, zap.NewNop()) },
		"no url":    func() { NewHandler(&launcherStore{}, testImage, "", testSecret, zap.NewNop()) },
		"no secret": func() { NewHandler(&launcherStore{}, testImage, testAPIURL, "", zap.NewNop()) },
		"no logger": func() { NewHandler(&launcherStore{}, testImage, testAPIURL, testSecret, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: built", name)
				}
			}()
			build()
		}()
	}
}
