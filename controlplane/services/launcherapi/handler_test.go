package launcherapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

	"github.com/scaleresearch/hypothesisloop/controlplane/services/launcherapi/launchertest"
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

const testDigest = "0123456789abcdef0123456789abcdef"

type launcherStore = launchertest.Store

const testBootLimit = 5 * time.Minute

var testSettings = Settings{Image: testImage, APIURL: testAPIURL, SigningSecret: testSecret, BootLimit: testBootLimit, ProgressLimit: 4 * testBootLimit, MaxNewPerAnswer: 50}

func hosted(pe, agent string) domain.AgentSignup {
	return domain.AgentSignup{
		PlatformExperimentID: pe, AgentID: agent, Hosted: true, Launcher: "main",
		Flavor: "data-search", Model: "claude-sonnet-5", Hyperparameters: map[string]any{"batch_size": float64(64), "a": "b"},
		Instance: &domain.AgentInstance{},
	}
}

// named is a hosted agent whose machine was asked for before, under name.
func named(pe, agent, name string) domain.AgentSignup {
	signup := hosted(pe, agent)
	signup.Instance.Name = name
	return signup
}

// ended is a hosted agent whose run is over for reason.
func ended(signup domain.AgentSignup, reason domain.RunEnd) domain.AgentSignup {
	signup.Ended = reason
	return signup
}

func observed(instances ...ObservedInstance) Observed {
	if instances == nil {
		instances = []ObservedInstance{}
	}
	return Observed{LaunchDigest: testDigest, Instances: instances, ReconcileIntervalSeconds: 15, CreatePauseMilliseconds: 1000}
}

func reconcile(t *testing.T, store Store, caller auth.Principal, launcher string, logger *zap.Logger) (int, []byte) {
	t.Helper()
	return reconcileWith(t, store, caller, launcher, logger, observed())
}

func reconcileWith(t *testing.T, store Store, caller auth.Principal, launcher string, logger *zap.Logger, body any) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterHuma(doc, NewHandler(store, testSettings, logger))
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+launcher+"/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	return rec.Code, rec.Body.Bytes()
}

// mustReconcile is one launcher pass as the control plane sees it.
func mustReconcile(t *testing.T, store Store, body Observed) Desired {
	t.Helper()
	code, raw := reconcileWith(t, store, mainLauncher, "main", zap.NewNop(), body)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	return decode(t, raw)
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
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{
		"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: ended(named("pe-1", "ag-cut", "pe1-agcut-aaaaaaaaaaaa"), domain.RunEndCut)},
			{AgentSignup: ended(named("pe-2", "ag-closed", "pe2-agclosed-bbbbbbbbbbbb"), domain.RunEndClosed)},
			{AgentSignup: ended(hosted("pe-2", "ag-closed-before-it-ever-ran"), domain.RunEndClosed)},
			{AgentSignup: hosted("pe-3", "ag-open")},
		},
		"other": {{AgentSignup: hosted("pe-9", "ag-elsewhere")}},
	}}
	code, raw := reconcile(t, store, mainLauncher, "main", zap.NewNop())
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if !reflect.DeepEqual(store.Listed, []string{"main"}) || store.WindDown != domain.AgentWindDown {
		t.Fatalf("store asked for %v with wind-down %s", store.Listed, store.WindDown)
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
	// The token says which agent of which platform experiment the machine is, and the agent
	// reads the rest from its signup.
	wantEnv := map[string]string{"API_URL": testAPIURL, "HL_TOKEN": token}
	if running.Image != testImage || !reflect.DeepEqual(running.Env, wantEnv) {
		t.Errorf("instance = %+v\nwant env %v", running, wantEnv)
	}
	if !strings.HasPrefix(running.Name, "pe1-agrunning-") || !strings.HasPrefix(desired.Instances[1].Name, "pe3-agopen-") {
		t.Errorf("names = %q, %q", running.Name, desired.Instances[1].Name)
	}
	if !reflect.DeepEqual(desired.Ending, []string{"pe1-agcut-aaaaaaaaaaaa", "pe2-agclosed-bbbbbbbbbbbb"}) {
		t.Errorf("ending = %v, want the two names the instances were created under", desired.Ending)
	}
	if got := store.Find("pe-1", "ag-running").Name; got != running.Name {
		t.Errorf("the name asked for was recorded as %q, want %q", got, running.Name)
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

func TestAnAgentWhoseModelChangesGetsANewMachineAndASignupWithoutFlavorOrModelIsLeftOutAndLogged(t *testing.T) {
	agent := hosted("pe-1", "ag-1")
	instance, err := instanceFor(agent, agent.Instance.Restarts, testImage, testAPIURL, testSecret, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	agent.Model = "another-model"
	changed, err := instanceFor(agent, agent.Instance.Restarts, testImage, testAPIURL, testSecret, testDigest)
	if err != nil || changed.Name == instance.Name || !maps.Equal(changed.Env, instance.Env) {
		t.Errorf("after a change of model: got = %+v, %v, want another name than %s for the same environment", changed, err, instance.Name)
	}

	noFlavor, noModel := hosted("pe-1", "ag-no-flavor"), hosted("pe-1", "ag-no-model")
	noFlavor.Flavor, noModel.Model = "", ""
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: noFlavor}, {AgentSignup: hosted("pe-1", "ag-whole")}, {AgentSignup: noModel},
	}}}
	code, raw := reconcileWith(t, store, mainLauncher, "main", zap.New(core), observed())
	if code != http.StatusOK {
		t.Fatalf("one bad row failed the whole list: %d %s", code, raw)
	}
	desired := decode(t, raw)
	if len(desired.Instances) != 1 || !strings.HasPrefix(desired.Instances[0].Name, "pe1-agwhole-") {
		t.Fatalf("instances = %+v, want the one whole agent", desired.Instances)
	}
	if logs.Len() != 2 {
		t.Fatalf("logged %d entries, want one for each agent left out", logs.Len())
	}
}

func TestAnExitedInstanceIsRecordedAndItsAgentAskedForUnderANewNameUntilItsRestartsAreSpent(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: hosted("pe-1", "ag-crashing")}, {AgentSignup: hosted("pe-1", "ag-steady")},
	}}}
	first := mustReconcile(t, store, observed())
	if len(first.Instances) != 2 {
		t.Fatalf("instances = %+v", first.Instances)
	}
	crashing, steady := first.Instances[0].Name, first.Instances[1].Name
	seen := map[string]bool{crashing: true}

	for exit := 1; exit <= domain.MaxAgentInstanceRestarts; exit++ {
		answer := mustReconcile(t, store, observed(
			ObservedInstance{Name: crashing, State: "exited", Log: fmt.Sprintf("exit %d", exit)},
			ObservedInstance{Name: steady, State: "running"},
		))
		if len(answer.Instances) != 2 || answer.Instances[1].Name != steady {
			t.Fatalf("exit %d: instances = %+v, want both agents and the steady one unchanged", exit, answer.Instances)
		}
		next := answer.Instances[0].Name
		if seen[next] {
			t.Fatalf("exit %d: the agent is asked for under %q again, so its exited machine would be kept", exit, next)
		}
		seen[next] = true
		recorded := store.Find("pe-1", "ag-crashing")
		if recorded.Restarts != exit || recorded.Stopped || recorded.Name != next || recorded.ExitLog != fmt.Sprintf("exit %d", exit) {
			t.Fatalf("exit %d: recorded %+v", exit, recorded)
		}
		// The launcher could not destroy the old machine and reports it again: nothing is counted.
		again := mustReconcile(t, store, observed(ObservedInstance{Name: crashing, State: "exited", Log: "said again"}, ObservedInstance{Name: steady, State: "running"}))
		if again.Instances[0].Name != next || store.Find("pe-1", "ag-crashing").Restarts != exit {
			t.Fatalf("exit %d reported twice was counted twice", exit)
		}
		crashing = next
	}

	last := mustReconcile(t, store, observed(ObservedInstance{Name: crashing, State: "exited", Log: "the last words"}, ObservedInstance{Name: steady, State: "running"}))
	if len(last.Instances) != 1 || last.Instances[0].Name != steady || len(last.Ending) != 0 {
		t.Fatalf("after the last restart: %+v, want the agent in neither list", last)
	}
	recorded := store.Find("pe-1", "ag-crashing")
	if !recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts || recorded.ExitLog != "the last words" || recorded.ExitedAt == nil {
		t.Fatalf("recorded %+v, want stopped with its last log", recorded)
	}
	if after := mustReconcile(t, store, observed()); len(after.Instances) != 1 {
		t.Fatalf("a stopped agent is asked for again: %+v", after.Instances)
	}
}

// A provider that ends every instance at its own lifetime would otherwise stop a healthy agent
// at its fourth hour.
func TestAnInstanceItsProviderExpiredIsAskedForUnderANewNameEveryTimeAndSpendsNoRestart(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	seen := map[string]bool{agent: true, coordinator: true}
	for expiry := 1; expiry <= domain.MaxAgentInstanceRestarts+2; expiry++ {
		answer := mustReconcile(t, store, observed(
			ObservedInstance{Name: agent, State: "exited", Log: fmt.Sprintf("expiry %d", expiry), Expired: true},
			ObservedInstance{Name: coordinator, State: "exited", Expired: true},
		))
		if len(answer.Instances) != 2 || seen[answer.Instances[0].Name] || seen[answer.Instances[1].Name] {
			t.Fatalf("expiry %d: instances = %+v, want both under names not used before", expiry, answer.Instances)
		}
		for _, recorded := range []*domain.AgentInstance{store.Find("pe-1", "ag-1"), store.Find("pe-1", "")} {
			if recorded.Restarts != 0 || recorded.Stopped || recorded.Renewals != expiry || recorded.EndReason != string(db.InstanceExpired) || recorded.ExitedAt == nil {
				t.Fatalf("expiry %d: recorded %+v, want it expired with no restart spent", expiry, recorded)
			}
		}
		if got := store.Find("pe-1", "ag-1").ExitLog; got != fmt.Sprintf("expiry %d", expiry) {
			t.Fatalf("expiry %d: exit log %q", expiry, got)
		}
		// Reported again before the launcher destroyed it: nothing more is recorded.
		again := mustReconcile(t, store, observed(ObservedInstance{Name: agent, State: "exited", Expired: true}))
		if again.Instances[0].Name != answer.Instances[0].Name || store.Find("pe-1", "ag-1").Renewals != expiry {
			t.Fatalf("expiry %d reported twice was recorded twice", expiry)
		}
		agent, coordinator = answer.Instances[0].Name, answer.Instances[1].Name
		seen[agent], seen[coordinator] = true, true
	}

	// The restarts are all still there for a command that ends by itself.
	crashed := mustReconcile(t, store, observed(ObservedInstance{Name: agent, State: "exited", Log: "boom"}))
	if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceExited) || seen[crashed.Instances[0].Name] {
		t.Fatalf("a crash after the expiries: recorded %+v, asked for as %q", recorded, crashed.Instances[0].Name)
	}

	for _, state := range []string{"starting", "running", "unknown"} {
		body := observed(ObservedInstance{Name: agent, State: state, Expired: true})
		if code, raw := reconcileUnder(t, store, testSettings, body); code != http.StatusBadRequest || !strings.Contains(string(raw), "expired is reported of an exited instance") {
			t.Fatalf("expired of a %s instance: %d %s, want 400", state, code, raw)
		}
	}
}

func TestTheLogOfAnExitIsKeptToItsLastBytes(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	long := strings.Repeat("an early line that is dropped\n", 400) + "Traceback: the reason it exited"
	mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "exited", Log: long}))
	kept := store.Find("pe-1", "ag-1").ExitLog
	if len(kept) > domain.MaxAgentInstanceLogBytes || !strings.HasSuffix(kept, "Traceback: the reason it exited") || !strings.HasPrefix(kept, "an early line") {
		t.Fatalf("kept %d bytes ending %q", len(kept), kept[max(0, len(kept)-40):])
	}
}

func TestAChangeOfConfigOrOfTheLaunchersInputsRenamesRunningInstancesAndLeavesEndingOnesAsCreated(t *testing.T) {
	agents := func() map[string][]db.LauncherAgent {
		return map[string][]db.LauncherAgent{"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: ended(named("pe-1", "ag-cut", "pe1-agcut-createdunder1"), domain.RunEndCut)},
		}}
	}
	store := &launcherStore{ByLauncher: agents()}
	before := mustReconcile(t, store, observed())

	rotated := observed()
	rotated.LaunchDigest = "fedcba9876543210fedcba9876543210"
	after := mustReconcile(t, store, rotated)
	if after.Instances[0].Name == before.Instances[0].Name {
		t.Error("a launcher whose command, size or credential changed is answered with the same instance name")
	}
	if !reflect.DeepEqual(after.Ending, []string{"pe1-agcut-createdunder1"}) {
		t.Errorf("ending = %v, want the name the instance was created under", after.Ending)
	}
	if got := store.Find("pe-1", "ag-running").Name; got != after.Instances[0].Name {
		t.Errorf("recorded name %q, want the new one %q", got, after.Instances[0].Name)
	}

	// The same agents under another image: the handler is built anew, as after a config change.
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(mainLauncher), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	moved := testSettings
	moved.Image, moved.APIURL = "registry.example.invalid/agent:2", "https://moved.example.invalid"
	RegisterHuma(doc, NewHandler(store, moved, zap.NewNop()))
	raw, _ := json.Marshal(rotated)
	req := httptest.NewRequest(http.MethodPost, "/main/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	answer := decode(t, rec.Body.Bytes())
	if answer.Instances[0].Name == after.Instances[0].Name || answer.Instances[0].Image != "registry.example.invalid/agent:2" {
		t.Errorf("after an image change: %+v", answer.Instances[0])
	}
	if !reflect.DeepEqual(answer.Ending, []string{"pe1-agcut-createdunder1"}) {
		t.Errorf("after an image change the ending instance was renamed: %v", answer.Ending)
	}
}

func TestAnAgentWhoseWriteFailsIsLeftOutAndTheRestOfTheListStands(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{
		ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-broken")}, {AgentSignup: hosted("pe-1", "ag-fine")}}},
		WriteErr:   map[string]error{"ag-broken": errors.New("row lock timeout")},
	}
	code, raw := reconcileWith(t, store, mainLauncher, "main", zap.New(core), observed())
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if desired := decode(t, raw); len(desired.Instances) != 1 || !strings.HasPrefix(desired.Instances[0].Name, "pe1-agfine-") {
		t.Fatalf("instances = %+v", desired.Instances)
	}
	if logs.Len() != 1 {
		t.Fatalf("logged %d entries, want 1", logs.Len())
	}
}

func TestARequestThatDoesNotSayWhatWasObservedIsRefused(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	for what, body := range map[string]any{
		"no body fields":        map[string]any{},
		"no digest":             map[string]any{"instances": []any{}},
		"no instances":          map[string]any{"launch_digest": testDigest},
		"null instances":        map[string]any{"launch_digest": testDigest, "instances": nil},
		"digest not hex":        map[string]any{"launch_digest": "sk-ant-a-credential-sent-by-mistake", "instances": []any{}},
		"digest too short":      map[string]any{"launch_digest": "abc", "instances": []any{}},
		"unknown state":         map[string]any{"launch_digest": testDigest, "instances": []any{map[string]any{"name": "x", "state": "paused"}}},
		"instance with no name": map[string]any{"launch_digest": testDigest, "instances": []any{map[string]any{"name": "", "state": "running"}}},
		"unknown field":         map[string]any{"launch_digest": testDigest, "instances": []any{}, "desired": []any{}},
	} {
		store.Listed = nil
		code, raw := reconcileWith(t, store, mainLauncher, "main", zap.NewNop(), body)
		if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d: %s", what, code, raw)
		}
		if len(store.Listed) != 0 || strings.Contains(string(raw), "hls_") {
			t.Errorf("%s: a refused request reached the store or was shown a token", what)
		}
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
		store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
		code, raw := reconcile(t, store, tc.caller, "main", zap.NewNop())
		if code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, code, raw, tc.want)
		}
		if tc.want != http.StatusOK && (len(store.Listed) != 0 || strings.Contains(string(raw), "hls_")) {
			t.Errorf("%s: a refused caller reached the store or was shown a token: %s", name, raw)
		}
	}
}

func TestARequestWithNoTokenIsRefused(t *testing.T) {
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(mainLauncher), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	store := &launcherStore{}
	RegisterHuma(doc, NewHandler(store, testSettings, zap.NewNop()))
	rec := httptest.NewRecorder()
	raw, _ := json.Marshal(observed())
	req := httptest.NewRequest(http.MethodPost, "/main/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(store.Listed) != 0 {
		t.Fatalf("got = %d, store asked %v", rec.Code, store.Listed)
	}
}

func TestAStoreFailureIsAnErrorAndNeverAnEmptyList(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{
		ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		Err:        errors.New("connection refused to 10.0.0.5"),
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
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	if code, raw := reconcile(t, store, mainLauncher, "main", zap.New(core)); code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	store.Err = errors.New("down")
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
		name, err := instanceName(ids[0], ids[1], "", testImage, env, testDigest, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !providerNamePattern.MatchString(name) || len(name) > MaxInstanceNameLength || len(strings.Split(name, "-")) != 3 {
			t.Errorf("name %q of %q: not three parts of a-z, 0-9 within %d characters", name, ids, MaxInstanceNameLength)
		}
		again, _ := instanceName(ids[0], ids[1], "", testImage, map[string]string{"AGENT_ID": "x"}, testDigest, 0)
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
		name, err := instanceName(ids[0], ids[1], "", testImage, nil, testDigest, 0)
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
		instance, err := instanceFor(agent, agent.Instance.Restarts, image, apiURL, secret, testDigest)
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
	restarted := hosted("pe-1", "ag-1")
	restarted.Instance.Restarts = 1
	changed["restart count"] = name(restarted, testImage, testAPIURL, testSecret)
	otherLauncherInputs, err := instanceFor(base, 0, testImage, testAPIURL, testSecret, "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	changed["launcher's own inputs"] = otherLauncherInputs.Name
	for what, got := range changed {
		if got == original {
			t.Errorf("a changed %s left the name %q as it was", what, got)
		}
	}

	// A rotated secret changes the token inside the environment. The name is derived from no
	// secret, so it stays, and the agent whose old token stops working is replaced as any
	// crashed one is.
	rotated, err := instanceFor(base, 0, testImage, testAPIURL, "another-secret-of-the-same-length-xx", testDigest)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := instanceFor(base, 0, testImage, testAPIURL, testSecret, testDigest)
	if rotated.Name != original || rotated.Env["HL_TOKEN"] == first.Env["HL_TOKEN"] {
		t.Errorf("after a secret rotation: name %q (was %q), token changed = %v", rotated.Name, original, rotated.Env["HL_TOKEN"] != first.Env["HL_TOKEN"])
	}
	if strings.Contains(original, first.Env["HL_TOKEN"]) {
		t.Error("the name holds the token")
	}
}

func hostedCoordinator(pe string) db.LauncherCoordinator {
	return db.LauncherCoordinator{PlatformExperimentID: pe, HostedCoordinator: domain.HostedCoordinator{Launcher: "main", Model: "claude-sonnet-5"}}
}

func TestAHostedCoordinatorIsOneMoreInstanceWithACoordinatorTokenAndNoAgentID(t *testing.T) {
	store := &launcherStore{
		ByLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		Coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1"), {PlatformExperimentID: "pe-2", HostedCoordinator: domain.HostedCoordinator{Launcher: "other", Model: "m"}}},
	}
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 2 {
		t.Fatalf("instances = %+v, want the agent and the coordinator of this launcher", answer.Instances)
	}
	coordinator := answer.Instances[1]
	token, err := auth.Mint(testSecret, auth.CoordinatorScope("pe-1"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"API_URL": testAPIURL, "HL_TOKEN": token}
	if !maps.Equal(coordinator.Env, want) || coordinator.Image != testImage {
		t.Errorf("coordinator = %+v, want env %v", coordinator, want)
	}
	if !strings.HasPrefix(coordinator.Name, "pe1-coordinator-") || len(coordinator.Name) > MaxInstanceNameLength || strings.Contains(coordinator.Name, token) {
		t.Errorf("name = %q", coordinator.Name)
	}
	if store.Find("pe-1", "").Name != coordinator.Name {
		t.Error("the name asked for was not recorded")
	}
	if again := mustReconcile(t, store, observed(ObservedInstance{Name: coordinator.Name, State: "running"})); again.Instances[1].Name != coordinator.Name {
		t.Errorf("a second ask renamed the coordinator: %q then %q", coordinator.Name, again.Instances[1].Name)
	}
}

func TestAnAgentWhoseIDIsTheCoordinatorsNameGetsAnotherInstanceName(t *testing.T) {
	store := &launcherStore{
		ByLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "coordinator")}}},
		Coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")},
	}
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 2 || answer.Instances[0].Name == answer.Instances[1].Name {
		t.Fatalf("instances = %+v, want two names", answer.Instances)
	}
}

func reconcileUnder(t *testing.T, store Store, cfg Settings, body Observed) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(mainLauncher), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterHuma(doc, NewHandler(store, cfg, zap.NewNop()))
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/main/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	return rec.Code, rec.Body.Bytes()
}

func oneAgentAndACoordinator() *launcherStore {
	return &launcherStore{
		ByLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		Coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")},
	}
}

// What is reported of another name, of a machine that is running, and of one whose run is over
// spends nothing.
func TestOnlyTheEndOfTheMachineAskedForIsRecorded(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: hosted("pe-1", "ag-1")},
		{AgentSignup: ended(named("pe-1", "ag-ending", "pe1-agending-aaaaaaaaaaaa"), domain.RunEndPlanned)},
	}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	for _, reported := range [][]ObservedInstance{
		{{Name: name, State: "starting"}},
		{{Name: name, State: "running", Log: "ignored"}},
		{{Name: name, State: "running"}, {Name: "pe1-ag1-000000000000", State: "exited"}},
		{{Name: name, State: "running"}, {Name: "pe1-agending-aaaaaaaaaaaa", State: "exited", Log: "finished its last turn"}},
		{{Name: name, State: "running"}},
	} {
		store.Advance(testBootLimit / 2)
		answer := mustReconcile(t, store, observed(reported...))
		if len(answer.Instances) != 1 || answer.Instances[0].Name != name || !reflect.DeepEqual(answer.Ending, []string{"pe1-agending-aaaaaaaaaaaa"}) {
			t.Errorf("%+v: answer = %+v", reported, answer)
		}
		for _, agent := range []string{"ag-1", "ag-ending"} {
			if got := store.Find("pe-1", agent); got.Restarts != 0 || got.ExitedAt != nil {
				t.Errorf("%+v: recorded as an end of %s: %+v", reported, agent, got)
			}
		}
	}
}

func TestAMachineNeverListedIsGivenTheBootLimitAndThenSpendsARestart(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	names := []string{first.Instances[0].Name, first.Instances[1].Name}

	// Inside the limit the create is asked for again under the same name and costs nothing.
	store.Advance(testBootLimit - time.Second)
	if answer := mustReconcile(t, store, observed()); answer.Instances[0].Name != names[0] || answer.Instances[1].Name != names[1] {
		t.Fatalf("inside the limit: %+v, want both names unchanged", answer.Instances)
	}
	store.Advance(time.Second)
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 2 || answer.Instances[0].Name == names[0] || answer.Instances[1].Name == names[1] {
		t.Fatalf("at the limit: %+v, want both under new names", answer.Instances)
	}
	for _, agent := range []string{"ag-1", ""} {
		recorded := store.Find("pe-1", agent)
		if recorded.Restarts != 1 || recorded.Stopped || !strings.Contains(recorded.ExitLog, "no machine was listed 5m0s after it was asked for") {
			t.Fatalf("%q: recorded %+v, want a restart spent and why", agent, recorded)
		}
	}
	// The replacement's time starts when it is asked for.
	if again := mustReconcile(t, store, observed()); again.Instances[0].Name != answer.Instances[0].Name {
		t.Fatal("the replacement was given up on at once")
	}

	// Every restart ends the same way, and then nothing is asked of the launcher.
	for range domain.MaxAgentInstanceRestarts {
		store.Advance(testBootLimit)
		answer = mustReconcile(t, store, observed())
	}
	if len(answer.Instances) != 0 || !store.Find("pe-1", "ag-1").Stopped || !store.Find("pe-1", "").Stopped {
		t.Fatalf("after every restart: %+v, agent %+v, coordinator %+v", answer.Instances, store.Find("pe-1", "ag-1"), store.Find("pe-1", ""))
	}
}

func TestAMachineThatStaysStartingIsTimedFromWhenItWasFirstListed(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	starting := func() Observed {
		return observed(ObservedInstance{Name: agent, State: "starting"}, ObservedInstance{Name: coordinator, State: "starting"})
	}

	// First listed long after it was asked for, as the last machine of a slow pass is: the limit
	// starts now, and nothing is spent on the pass having been slow.
	store.Advance(3 * testBootLimit)
	if answer := mustReconcile(t, store, starting()); answer.Instances[0].Name != agent || store.Find("pe-1", "ag-1").SeenAt == nil {
		t.Fatalf("first listed: %+v, recorded %+v", answer.Instances, store.Find("pe-1", "ag-1"))
	}
	store.Advance(testBootLimit - time.Second)
	if answer := mustReconcile(t, store, starting()); answer.Instances[0].Name != agent || answer.Instances[1].Name != coordinator {
		t.Fatalf("inside the limit: %+v", answer.Instances)
	}
	store.Advance(time.Second)
	answer := mustReconcile(t, store, starting())
	if answer.Instances[0].Name == agent || answer.Instances[1].Name == coordinator {
		t.Fatalf("at the limit: %+v, want new names", answer.Instances)
	}
	if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 1 || !strings.Contains(recorded.ExitLog, "still starting 5m0s after it was first listed") {
		t.Fatalf("recorded %+v", recorded)
	}

	// Running is no boot: it is held to the progress limit, which is longer.
	next := answer.Instances[0].Name
	mustReconcile(t, store, observed(ObservedInstance{Name: next, State: "running"}, ObservedInstance{Name: answer.Instances[1].Name, State: "running"}))
	store.Advance(testSettings.ProgressLimit - time.Second)
	up := mustReconcile(t, store, observed(ObservedInstance{Name: next, State: "running"}, ObservedInstance{Name: answer.Instances[1].Name, State: "running"}))
	if up.Instances[0].Name != next || store.Find("pe-1", "ag-1").Restarts != 1 {
		t.Fatal("a running machine was held to the boot limit")
	}
}

// A provider that cannot reach a machine's host reports it unknown. One that ran is not held to
// the boot limit, which passed long ago: it stays as recorded and is given up on only when its
// command's last progress is a progress limit old. One that never ran is still starting.
func TestAMachineReportedUnknownIsHeldToTheProgressLimitOnceItRanAndToTheBootLimitBefore(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	report := func(agentState string, progress *string) Observed {
		return observed(ObservedInstance{Name: agent, State: agentState, Progress: progress}, ObservedInstance{Name: coordinator, State: "unknown"})
	}

	mustReconcile(t, store, report("running", marker("1")))
	store.Advance(testBootLimit)
	// The agent ran and its host is away for a pass. The coordinator never ran: its boot limit is up.
	answer := mustReconcile(t, store, report("unknown", nil))
	if recorded := store.Find("pe-1", "ag-1"); answer.Instances[0].Name != agent || recorded.Restarts != 0 || recorded.Reported != domain.InstanceRunning {
		t.Fatalf("a running machine unknown for a pass: %+v, recorded %+v, want it kept as running", answer.Instances, recorded)
	}
	if recorded := store.Find("pe-1", ""); answer.Instances[1].Name == coordinator || recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceStillStarting) {
		t.Fatalf("a machine unknown from its first listing: %+v, recorded %+v, want it replaced as still starting", answer.Instances, recorded)
	}

	// The host answers again and the agent goes on.
	back := observed(ObservedInstance{Name: agent, State: "running", Progress: marker("2")})
	store.Advance(testSettings.ProgressLimit / 2)
	if answer := mustReconcile(t, store, back); answer.Instances[0].Name != agent || store.Find("pe-1", "ag-1").Restarts != 0 {
		t.Fatalf("after the host answered again: %+v", answer.Instances)
	}

	// The host stays away: the agent is kept inside the progress limit and given up on at it.
	away := observed(ObservedInstance{Name: agent, State: "unknown"})
	store.Advance(testSettings.ProgressLimit - time.Second)
	if answer := mustReconcile(t, store, away); answer.Instances[0].Name != agent {
		t.Fatalf("unknown inside the progress limit: %+v, want it kept", answer.Instances)
	}
	store.Advance(time.Second)
	answer = mustReconcile(t, store, away)
	recorded := store.Find("pe-1", "ag-1")
	if answer.Instances[0].Name == agent || recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceSilent) || !strings.Contains(recorded.ExitLog, "stopped saying whether it still does") {
		t.Fatalf("unknown for a whole progress limit: %+v, recorded %+v, want it replaced as silent", answer.Instances, recorded)
	}
}

func TestAMachineThatWasListedAndIsGoneIsAnExitAtOnce(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	mustReconcile(t, store, observed(ObservedInstance{Name: agent, State: "running"}, ObservedInstance{Name: coordinator, State: "running"}))

	answer := mustReconcile(t, store, observed(ObservedInstance{Name: coordinator, State: "running"}))
	if len(answer.Instances) != 2 || answer.Instances[0].Name == agent || answer.Instances[1].Name != coordinator {
		t.Fatalf("answer = %+v, want the agent under a new name and the coordinator as it was", answer.Instances)
	}
	if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 1 || !strings.Contains(recorded.ExitLog, "no longer lists it") {
		t.Fatalf("recorded %+v", recorded)
	}
	if recorded := store.Find("pe-1", ""); recorded.Restarts != 0 {
		t.Fatalf("the coordinator, still listed: recorded %+v", recorded)
	}
	gone := mustReconcile(t, store, observed())
	if gone.Instances[1].Name == coordinator || store.Find("pe-1", "").Restarts != 1 {
		t.Fatalf("the coordinator gone: %+v, recorded %+v", gone.Instances, store.Find("pe-1", ""))
	}
}

// A planner that answered its draft is no longer asked for, and its launcher takes the machine
// down. A revision asks for the planner again: the machine that is not listed then was released
// by the platform, and is no machine its provider lost.
func TestAMachineThePlatformReleasedIsNotOneItsProviderLost(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-planner")}}}}
	first := mustReconcile(t, store, observed())
	machine := first.Instances[0].Name
	mustReconcile(t, store, observed(ObservedInstance{Name: machine, State: "running"}))

	store.Over = map[string]domain.RunEnd{"pe-1/ag-planner": domain.RunEndPlanned}
	if answer := mustReconcile(t, store, observed(ObservedInstance{Name: machine, State: "running"})); len(answer.Instances) != 0 || len(answer.Ending) != 0 {
		t.Fatalf("answer = %+v, want the machine no longer asked for", answer)
	}
	released := *store.Find("pe-1", "ag-planner")
	if released.Name != "" || released.EndReason != string(db.InstanceReleased) || released.Restarts != 0 || released.ExitLog != domain.RunEndPlanned.ReleasedBecause() {
		t.Fatalf("recorded %+v, want released on purpose", released)
	}

	store.Over = nil
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 1 {
		t.Fatalf("answer = %+v, want a machine for the revision", answer)
	}
	if recorded := store.Find("pe-1", "ag-planner"); recorded.Restarts != 0 || recorded.Stopped || recorded.EndReason != string(db.InstanceReleased) || strings.Contains(recorded.ExitLog, "lost") {
		t.Fatalf("recorded %+v, want no restart spent and no machine lost", recorded)
	}
}

// Restarts are spent under one image and one launcher config. What stopped an agent may have
// been either, for every agent of every org at once, so a change of either starts it again.
func TestAStoppedAgentIsAskedForAgainOnceTheImageOrTheLaunchersInputsChange(t *testing.T) {
	stopped := func() (*launcherStore, string) {
		store := oneAgentAndACoordinator()
		answer := mustReconcile(t, store, observed())
		for len(answer.Instances) > 0 {
			var exited []ObservedInstance
			for _, instance := range answer.Instances {
				exited = append(exited, ObservedInstance{Name: instance.Name, State: "exited", Log: "the credential was refused"})
			}
			answer = mustReconcile(t, store, observed(exited...))
		}
		if !store.Find("pe-1", "ag-1").Stopped || !store.Find("pe-1", "").Stopped {
			t.Fatal("not stopped after every restart")
		}
		return store, store.Find("pe-1", "ag-1").Launch
	}

	store, launch := stopped()
	if again := mustReconcile(t, store, observed()); len(again.Instances) != 0 {
		t.Fatalf("under the same image and launcher inputs: %+v, want nothing asked for", again.Instances)
	}
	rotated := observed()
	rotated.LaunchDigest = "fedcba9876543210fedcba9876543210"
	answer := mustReconcile(t, store, rotated)
	if len(answer.Instances) != 2 {
		t.Fatalf("after the launcher's inputs changed: %+v, want the agent and the coordinator", answer.Instances)
	}
	for _, agent := range []string{"ag-1", ""} {
		if recorded := store.Find("pe-1", agent); recorded.Stopped || recorded.Restarts != 0 || recorded.Launch == launch || recorded.Name == "" {
			t.Fatalf("%q: recorded %+v, want a fresh budget under the new launch", agent, recorded)
		}
	}
	// The budget is whole again, and no more than whole.
	for exit := 1; exit <= domain.MaxAgentInstanceRestarts+1; exit++ {
		body := rotated
		body.Instances = []ObservedInstance{{Name: answer.Instances[0].Name, State: "exited"}, {Name: answer.Instances[len(answer.Instances)-1].Name, State: "running"}}
		answer = mustReconcile(t, store, body)
	}
	if recorded := store.Find("pe-1", "ag-1"); !recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts {
		t.Fatalf("after the new budget was spent: %+v", recorded)
	}

	store, _ = stopped()
	newImage := testSettings
	newImage.Image = "registry.example.invalid/agent:2"
	code, raw := reconcileUnder(t, store, newImage, observed())
	if code != http.StatusOK || len(decode(t, raw).Instances) != 2 || store.Find("pe-1", "ag-1").Stopped {
		t.Fatalf("after the image changed: %d %s", code, raw)
	}
}

// A machine whose name config has since changed is replaced, and what became of it is no end:
// the launcher destroys it because it is no longer asked for.
func TestAMachineReplacedByAConfigChangeSpendsNoRestartWhenItIsGone(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	running := observed(ObservedInstance{Name: first.Instances[0].Name, State: "running"}, ObservedInstance{Name: first.Instances[1].Name, State: "running"})
	mustReconcile(t, store, running)

	moved := testSettings
	moved.APIURL = "https://moved.example.invalid"
	code, raw := reconcileUnder(t, store, moved, observed())
	answer := decode(t, raw)
	if code != http.StatusOK || answer.Instances[0].Name == first.Instances[0].Name {
		t.Fatalf("after the address changed: %d %+v", code, answer.Instances)
	}
	if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 0 || recorded.ExitedAt != nil {
		t.Fatalf("the replaced machine was recorded as an end: %+v", recorded)
	}
}

// A launcher creates one machine after another, so an answer asks for a bounded number of new
// ones: its pass ends, and its machines are reported, well inside the boot limit.
func TestAnAnswerAsksForABoundedNumberOfNewMachinesAndTheNextForTheRest(t *testing.T) {
	var agents []db.LauncherAgent
	for i := range 7 {
		agents = append(agents, db.LauncherAgent{AgentSignup: hosted("pe-1", fmt.Sprintf("ag-%d", i))})
	}
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": agents}, Coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")}}
	cfg := testSettings
	cfg.MaxNewPerAnswer = 3
	var listed []ObservedInstance
	sizes := []int{}
	for range 4 {
		// Each pass takes longer than the boot limit, which would spend a restart of every
		// machine asked for and not yet created, were all eight asked for at once.
		store.Advance(testBootLimit)
		code, raw := reconcileUnder(t, store, cfg, observed(listed...))
		if code != http.StatusOK {
			t.Fatalf("status = %d %s", code, raw)
		}
		answer := decode(t, raw)
		sizes = append(sizes, len(answer.Instances))
		listed = nil
		for _, instance := range answer.Instances {
			listed = append(listed, ObservedInstance{Name: instance.Name, State: "running"})
		}
	}
	if !reflect.DeepEqual(sizes, []int{3, 6, 8, 8}) {
		t.Fatalf("instances per answer = %v, want three more each time", sizes)
	}
	for i := range 7 {
		if recorded := store.Find("pe-1", fmt.Sprintf("ag-%d", i)); recorded.Restarts != 0 {
			t.Fatalf("ag-%d: %+v, want no restart spent on waiting its turn", i, recorded)
		}
	}
	if store.Find("pe-1", "").Restarts != 0 {
		t.Fatal("the coordinator spent a restart waiting its turn")
	}
}

func TestALauncherWhosePassCannotFitTheBootLimitIsRefusedWithTheArithmetic(t *testing.T) {
	store := oneAgentAndACoordinator()
	cfg := testSettings
	cfg.MaxNewPerAnswer, cfg.BootLimit = 10, 50*time.Second
	fits := observed()
	fits.ReconcileIntervalSeconds, fits.CreatePauseMilliseconds = 15, 1000
	if code, raw := reconcileUnder(t, store, cfg, fits); code != http.StatusOK {
		t.Fatalf("2 x (10 x 1s + 15s) = 50s against a limit of 50s: %d %s", code, raw)
	}
	for name, edit := range map[string]func(*Observed){
		"a longer pause":    func(o *Observed) { o.CreatePauseMilliseconds = 1100 },
		"a longer interval": func(o *Observed) { o.ReconcileIntervalSeconds = 16 },
	} {
		body := fits
		edit(&body)
		code, raw := reconcileUnder(t, store, cfg, body)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d %s, want 200 with the refusal stated", name, code, raw)
		}
		refusal := decode(t, raw).Refusal
		if refusal == nil || refusal.Reason != "pass_does_not_fit" {
			t.Fatalf("%s: refusal = %+v, want pass_does_not_fit: %s", name, refusal, raw)
		}
		for _, part := range []string{"10 new instances per answer", "agents.boot_limit_seconds 50s", "create_pause", "reconcile_interval"} {
			if !strings.Contains(refusal.Message, part) {
				t.Errorf("%s: the refusal does not say %q: %s", name, part, raw)
			}
		}
	}
	for name, edit := range map[string]func(*Observed){
		"no interval":      func(o *Observed) { o.ReconcileIntervalSeconds = 0 },
		"a negative pause": func(o *Observed) { o.CreatePauseMilliseconds = -1 },
	} {
		body := fits
		edit(&body)
		if code, raw := reconcileUnder(t, store, cfg, body); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, code, raw)
		}
	}
}

// A refused launcher still holds machines only it can end, so the refusal stops creation and
// nothing else: the lists are answered as ever, and nothing that does not exist is asked for.
func TestARefusedLauncherIsToldWhatToKeepAndWhatEndsAndIsAskedForNothingNew(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: hosted("pe-1", "ag-kept")}, {AgentSignup: hosted("pe-1", "ag-over")},
	}}}
	first := mustReconcile(t, store, observed())
	if len(first.Instances) != 2 || first.Refusal != nil {
		t.Fatalf("a launcher that fits: %+v", first)
	}
	kept, over := first.Instances[0].Name, first.Instances[1].Name

	store.ByLauncher["main"][1].Ended = domain.RunEndClosed
	store.ByLauncher["main"] = append(store.ByLauncher["main"], db.LauncherAgent{AgentSignup: hosted("pe-2", "ag-new")})
	slow := func(instances ...ObservedInstance) Observed {
		body := observed(instances...)
		body.ReconcileIntervalSeconds = 600
		return body
	}
	refused := mustReconcile(t, store, slow(ObservedInstance{Name: kept, State: "running"}, ObservedInstance{Name: over, State: "running"}))
	if refused.Refusal == nil || refused.Refusal.Reason != "pass_does_not_fit" || !strings.Contains(refused.Refusal.Message, "agents.boot_limit_seconds") {
		t.Fatalf("refusal = %+v, want pass_does_not_fit with the arithmetic", refused.Refusal)
	}
	if len(refused.Instances) != 1 || refused.Instances[0].Name != kept || !reflect.DeepEqual(refused.Ending, []string{over}) {
		t.Fatalf("refused answer = %+v, want %q kept and %q ending", refused, kept, over)
	}
	if asked := store.Find("pe-2", "ag-new").Name; asked != "" {
		t.Fatalf("a refused launcher was asked for a new machine, %q", asked)
	}
	if !store.Holds["main"] {
		t.Fatal("a refused launcher that lists machines is recorded as holding none")
	}

	// An exit is recorded while refused, and its replacement waits for a launcher that fits.
	exited := mustReconcile(t, store, slow(ObservedInstance{Name: kept, State: "exited", Log: "boom"}, ObservedInstance{Name: over, State: "running"}))
	if recorded := store.Find("pe-1", "ag-kept"); recorded.Restarts != 1 || recorded.ExitLog != "boom" || recorded.Name != "" {
		t.Fatalf("an exit reported by a refused launcher: recorded %+v", recorded)
	}
	if len(exited.Instances) != 0 || !reflect.DeepEqual(exited.Ending, []string{over}) {
		t.Fatalf("refused answer after an exit = %+v, want nothing to keep and %q ending", exited, over)
	}

	fitting := mustReconcile(t, store, observed(ObservedInstance{Name: over, State: "running"}))
	if fitting.Refusal != nil || len(fitting.Instances) != 2 || fitting.Instances[0].Name == kept || store.Find("pe-2", "ag-new").Name == "" {
		t.Fatalf("once it fits: %+v, want the replacement and the new machine asked for", fitting)
	}
}

func TestEveryInstanceCarriesTheMomentItsPlatformExperimentCanLastUntil(t *testing.T) {
	agentEnd := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	coordinatorEnd := agentEnd.Add(time.Hour)
	store := oneAgentAndACoordinator()
	store.ByLauncher["main"][0].EndsBy = agentEnd.In(time.FixedZone("elsewhere", 7200))
	store.Coordinators[0].EndsBy = coordinatorEnd
	code, raw := reconcileUnder(t, store, testSettings, observed())
	if code != http.StatusOK || !strings.Contains(string(raw), `"ends_at":"2026-03-01T12:00:00Z"`) {
		t.Fatalf("answer = %d %s, want the agent's ends_at in UTC", code, raw)
	}
	answer := decode(t, raw)
	if !answer.Instances[0].EndsAt.Equal(agentEnd) || !answer.Instances[1].EndsAt.Equal(coordinatorEnd) {
		t.Fatalf("ends_at = %s and %s", answer.Instances[0].EndsAt, answer.Instances[1].EndsAt)
	}
	named := mustReconcile(t, store, observed())
	store.ByLauncher["main"][0].EndsBy = agentEnd.Add(48 * time.Hour)
	if moved := mustReconcile(t, store, observed()); moved.Instances[0].Name != named.Instances[0].Name || !moved.Instances[0].EndsAt.Equal(agentEnd.Add(48*time.Hour)) {
		t.Fatalf("a moved bound: %+v, want the same name with the new ends_at", moved.Instances[0])
	}
}

func TestTheTokenMintedForAMachineIsTakenOutOfItsExitLog(t *testing.T) {
	store := oneAgentAndACoordinator()
	answer := mustReconcile(t, store, observed())
	var exited []ObservedInstance
	for _, instance := range answer.Instances {
		exited = append(exited, ObservedInstance{Name: instance.Name, State: "exited", Log: "env dump: HL_TOKEN=" + instance.Env["HL_TOKEN"] + "\nTraceback"})
	}
	mustReconcile(t, store, observed(exited...))
	for i, agent := range []string{"ag-1", ""} {
		kept := store.Find("pe-1", agent).ExitLog
		if strings.Contains(kept, answer.Instances[i].Env["HL_TOKEN"]) || strings.Contains(kept, "hls_") {
			t.Fatalf("%q: the stored log holds the token: %s", agent, kept)
		}
		if !strings.Contains(kept, "HL_TOKEN=<HL_TOKEN>") || !strings.HasSuffix(kept, "Traceback") {
			t.Fatalf("%q: stored %q, want the token named and the rest kept", agent, kept)
		}
	}
}

// A platform experiment that names its own image has its agents and its coordinator boot it, and
// every other one boots the platform's. Restarts are spent per image: a machine stopped under
// the platform's image says nothing about one that boots another.
func TestAPlatformExperimentsOwnImageIsWhatItsAgentsAndCoordinatorBoot(t *testing.T) {
	const own = "registry.example.invalid/team/agent:7"
	withImage := hostedCoordinator("pe-own")
	withImage.AgentImage = own
	store := &launcherStore{
		ByLauncher: map[string][]db.LauncherAgent{"main": {
			{AgentSignup: hosted("pe-own", "ag-1"), AgentImage: own},
			{AgentSignup: hosted("pe-plain", "ag-2")},
		}},
		Coordinators: []db.LauncherCoordinator{withImage, hostedCoordinator("pe-plain")},
	}
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 4 {
		t.Fatalf("instances = %+v, want two agents and two coordinators", answer.Instances)
	}
	images := map[string]string{}
	for _, instance := range answer.Instances {
		images[instance.Name[:strings.LastIndex(instance.Name, "-")]] = instance.Image
	}
	want := map[string]string{"peown-ag1": own, "peown-coordinator": own, "peplain-ag2": testImage, "peplain-coordinator": testImage}
	if !maps.Equal(images, want) {
		t.Fatalf("images = %v, want %v", images, want)
	}
	if store.Find("pe-own", "ag-1").Launch == store.Find("pe-plain", "ag-2").Launch {
		t.Fatal("two images share one launch, so restarts spent under one would count against the other")
	}
	if store.Find("pe-own", "ag-1").Launch != launchOf(own, testDigest) || store.Find("pe-own", "").Launch != launchOf(own, testDigest) {
		t.Fatal("the launch recorded for the experiment's machines is not that of its own image")
	}
}

func marker(m string) *string { return &m }

func TestARunningMachineThatReportsNoProgressForTheLimitIsEndedAndReplaced(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	running := func(agentProgress, coordinatorProgress *string) Observed {
		return observed(ObservedInstance{Name: agent, State: "running", Progress: agentProgress},
			ObservedInstance{Name: coordinator, State: "running", Progress: coordinatorProgress})
	}
	limit := testSettings.ProgressLimit

	// Both work. The coordinator keeps printing, the agent's marker stands still from here on.
	mustReconcile(t, store, running(marker("1"), marker("1")))
	for step := 2; step <= 5; step++ {
		store.Advance(limit / 4)
		answer := mustReconcile(t, store, running(marker("1"), marker(fmt.Sprint(step))))
		if step < 5 && (answer.Instances[0].Name != agent || answer.Instances[1].Name != coordinator) {
			t.Fatalf("%s after the agent's last progress: %+v, want both kept", time.Duration(step-1)*limit/4, answer.Instances)
		}
		if step == 5 && (answer.Instances[0].Name == agent || answer.Instances[1].Name != coordinator) {
			t.Fatalf("a whole limit after the agent's last progress: %+v, want the agent under a new name and the coordinator kept", answer.Instances)
		}
	}
	recorded := store.Find("pe-1", "ag-1")
	if recorded.Restarts != 1 || recorded.EndReason != "silent" || !strings.Contains(recorded.ExitLog, "reported no progress for "+limit.String()) {
		t.Fatalf("recorded %+v", recorded)
	}
	if kept := store.Find("pe-1", ""); kept.Restarts != 0 || kept.ProgressAt == nil {
		t.Fatalf("the coordinator, which made progress: %+v", kept)
	}
}

func TestAMachineThatNeverPrintsAMarkerIsSilentFromItsFirstSight(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	quiet := observed(ObservedInstance{Name: first.Instances[0].Name, State: "running", Progress: marker("")},
		ObservedInstance{Name: first.Instances[1].Name, State: "running", Progress: marker("")})
	mustReconcile(t, store, quiet)
	store.Advance(testSettings.ProgressLimit - time.Second)
	if answer := mustReconcile(t, store, quiet); answer.Instances[0].Name != first.Instances[0].Name {
		t.Fatal("ended inside the limit")
	}
	store.Advance(time.Second)
	answer := mustReconcile(t, store, quiet)
	if answer.Instances[0].Name == first.Instances[0].Name || answer.Instances[1].Name == first.Instances[1].Name {
		t.Fatalf("at the limit: %+v, want both replaced", answer.Instances)
	}
}

// With a provider's log down no marker is read, and a hung agent would run to the end of its
// experiment. A machine whose progress cannot be read is held to the limit a silent one is, by
// the same clock. Its end says nothing of its command, so an outage however long spends no
// restart and stops no agent.
func TestAMachineWhoseProgressCannotBeReadForTheLimitIsReplacedAndSpendsNoRestart(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	limit := testSettings.ProgressLimit
	unread := func() Observed {
		return observed(ObservedInstance{Name: agent, State: "running"}, ObservedInstance{Name: coordinator, State: "running"})
	}
	// The limit runs from the last progress recorded, and with none from the first sight.
	mustReconcile(t, store, observed(ObservedInstance{Name: agent, State: "running", Progress: marker("1")}, ObservedInstance{Name: coordinator, State: "running"}))
	store.Advance(limit / 2)
	mustReconcile(t, store, observed(ObservedInstance{Name: agent, State: "running", Progress: marker("2")}, ObservedInstance{Name: coordinator, State: "running"}))
	store.Advance(limit/2 - time.Second)
	answer := mustReconcile(t, store, unread())
	if answer.Instances[0].Name != agent || answer.Instances[1].Name != coordinator {
		t.Fatalf("inside the limit of the agent's last progress and of the coordinator's first sight: %+v, want both kept", answer.Instances)
	}
	store.Advance(time.Second)
	answer = mustReconcile(t, store, unread())
	if answer.Instances[0].Name != agent || answer.Instances[1].Name == coordinator {
		t.Fatalf("a limit after the coordinator was first seen: %+v, want it replaced and the agent kept", answer.Instances)
	}
	coordinator = answer.Instances[1].Name
	store.Advance(limit / 2)
	answer = mustReconcile(t, store, unread())
	if answer.Instances[0].Name == agent {
		t.Fatalf("a limit after the agent's last progress: %+v, want it replaced", answer.Instances)
	}
	recorded := store.Find("pe-1", "ag-1")
	if recorded.Restarts != 0 || recorded.Renewals != 1 || recorded.Stopped || recorded.EndReason != string(db.InstanceProgressUnreadable) || !strings.Contains(recorded.ExitLog, "cannot read its output") || !strings.Contains(recorded.ExitLog, limit.String()) {
		t.Fatalf("recorded %+v, want it ended as progress unreadable with no restart spent", recorded)
	}

	// The outage lasts: every machine is replaced once a limit, and the agent is never stopped.
	seen := map[string]bool{agent: true}
	agent = answer.Instances[0].Name
	for round := 2; round <= domain.MaxAgentInstanceRestarts+3; round++ {
		mustReconcile(t, store, unread())
		store.Advance(limit - time.Second)
		if kept := mustReconcile(t, store, unread()); kept.Instances[0].Name != agent {
			t.Fatalf("round %d: replaced inside the limit", round)
		}
		store.Advance(time.Second)
		answer = mustReconcile(t, store, unread())
		if len(answer.Instances) != 2 || answer.Instances[0].Name == agent || seen[answer.Instances[0].Name] {
			t.Fatalf("round %d: %+v, want the agent under a name not used before", round, answer.Instances)
		}
		seen[agent] = true
		agent, coordinator = answer.Instances[0].Name, answer.Instances[1].Name
		if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 0 || recorded.Renewals != round || recorded.Stopped {
			t.Fatalf("round %d: recorded %+v", round, recorded)
		}
	}

	// The log is back and the marker stands where it stood: that is a silent machine, and a restart.
	mustReconcile(t, store, unread())
	store.Advance(limit)
	stuck := observed(ObservedInstance{Name: agent, State: "running", Progress: marker("")}, ObservedInstance{Name: coordinator, State: "running", Progress: marker("")})
	if answer := mustReconcile(t, store, stuck); answer.Instances[0].Name == agent {
		t.Fatal("a machine read again and silent for the limit was kept")
	}
	if recorded := store.Find("pe-1", "ag-1"); recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceSilent) {
		t.Fatalf("read again and silent: recorded %+v", recorded)
	}
}

func TestProgressIsWrittenDownAFewTimesALimitHoweverOftenItIsReported(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	const reports = 200
	for i := range reports {
		store.Advance(2 * testSettings.ProgressLimit / reports)
		mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "running", Progress: marker(fmt.Sprint(i))}))
	}
	if store.ProgressWrites < 8 || store.ProgressWrites > 9 {
		t.Fatalf("%d reports over two limits wrote progress %d times, want four a limit", reports, store.ProgressWrites)
	}
	if store.Find("pe-1", "ag-1").Restarts != 0 {
		t.Fatal("an agent that kept making progress was ended")
	}
}

func TestProgressOfAnInstanceThatIsNotRunningAndALauncherTooSlowForTheProgressLimitAreRefused(t *testing.T) {
	store := oneAgentAndACoordinator()
	body := observed(ObservedInstance{Name: "pe1-ag1-000000000000", State: "starting", Progress: marker("1")})
	if code, raw := reconcileUnder(t, store, testSettings, body); code != http.StatusBadRequest {
		t.Fatalf("progress of a starting instance: %d %s, want 400", code, raw)
	}
	cfg := testSettings
	cfg.MaxNewPerAnswer, cfg.BootLimit, cfg.ProgressLimit = 10, time.Hour, 49*time.Second
	code, raw := reconcileUnder(t, store, cfg, observed())
	if code != http.StatusOK || !strings.Contains(string(raw), `"reason":"pass_does_not_fit"`) || !strings.Contains(string(raw), "agents.progress_limit_seconds 49s") {
		t.Fatalf("2 x (10 x 1s + 15s) = 50s against a progress limit of 49s: %d %s", code, raw)
	}
}

// A launcher is removed only when it holds nothing, so every question records whether it does
// once it has acted on the answer: it listed a machine, which it keeps or is about to destroy
// and has not yet reported gone, or the answer asks it for one.
func TestEveryQuestionRecordsWhetherTheLauncherHoldsMachines(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	asked := mustReconcile(t, store, observed())
	if len(asked.Instances) != 1 || !store.Holds["main"] {
		t.Fatalf("asked for %d machines, holds %v, want one and held", len(asked.Instances), store.Holds["main"])
	}
	// Its agent's run is long over: the machine is in neither list, and still rented.
	store.ByLauncher["main"] = nil
	leftover := mustReconcile(t, store, observed(ObservedInstance{Name: asked.Instances[0].Name, State: stateRunning}))
	if len(leftover.Instances) != 0 || !store.Holds["main"] {
		t.Fatalf("answered %d machines, holds %v, want none and still held until it is reported gone", len(leftover.Instances), store.Holds["main"])
	}
	mustReconcile(t, store, observed())
	if store.Holds["main"] {
		t.Fatal("a launcher that lists nothing and is asked for nothing is recorded as holding machines")
	}
}

// Once its platform experiment has an agent environment, an agent's next machine boots it, and a
// machine already running in the base keeps running in it: the declaration of the baseline is
// made from such a machine, and replacing it would cut that turn short.
func TestAMachineBootsTheAgentEnvironmentOnceThereIsOneAndARunningOneKeepsItsImage(t *testing.T) {
	const environment = "registry.example.invalid/hypothesisloop-environment:abc"
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-running")}}}}
	first := mustReconcile(t, store, observed())
	if first.Instances[0].Image != testImage {
		t.Fatalf("before an environment: %s, want the agent image", first.Instances[0].Image)
	}
	running := first.Instances[0].Name
	progress := "p1"

	store.ByLauncher["main"][0].EnvironmentImage = environment
	store.ByLauncher["main"] = append(store.ByLauncher["main"], db.LauncherAgent{AgentSignup: hosted("pe-1", "ag-new"), EnvironmentImage: environment})
	answer := mustReconcile(t, store, observed(ObservedInstance{Name: running, State: stateRunning, Progress: &progress}))
	images := map[string]string{}
	for _, instance := range answer.Instances {
		images[instance.Name] = instance.Image
	}
	if images[running] != testImage || len(images) != 2 {
		t.Fatalf("answer %v, want %s kept in the agent image beside one new machine", images, running)
	}
	for name, image := range images {
		if name != running && image != environment {
			t.Errorf("the new agent's machine boots %s, want the environment", image)
		}
	}

	exited := mustReconcile(t, store, observed(ObservedInstance{Name: running, State: stateExited, Log: "done"}))
	next := store.Find("pe-1", "ag-running").Name
	for _, instance := range exited.Instances {
		if instance.Name == next && (next == running || instance.Image != environment) {
			t.Errorf("after its machine ended the agent is asked for as %s in %s, want a new machine in the environment", next, instance.Image)
		}
	}
	if next == "" || next == running {
		t.Errorf("after its machine ended the agent holds %q, want a new machine", next)
	}
}
