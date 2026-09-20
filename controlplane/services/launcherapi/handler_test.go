package launcherapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
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

const testDigest = "0123456789abcdef0123456789abcdef"

// launcherStore holds signups as the database does and applies the two writes with the store's
// own conditions, so a test can follow an agent over several reconciles.
type launcherStore struct {
	byLauncher map[string][]db.LauncherAgent
	err        error
	writeErr   map[string]error
	asked      []string
	windDown   time.Duration
	exitLogs   map[string]string
	// askedAgo is how long ago each agent's current instance name was first asked for.
	askedAgo     map[string]time.Duration
	coordinators []db.LauncherCoordinator
}

func (s *launcherStore) coordinator(pe string) *db.LauncherCoordinator {
	for i := range s.coordinators {
		if s.coordinators[i].PlatformExperimentID == pe {
			return &s.coordinators[i]
		}
	}
	panic("no coordinator of " + pe)
}

func (s *launcherStore) ListLauncherCoordinators(_ context.Context, launcher string, _ time.Duration) ([]db.LauncherCoordinator, error) {
	listed := []db.LauncherCoordinator{}
	for _, coordinator := range s.coordinators {
		if coordinator.Launcher == launcher && !coordinator.Instance.Stopped {
			listed = append(listed, coordinator)
		}
	}
	return listed, nil
}

func (s *launcherStore) SetCoordinatorInstanceName(_ context.Context, pe, name string) error {
	s.coordinator(pe).Instance.Name = name
	delete(s.askedAgo, coordinatorKey+pe)
	return nil
}

func (s *launcherStore) RecordCoordinatorInstanceExit(_ context.Context, pe, name, exitLog string, maxRestarts int) (bool, error) {
	instance := &s.coordinator(pe).Instance
	if instance.Stopped || name == "" || instance.Name != name {
		return false, nil
	}
	now := time.Now()
	instance.Name, instance.ExitedAt, instance.ExitLog = "", &now, exitLog
	if instance.Restarts >= maxRestarts {
		instance.Stopped = true
	} else {
		instance.Restarts++
	}
	return true, nil
}

func (s *launcherStore) RecordCoordinatorInstanceStuck(ctx context.Context, pe, name, why string, bootLimit time.Duration, maxRestarts int) (bool, error) {
	if s.askedAgo[coordinatorKey+pe] <= bootLimit {
		return false, nil
	}
	return s.RecordCoordinatorInstanceExit(ctx, pe, name, why, maxRestarts)
}

// coordinatorKey prefixes a platform experiment's id where askedAgo keeps its coordinator.
const coordinatorKey = "coordinator of "

const testBootLimit = 5 * time.Minute

func (s *launcherStore) RecordAgentInstanceStuck(ctx context.Context, pe, agent, name, why string, bootLimit time.Duration, maxRestarts int) (bool, error) {
	if err := s.writeErr[agent]; err != nil {
		return false, err
	}
	if s.askedAgo[agent] <= bootLimit {
		return false, nil
	}
	return s.RecordAgentInstanceExit(ctx, pe, agent, name, why, maxRestarts)
}

func (s *launcherStore) ListLauncherAgents(_ context.Context, launcher string, windDown time.Duration) ([]db.LauncherAgent, error) {
	s.asked = append(s.asked, launcher)
	s.windDown = windDown
	listed := []db.LauncherAgent{}
	for _, agent := range s.byLauncher[launcher] {
		if agent.Instance != nil && agent.Instance.Stopped {
			continue
		}
		if agent.Instance != nil {
			instance := *agent.Instance
			agent.Instance = &instance
		}
		listed = append(listed, agent)
	}
	return listed, s.err
}

func (s *launcherStore) find(pe, agent string) *domain.AgentInstance {
	for _, agents := range s.byLauncher {
		for i := range agents {
			if agents[i].PlatformExperimentID == pe && agents[i].AgentID == agent {
				return agents[i].Instance
			}
		}
	}
	return nil
}

func (s *launcherStore) SetAgentInstanceName(_ context.Context, pe, agent, name string) error {
	if err := s.writeErr[agent]; err != nil {
		return err
	}
	s.find(pe, agent).Name = name
	delete(s.askedAgo, agent)
	return nil
}

func (s *launcherStore) RecordAgentInstanceExit(_ context.Context, pe, agent, name, exitLog string, maxRestarts int) (bool, error) {
	if err := s.writeErr[agent]; err != nil {
		return false, err
	}
	instance := s.find(pe, agent)
	if instance.Stopped || name == "" || instance.Name != name {
		return false, nil
	}
	now := time.Now()
	instance.Name, instance.ExitedAt, instance.ExitLog = "", &now, exitLog
	if instance.Restarts >= maxRestarts {
		instance.Stopped = true
	} else {
		instance.Restarts++
	}
	if s.exitLogs == nil {
		s.exitLogs = map[string]string{}
	}
	s.exitLogs[agent] = exitLog
	return true, nil
}

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

func observed(instances ...ObservedInstance) Observed {
	if instances == nil {
		instances = []ObservedInstance{}
	}
	return Observed{LaunchDigest: testDigest, Instances: instances}
}

func reconcile(t *testing.T, store Store, caller auth.Principal, launcher string, logger *zap.Logger) (int, []byte) {
	t.Helper()
	return reconcileWith(t, store, caller, launcher, logger, observed())
}

func reconcileWith(t *testing.T, store Store, caller auth.Principal, launcher string, logger *zap.Logger, body any) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterHuma(doc, NewHandler(store, testImage, testAPIURL, testSecret, testBootLimit, logger))
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
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{
		"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: named("pe-1", "ag-cut", "pe1-agcut-aaaaaaaaaaaa"), Ending: true},
			{AgentSignup: named("pe-2", "ag-closed", "pe2-agclosed-bbbbbbbbbbbb"), Ending: true},
			{AgentSignup: hosted("pe-2", "ag-closed-before-it-ever-ran"), Ending: true},
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
		"AGENT_MODEL":            "claude-sonnet-5",
		"HL_TOKEN":               token,
	}
	if running.Image != testImage || !reflect.DeepEqual(running.Env, wantEnv) {
		t.Errorf("instance = %+v\nwant env %v", running, wantEnv)
	}
	if !strings.HasPrefix(running.Name, "pe1-agrunning-") || !strings.HasPrefix(desired.Instances[1].Name, "pe3-agopen-") {
		t.Errorf("names = %q, %q", running.Name, desired.Instances[1].Name)
	}
	if !reflect.DeepEqual(desired.Ending, []string{"pe1-agcut-aaaaaaaaaaaa", "pe2-agclosed-bbbbbbbbbbbb"}) {
		t.Errorf("ending = %v, want the two names the instances were created under", desired.Ending)
	}
	if got := store.find("pe-1", "ag-running").Name; got != running.Name {
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

func TestEveryInstanceIsToldItsFlavorAndModelAndASignupWithoutThemIsLeftOutAndLogged(t *testing.T) {
	agent := hosted("pe-1", "ag-1")
	agent.Hyperparameters = nil
	instance, err := instanceFor(agent, agent.Instance.Restarts, testImage, testAPIURL, testSecret, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Env["AGENT_FLAVOR"] != "data-search" || instance.Env["AGENT_MODEL"] != "claude-sonnet-5" || instance.Env["AGENT_HYPERPARAMETERS"] != "{}" {
		t.Errorf("env = %v", instance.Env)
	}

	noFlavor, noModel := hosted("pe-1", "ag-no-flavor"), hosted("pe-1", "ag-no-model")
	noFlavor.Flavor, noModel.Model = "", ""
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: noFlavor}, {AgentSignup: hosted("pe-1", "ag-whole")}, {AgentSignup: noModel},
	}}}
	code, raw := reconcileWith(t, store, mainLauncher, "main", zap.New(core), observed())
	if code != http.StatusOK {
		t.Fatalf("one bad row failed the whole list: %d %s", code, raw)
	}
	desired := decode(t, raw)
	if len(desired.Instances) != 1 || desired.Instances[0].Env["AGENT_ID"] != "ag-whole" {
		t.Fatalf("instances = %+v, want the one whole agent", desired.Instances)
	}
	if logs.Len() != 2 {
		t.Fatalf("logged %d entries, want one for each agent left out", logs.Len())
	}
}

func TestAnExitedInstanceIsRecordedAndItsAgentAskedForUnderANewNameUntilItsRestartsAreSpent(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {
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
		recorded := store.find("pe-1", "ag-crashing")
		if recorded.Restarts != exit || recorded.Stopped || recorded.Name != next || recorded.ExitLog != fmt.Sprintf("exit %d", exit) {
			t.Fatalf("exit %d: recorded %+v", exit, recorded)
		}
		// The launcher could not destroy the old machine and reports it again: nothing is counted.
		again := mustReconcile(t, store, observed(ObservedInstance{Name: crashing, State: "exited", Log: "said again"}))
		if again.Instances[0].Name != next || store.find("pe-1", "ag-crashing").Restarts != exit {
			t.Fatalf("exit %d reported twice was counted twice", exit)
		}
		crashing = next
	}

	last := mustReconcile(t, store, observed(ObservedInstance{Name: crashing, State: "exited", Log: "the last words"}))
	if len(last.Instances) != 1 || last.Instances[0].Name != steady || len(last.Ending) != 0 {
		t.Fatalf("after the last restart: %+v, want the agent in neither list", last)
	}
	recorded := store.find("pe-1", "ag-crashing")
	if !recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts || recorded.ExitLog != "the last words" || recorded.ExitedAt == nil {
		t.Fatalf("recorded %+v, want stopped with its last log", recorded)
	}
	if after := mustReconcile(t, store, observed()); len(after.Instances) != 1 {
		t.Fatalf("a stopped agent is asked for again: %+v", after.Instances)
	}
}

func TestOnlyAnExitOfTheMachineAnAgentIsAskedForUnderIsRecorded(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: hosted("pe-1", "ag-1")},
		{AgentSignup: named("pe-1", "ag-ending", "pe1-agending-aaaaaaaaaaaa"), Ending: true},
	}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	for what, instance := range map[string]ObservedInstance{
		"still starting":       {Name: name, State: "starting"},
		"running":              {Name: name, State: "running", Log: "ignored"},
		"another name exited":  {Name: "pe1-ag1-000000000000", State: "exited"},
		"an ending one exited": {Name: "pe1-agending-aaaaaaaaaaaa", State: "exited", Log: "finished its last turn"},
	} {
		answer := mustReconcile(t, store, observed(instance))
		if len(answer.Instances) != 1 || answer.Instances[0].Name != name || !reflect.DeepEqual(answer.Ending, []string{"pe1-agending-aaaaaaaaaaaa"}) {
			t.Errorf("%s: answer = %+v", what, answer)
		}
		if got := store.find("pe-1", "ag-1"); got.Restarts != 0 || got.ExitedAt != nil {
			t.Errorf("%s: recorded as an exit: %+v", what, got)
		}
		if got := store.find("pe-1", "ag-ending"); got.Restarts != 0 || got.ExitedAt != nil {
			t.Errorf("%s: the ending agent's normal exit was recorded as a crash: %+v", what, got)
		}
	}
}

func TestTheLogOfAnExitIsKeptToItsLastBytes(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	long := strings.Repeat("an early line that is dropped\n", 400) + "Traceback: the reason it exited"
	mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "exited", Log: long}))
	kept := store.exitLogs["ag-1"]
	if len(kept) > domain.MaxAgentInstanceLogBytes || !strings.HasSuffix(kept, "Traceback: the reason it exited") || !strings.HasPrefix(kept, "an early line") {
		t.Fatalf("kept %d bytes ending %q", len(kept), kept[max(0, len(kept)-40):])
	}
}

func TestAChangeOfConfigOrOfTheLaunchersInputsRenamesRunningInstancesAndLeavesEndingOnesAsCreated(t *testing.T) {
	agents := func() map[string][]db.LauncherAgent {
		return map[string][]db.LauncherAgent{"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: named("pe-1", "ag-cut", "pe1-agcut-createdunder1"), Ending: true},
		}}
	}
	store := &launcherStore{byLauncher: agents()}
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
	if got := store.find("pe-1", "ag-running").Name; got != after.Instances[0].Name {
		t.Errorf("recorded name %q, want the new one %q", got, after.Instances[0].Name)
	}

	// The same agents under another image: the handler is built anew, as after a config change.
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(mainLauncher), auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterHuma(doc, NewHandler(store, "registry.example.invalid/agent:2", "https://moved.example.invalid", testSecret, testBootLimit, zap.NewNop()))
	raw, _ := json.Marshal(rotated)
	req := httptest.NewRequest(http.MethodPost, "/main/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	moved := decode(t, rec.Body.Bytes())
	if moved.Instances[0].Name == after.Instances[0].Name || moved.Instances[0].Image != "registry.example.invalid/agent:2" {
		t.Errorf("after an image change: %+v", moved.Instances[0])
	}
	if !reflect.DeepEqual(moved.Ending, []string{"pe1-agcut-createdunder1"}) {
		t.Errorf("after an image change the ending instance was renamed: %v", moved.Ending)
	}
}

func TestAnAgentWhoseWriteFailsIsLeftOutAndTheRestOfTheListStands(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	store := &launcherStore{
		byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-broken")}, {AgentSignup: hosted("pe-1", "ag-fine")}}},
		writeErr:   map[string]error{"ag-broken": errors.New("row lock timeout")},
	}
	code, raw := reconcileWith(t, store, mainLauncher, "main", zap.New(core), observed())
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if desired := decode(t, raw); len(desired.Instances) != 1 || desired.Instances[0].Env["AGENT_ID"] != "ag-fine" {
		t.Fatalf("instances = %+v", desired.Instances)
	}
	if logs.Len() != 1 {
		t.Fatalf("logged %d entries, want 1", logs.Len())
	}
}

func TestARequestThatDoesNotSayWhatWasObservedIsRefused(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
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
		store.asked = nil
		code, raw := reconcileWith(t, store, mainLauncher, "main", zap.NewNop(), body)
		if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d: %s", what, code, raw)
		}
		if len(store.asked) != 0 || strings.Contains(string(raw), "hls_") {
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
	RegisterHuma(doc, NewHandler(store, testImage, testAPIURL, testSecret, testBootLimit, zap.NewNop()))
	rec := httptest.NewRecorder()
	raw, _ := json.Marshal(observed())
	req := httptest.NewRequest(http.MethodPost, "/main/reconcile", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
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

func TestAHandlerCannotBeBuiltWithoutWhatEveryInstanceNeeds(t *testing.T) {
	for name, build := range map[string]func(){
		"no store":  func() { NewHandler(nil, testImage, testAPIURL, testSecret, testBootLimit, zap.NewNop()) },
		"no image":  func() { NewHandler(&launcherStore{}, "", testAPIURL, testSecret, testBootLimit, zap.NewNop()) },
		"no url":    func() { NewHandler(&launcherStore{}, testImage, "", testSecret, testBootLimit, zap.NewNop()) },
		"no secret": func() { NewHandler(&launcherStore{}, testImage, testAPIURL, "", testBootLimit, zap.NewNop()) },
		"no logger": func() { NewHandler(&launcherStore{}, testImage, testAPIURL, testSecret, testBootLimit, nil) },
		"no limit":  func() { NewHandler(&launcherStore{}, testImage, testAPIURL, testSecret, 0, zap.NewNop()) },
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

func TestAMachineReportedAsStartingPastTheBootLimitIsGivenUpOnLikeAnExit(t *testing.T) {
	store := &launcherStore{
		byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-slow")}, {AgentSignup: hosted("pe-1", "ag-absent")}}},
		askedAgo:   map[string]time.Duration{},
	}
	first := mustReconcile(t, store, observed())
	slow, absent := first.Instances[0].Name, first.Instances[1].Name

	// Inside the limit, and for a machine the provider does not list at all, nothing is decided:
	// a create that keeps failing is no boot.
	store.askedAgo = map[string]time.Duration{"ag-slow": testBootLimit - time.Second, "ag-absent": 24 * time.Hour}
	answer := mustReconcile(t, store, observed(ObservedInstance{Name: slow, State: "starting"}))
	if answer.Instances[0].Name != slow || answer.Instances[1].Name != absent {
		t.Fatalf("answer = %+v, want both names unchanged", answer.Instances)
	}

	store.askedAgo["ag-slow"] = testBootLimit + time.Second
	answer = mustReconcile(t, store, observed(ObservedInstance{Name: slow, State: "starting"}))
	if answer.Instances[0].Name == slow || answer.Instances[1].Name != absent {
		t.Fatalf("answer = %+v, want the slow agent under a new name", answer.Instances)
	}
	recorded := store.find("pe-1", "ag-slow")
	if recorded.Restarts != 1 || recorded.Stopped || recorded.ExitedAt == nil || recorded.Name != answer.Instances[0].Name {
		t.Fatalf("recorded %+v", recorded)
	}
	// The replacement's time starts when it is asked for.
	again := mustReconcile(t, store, observed(ObservedInstance{Name: answer.Instances[0].Name, State: "starting"}))
	if again.Instances[0].Name != answer.Instances[0].Name {
		t.Fatal("the replacement was given up on at once")
	}

	// Running, however long ago it was asked for, is no boot.
	store.askedAgo["ag-slow"] = 24 * time.Hour
	up := mustReconcile(t, store, observed(ObservedInstance{Name: again.Instances[0].Name, State: "running"}))
	if up.Instances[0].Name != again.Instances[0].Name || store.find("pe-1", "ag-slow").Restarts != 1 {
		t.Fatal("a running machine was held to the boot limit")
	}
}

func hostedCoordinator(pe string) db.LauncherCoordinator {
	return db.LauncherCoordinator{PlatformExperimentID: pe, HostedCoordinator: domain.HostedCoordinator{Launcher: "main", Model: "claude-sonnet-5"}}
}

func TestAHostedCoordinatorIsOneMoreInstanceWithACoordinatorTokenAndNoAgentID(t *testing.T) {
	store := &launcherStore{
		byLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1"), {PlatformExperimentID: "pe-2", HostedCoordinator: domain.HostedCoordinator{Launcher: "other", Model: "m"}}},
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
	want := map[string]string{
		"API_URL": testAPIURL, "PLATFORM_EXPERIMENT_ID": "pe-1", "AGENT_FLAVOR": "coordinator", "AGENT_MODEL": "claude-sonnet-5", "HL_TOKEN": token,
	}
	if !maps.Equal(coordinator.Env, want) || coordinator.Image != testImage {
		t.Errorf("coordinator = %+v, want env %v", coordinator, want)
	}
	if !strings.HasPrefix(coordinator.Name, "pe1-coordinator-") || len(coordinator.Name) > MaxInstanceNameLength || strings.Contains(coordinator.Name, token) {
		t.Errorf("name = %q", coordinator.Name)
	}
	if store.coordinator("pe-1").Instance.Name != coordinator.Name {
		t.Error("the name asked for was not recorded")
	}
	if again := mustReconcile(t, store, observed(ObservedInstance{Name: coordinator.Name, State: "running"})); again.Instances[1].Name != coordinator.Name {
		t.Errorf("a second ask renamed the coordinator: %q then %q", coordinator.Name, again.Instances[1].Name)
	}
}

func TestAnAgentWhoseIDIsTheCoordinatorsNameGetsAnotherInstanceName(t *testing.T) {
	store := &launcherStore{
		byLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "coordinator")}}},
		coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")},
	}
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 2 || answer.Instances[0].Name == answer.Instances[1].Name {
		t.Fatalf("instances = %+v, want two names", answer.Instances)
	}
}

func TestACoordinatorsMachineIsRestartedBoundedTimedAndLeftToEndLikeAnAgents(t *testing.T) {
	store := &launcherStore{coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")}, askedAgo: map[string]time.Duration{}}
	name := mustReconcile(t, store, observed()).Instances[0].Name

	store.askedAgo[coordinatorKey+"pe-1"] = testBootLimit + time.Second
	stuck := mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "starting"}))
	if len(stuck.Instances) != 1 || stuck.Instances[0].Name == name || store.coordinator("pe-1").Instance.Restarts != 1 {
		t.Fatalf("after a boot past the limit: %+v, recorded %+v", stuck.Instances, store.coordinator("pe-1").Instance)
	}
	name = stuck.Instances[0].Name

	for restarts := 2; restarts <= domain.MaxAgentInstanceRestarts; restarts++ {
		answer := mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "exited", Log: "boom"}))
		if len(answer.Instances) != 1 || answer.Instances[0].Name == name {
			t.Fatalf("exit %d: %+v, want a new name", restarts, answer.Instances)
		}
		name = answer.Instances[0].Name
	}
	last := mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "exited", Log: "boom"}))
	recorded := store.coordinator("pe-1").Instance
	if len(last.Instances) != 0 || !recorded.Stopped || recorded.ExitLog != "boom" {
		t.Fatalf("after the last restart: %+v, recorded %+v", last.Instances, recorded)
	}

	ending := hostedCoordinator("pe-9")
	ending.Ending, ending.Instance.Name = true, "pe9-coordinator-0123456789ab"
	store = &launcherStore{coordinators: []db.LauncherCoordinator{ending}}
	answer := mustReconcile(t, store, observed(ObservedInstance{Name: ending.Instance.Name, State: "exited"}))
	if len(answer.Instances) != 0 || !slices.Equal(answer.Ending, []string{ending.Instance.Name}) {
		t.Fatalf("answer = %+v, want the coordinator only as ending", answer)
	}
}

func TestAMachineThatCouldNotBeCreatedIsGivenTheBootLimitAndThenSpendsARestart(t *testing.T) {
	store := &launcherStore{
		byLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")},
		askedAgo:     map[string]time.Duration{},
	}
	first := mustReconcile(t, store, observed())
	agentName, coordinatorName := first.Instances[0].Name, first.Instances[1].Name
	failed := func(names ...string) Observed {
		body := observed()
		for _, name := range names {
			body.CreateFailures = append(body.CreateFailures, CreateFailure{Name: name, Reason: "vendor: HTTP 422: bad guest"})
		}
		return body
	}

	// Inside the limit a failed create is retried under the same name and costs nothing.
	store.askedAgo["ag-1"] = testBootLimit - time.Second
	if answer := mustReconcile(t, store, failed(agentName)); answer.Instances[0].Name != agentName || store.find("pe-1", "ag-1").Restarts != 0 {
		t.Fatalf("inside the limit: %+v, recorded %+v", answer.Instances, store.find("pe-1", "ag-1"))
	}

	store.askedAgo["ag-1"] = testBootLimit + time.Second
	store.askedAgo[coordinatorKey+"pe-1"] = testBootLimit + time.Second
	answer := mustReconcile(t, store, failed(agentName, coordinatorName, "a-name-nobody-is-asked-for-under"))
	if len(answer.Instances) != 2 || answer.Instances[0].Name == agentName || answer.Instances[1].Name == coordinatorName {
		t.Fatalf("past the limit: %+v, want both under new names", answer.Instances)
	}
	recorded := store.find("pe-1", "ag-1")
	if recorded.Restarts != 1 || recorded.Stopped || !strings.Contains(recorded.ExitLog, "no machine could be created") || !strings.Contains(recorded.ExitLog, "bad guest") {
		t.Fatalf("recorded %+v, want a restart spent and the provider's words kept", recorded)
	}
	if got := store.coordinator("pe-1").Instance; got.Restarts != 1 || !strings.Contains(got.ExitLog, "bad guest") {
		t.Fatalf("the coordinator's recorded %+v", got)
	}

	// Every restart ends the same way: the agent is stopped and no longer asked for.
	name := answer.Instances[0].Name
	for range domain.MaxAgentInstanceRestarts {
		store.askedAgo["ag-1"] = testBootLimit + time.Second
		next := mustReconcile(t, store, failed(name))
		if len(next.Instances) == 2 {
			name = next.Instances[0].Name
		}
	}
	if recorded := store.find("pe-1", "ag-1"); !recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts {
		t.Fatalf("after every restart: recorded %+v, want the agent stopped", recorded)
	}
}

func TestAListedMachineIsBelievedOverAnEarlierFailedCreateOfItsName(t *testing.T) {
	store := &launcherStore{byLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}, askedAgo: map[string]time.Duration{}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	store.askedAgo["ag-1"] = 24 * time.Hour
	body := observed(ObservedInstance{Name: name, State: "running"})
	body.CreateFailures = []CreateFailure{{Name: name, Reason: "an earlier attempt"}}
	if answer := mustReconcile(t, store, body); answer.Instances[0].Name != name || store.find("pe-1", "ag-1").Restarts != 0 {
		t.Fatalf("a running machine was given up on for a create that failed before it: %+v", store.find("pe-1", "ag-1"))
	}
}

func TestACreateFailureWithoutAUsableNameIsRefused(t *testing.T) {
	for _, name := range []string{"", strings.Repeat("n", MaxInstanceNameLength+1)} {
		body := observed()
		body.CreateFailures = []CreateFailure{{Name: name, Reason: "r"}}
		if code, raw := reconcileWith(t, &launcherStore{}, mainLauncher, "main", zap.NewNop(), body); code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d %s, want 400", name, code, raw)
		}
	}
}
