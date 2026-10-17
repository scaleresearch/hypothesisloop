package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/hosting/hostingtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

const (
	testSecret = "a-signing-secret-only-this-test-knows"
	testImage  = "registry.example.invalid/agent:1"
	testAPIURL = "https://api.example.invalid"
)

const testDigest = "0123456789abcdef0123456789abcdef"

type launcherStore = hostingtest.Store

const testBootLimit = 5 * time.Minute

var testSettings = Settings{Image: testImage, APIURL: testAPIURL, SigningSecret: testSecret, BootLimit: testBootLimit, ProgressLimit: 4 * testBootLimit, MaxNewPerAnswer: 50}

func hosted(pe, agent string) domain.AgentSignup {
	return domain.AgentSignup{
		PlatformExperimentID: pe, AgentID: agent, Hosted: true,
		AgentType: "data-search", Model: "claude-sonnet-5", Hyperparameters: map[string]any{"batch_size": float64(64), "a": "b"},
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
	return withMachines(Observed{LaunchDigest: testDigest, Instances: instances})
}

// withMachines gives every instance reported with no machine id one of its own name: a test that
// does not care which machine a name is reported by.
func withMachines(o Observed) Observed {
	if o.Instances == nil {
		return o
	}
	instances := make([]ObservedInstance, len(o.Instances))
	copy(instances, o.Instances)
	for i := range instances {
		if instances[i].Machine == "" {
			instances[i].Machine = "m-" + instances[i].Name
		}
	}
	o.Instances = instances
	return o
}

// answer is a Desired as the tests read it, every list present.
type answer struct {
	Instances []answerInstance `json:"instances"`
	Ending    []string         `json:"ending"`
	Image     string           `json:"image,omitempty"`
}

type answerInstance struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
}

func reconcile(t *testing.T, store Store, logger *zap.Logger) (int, []byte) {
	t.Helper()
	return reconcileWith(t, store, logger, observed())
}

func reconcileWith(t *testing.T, store Store, logger *zap.Logger, body Observed) (int, []byte) {
	t.Helper()
	return decide(t, NewDecider(store, &hostingtest.Metrics{}, testSettings, logger), body)
}

// decide is one pass of the decider: 200 with the answer, or 500 with the error.
func decide(t *testing.T, d *Decider, body Observed) (int, []byte) {
	t.Helper()
	desired, err := d.Reconcile(context.Background(), withMachines(body))
	if err != nil {
		return http.StatusInternalServerError, []byte(err.Error())
	}
	out := answer{Instances: []answerInstance{}, Ending: desired.Ending, Image: desired.Image}
	for _, i := range desired.Instances {
		out.Instances = append(out.Instances, answerInstance(i))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return http.StatusOK, raw
}

// mustReconcile is one launcher pass as the platform sees it.
func mustReconcile(t *testing.T, store Store, body Observed) Desired {
	t.Helper()
	code, raw := reconcileWith(t, store, zap.NewNop(), body)
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
	var read answer
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	desired := Desired{Instances: []Instance{}, Ending: read.Ending, Image: read.Image}
	for _, i := range read.Instances {
		desired.Instances = append(desired.Instances, Instance(i))
	}
	return desired
}

func TestTheAnswerIsTheLaunchersRunningInstancesAndItsEndingOnes(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{
		"main": {
			{AgentSignup: hosted("pe-1", "ag-running")},
			{AgentSignup: ended(named("pe-1", "ag-cut", "pe1-agcut-aaaaaaaaaaaa"), domain.RunEndCut)},
			{AgentSignup: ended(named("pe-2", "ag-closed", "pe2-agclosed-bbbbbbbbbbbb"), domain.RunEndClosed)},
			{AgentSignup: ended(hosted("pe-2", "ag-closed-before-it-ever-ran"), domain.RunEndClosed)},
			{AgentSignup: hosted("pe-3", "ag-open")},
		},
	}}
	code, raw := reconcile(t, store, zap.NewNop())
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	if store.Listed != 1 || store.WindDown != domain.AgentWindDown || store.CoordinatorWindDown != domain.CoordinatorWindDown {
		t.Fatalf("store asked for %v with wind-down %s, coordinators %s", store.Listed, store.WindDown, store.CoordinatorWindDown)
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

func TestALauncherWithNothingToRunIsAnsweredWithTwoEmptyListsAndThePlatformsImage(t *testing.T) {
	code, raw := reconcile(t, &launcherStore{}, zap.NewNop())
	if code != http.StatusOK {
		t.Fatalf("got = %d %s", code, raw)
	}
	// decode refuses an answer in which either list is missing or null.
	if desired := decode(t, raw); len(desired.Instances) != 0 || len(desired.Ending) != 0 || desired.Image != testImage {
		t.Fatalf("got = %s, want two empty lists and image %s", raw, testImage)
	}
}

// The agent reads its type, model and hyperparameters from its instructions and applies an edit of
// them itself, so none of them replaces its machine, and a signup naming no model of its own runs
// on its type's like any other.
func TestAnAgentWhoseTypeModelOrHyperparametersChangeKeepsItsMachine(t *testing.T) {
	agent := hosted("pe-1", "ag-1")
	instance, err := instanceFor(agent, agent.Instance.Restarts, testImage, testAPIURL, testSecret, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	for what, edit := range map[string]func(*domain.AgentSignup){
		"flavor":          func(a *domain.AgentSignup) { a.AgentType = "data-search" },
		"model":           func(a *domain.AgentSignup) { a.Model = "another-model" },
		"no model":        func(a *domain.AgentSignup) { a.Model = "" },
		"hyperparameters": func(a *domain.AgentSignup) { a.Hyperparameters = map[string]any{"batch_size": float64(32)} },
	} {
		edited := agent
		edit(&edited)
		changed, err := instanceFor(edited, edited.Instance.Restarts, testImage, testAPIURL, testSecret, testDigest)
		if err != nil || changed.Name != instance.Name || !maps.Equal(changed.Env, instance.Env) {
			t.Errorf("after a change of %s: got = %+v, %v, want the machine %s as it was", what, changed, err, instance.Name)
		}
	}
	if _, err := coordinatorInstanceFor("pe-1", 0, testImage, testAPIURL, testSecret, testDigest); err != nil {
		t.Fatal(err)
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

// An agent of a long run that exits now and then is restarted whenever its exits are spread
// wider than the restart window, however many there are in all.
func TestExitsSpreadOverTheRestartWindowNeverStopTheAgent(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	name := mustReconcile(t, store, observed()).Instances[0].Name
	spacing := domain.AgentInstanceRestartWindow / time.Duration(domain.MaxAgentInstanceRestarts)
	for exit := 1; exit <= 3*domain.MaxAgentInstanceRestarts; exit++ {
		store.Advance(spacing)
		answer := mustReconcile(t, store, observed(ObservedInstance{Name: name, State: "exited", Log: "boom"}))
		if recorded := store.Find("pe-1", "ag-1"); recorded.Stopped || recorded.Restarts != exit || len(answer.Instances) != 1 {
			t.Fatalf("exit %d, one every %s: recorded %+v, want it restarted", exit, spacing, recorded)
		}
		name = answer.Instances[0].Name
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

	// The same agents under another image: the decider is built anew, as after a config change.
	moved := testSettings
	moved.Image, moved.APIURL = "registry.example.invalid/agent:2", "https://moved.example.invalid"
	_, raw := reconcileUnder(t, store, moved, rotated)
	answer := decode(t, raw)
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
	code, raw := reconcileWith(t, store, zap.New(core), observed())
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

// Only a decision written down changes what a launcher keeps: a pass whose write fails answers
// the machine already asked for, which the launcher would otherwise destroy.
func TestAMachineWhoseWriteFailsIsKeptAsItWas(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	first := mustReconcile(t, store, observed())
	if len(first.Instances) != 1 {
		t.Fatalf("instances = %+v", first.Instances)
	}
	running := first.Instances[0]
	store.WriteErr = map[string]error{"ag-1": errors.New("statement timeout")}

	exited := mustReconcile(t, store, observed(ObservedInstance{Name: running.Name, State: "exited", Log: "boom"}))
	if !reflect.DeepEqual(exited.Instances, []Instance{running}) {
		t.Fatalf("after a failed write of its end: instances = %+v, want %+v unchanged", exited.Instances, running)
	}

	rotated := observed(ObservedInstance{Name: running.Name, State: "running"})
	rotated.LaunchDigest = "fedcba9876543210fedcba9876543210"
	renamed := mustReconcile(t, store, rotated)
	if len(renamed.Instances) != 1 || renamed.Instances[0].Name != running.Name {
		t.Fatalf("after a failed write of its new name: instances = %+v, want %q kept", renamed.Instances, running.Name)
	}
	if got := store.Find("pe-1", "ag-1"); got.Name != running.Name || got.Restarts != 0 {
		t.Fatalf("recorded = %+v, want the machine as it was", got)
	}
}

// A machine renamed by a config change runs until its replacement's turn comes: the bound on new
// machines per answer holds back the new one and never ends the old.
func TestARenamedMachineWaitingItsTurnIsKept(t *testing.T) {
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {
		{AgentSignup: hosted("pe-1", "ag-1")}, {AgentSignup: hosted("pe-1", "ag-2")},
	}}}
	first := mustReconcile(t, store, observed())
	var listed []ObservedInstance
	for _, instance := range first.Instances {
		listed = append(listed, ObservedInstance{Name: instance.Name, State: "running"})
	}
	cfg := testSettings
	cfg.MaxNewPerAnswer = 1
	rotated := observed(listed...)
	rotated.LaunchDigest = "fedcba9876543210fedcba9876543210"
	code, raw := reconcileUnder(t, store, cfg, rotated)
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, raw)
	}
	answer := decode(t, raw)
	names := map[string]bool{}
	for _, instance := range answer.Instances {
		names[instance.Name] = true
	}
	if len(answer.Instances) != 2 || names[first.Instances[0].Name] || !names[first.Instances[1].Name] {
		t.Fatalf("instances = %+v, want the first renamed and the second kept under %q", answer.Instances, first.Instances[1].Name)
	}
}

func TestNoTokenIsLogged(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}}}
	if code, raw := reconcile(t, store, zap.New(core)); code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	store.Err = errors.New("down")
	reconcile(t, store, zap.New(core))
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
	return db.LauncherCoordinator{PlatformExperimentID: pe, HostedCoordinator: domain.HostedCoordinator{Model: "claude-sonnet-5"}}
}

func TestAHostedCoordinatorIsOneMoreInstanceWithACoordinatorTokenAndNoAgentID(t *testing.T) {
	store := &launcherStore{
		ByLauncher:   map[string][]db.LauncherAgent{"main": {{AgentSignup: hosted("pe-1", "ag-1")}}},
		Coordinators: []db.LauncherCoordinator{hostedCoordinator("pe-1")},
	}
	answer := mustReconcile(t, store, observed())
	if len(answer.Instances) != 2 {
		t.Fatalf("instances = %+v, want the agent and the coordinator", answer.Instances)
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
	return reconcileRecording(t, store, &hostingtest.Metrics{}, cfg, body)
}

func reconcileRecording(t *testing.T, store Store, metrics Metrics, cfg Settings, body Observed) (int, []byte) {
	t.Helper()
	return decide(t, NewDecider(store, metrics, cfg, zap.NewNop()), body)
}

// What a hosted agent's harness says of itself lived only in its machine's log. The launcher
// reports it with the progress, and it goes to the run's health series under the agent's role,
// the coordinator's under its own; a line that cannot be read is passed over.
func TestARunningMachinesHarnessStateGoesToTheRunsHealth(t *testing.T) {
	store := oneAgentAndACoordinator()
	store.ByLauncher["main"][0].Role = domain.SignupRoleCompetitor
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	metrics := &hostingtest.Metrics{}
	body := observed(
		ObservedInstance{Name: agent, State: "running", Progress: marker("1"), HarnessState: "subagents=3 turn=working since=1759990000 instructions_digest=0123456789ab version=4 model_status=rejected model_type=seven_day model_utilization=1.0 model_resets_at=1760000000"},
		ObservedInstance{Name: coordinator, State: "running", Progress: marker("1"), HarnessState: "subagents=0 turn=waiting since=1759999000"})
	if code, raw := reconcileRecording(t, store, metrics, testSettings, body); code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, raw)
	}
	full, resets := 1.0, time.Unix(1760000000, 0).UTC()
	want := []domain.HarnessState{
		{PlatformExperimentID: "pe-1", AgentID: "ag-1", Role: "competitor", Subagents: 3, TurnSince: time.Unix(1759990000, 0).UTC(), InstructionsDigest: "0123456789ab",
			AgentType: "data-search", AgentTypeVersion: 4, Model: domain.RateLimit{Rejected: true, Utilization: &full, ResetsAt: &resets}},
		{PlatformExperimentID: "pe-1", Role: domain.CoordinatorHealthRole, AgentType: domain.CoordinatorFlavor, TurnWaiting: true, TurnSince: time.Unix(1759999000, 0).UTC()},
	}
	if got := metrics.Written(); !reflect.DeepEqual(got, want) {
		t.Fatalf("written %+v, want %+v", got, want)
	}

	metrics = &hostingtest.Metrics{}
	body.Instances[0].HarnessState, body.Instances[1].HarnessState = "subagents=1 turn=working since=1759990000 model_status=exhausted", "subagents=0 turn=idle since=1759999000"
	if code, raw := reconcileRecording(t, store, metrics, testSettings, body); code != http.StatusOK || len(metrics.Written()) != 0 {
		t.Fatalf("lines that cannot be read: status = %d, written %+v: %s", code, metrics.Written(), raw)
	}
}

func TestParseHarnessStateRefusesWhatTheHarnessDoesNotPrint(t *testing.T) {
	for _, line := range []string{
		"",
		"turn=working since=1759990000",
		"subagents=1 since=1759990000",
		"subagents=1 turn=working",
		"subagents=-1 turn=working since=1759990000",
		"subagents=1 turn=busy since=1759990000",
		"subagents=1 turn=working since=0",
		"subagents=1 turn=working since=1759990000 subagents=2",
		"subagents=1 turn=working since=1759990000 model_utilization=0.5",
		"subagents=1 turn=working since=1759990000 model_status=allowed model_utilization=1.5",
		"subagents=1 turn=working since=1759990000 model_status=allowed model_resets_at=soon",
		"subagents=1 turn=working since=1759990000 status=allowed",
		"subagents=1 turn=working since=1759990000 stray",
		"subagents=1 turn=working since=1759990000 instructions_digest=0123456789",
		"subagents=1 turn=working since=1759990000 instructions_digest=0123456789AB",
		"subagents=1 turn=working since=1759990000 instructions_digest=0123456789ab instructions_digest=0123456789ab",
		"subagents=1 turn=working since=1759990000 instructions_digest=0123456789ab version=0",
		"subagents=1 turn=working since=1759990000 instructions_digest=0123456789ab version=v2",
	} {
		if state, _, err := parseHarnessState(line); err == nil {
			t.Errorf("%q: read %+v, want an error", line, state)
		}
	}
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

// Held is the machines the platform holds, as the launcher reads them.
type Held struct{ Machines []HeldMachine }

func held(t *testing.T, store Store) (int, Held) {
	t.Helper()
	machines, err := NewDecider(store, &hostingtest.Metrics{}, testSettings, zap.NewNop()).Held(context.Background())
	if err != nil {
		return http.StatusInternalServerError, Held{}
	}
	return http.StatusOK, Held{Machines: machines}
}

func TestALauncherIsToldTheMachineEachNameItHoldsWasReportedByUntilItIsGivenUpOn(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	agent, coordinator := first.Instances[0].Name, first.Instances[1].Name
	if code, got := held(t, store); code != http.StatusOK || len(got.Machines) != 0 {
		t.Fatalf("before any report: %d %+v, want none held", code, got)
	}
	mustReconcile(t, store, observed(ObservedInstance{Name: agent, Machine: "m-agent", State: "running"}, ObservedInstance{Name: coordinator, Machine: "m-coordinator", State: "running"}))
	want := []HeldMachine{{Name: agent, Machine: "m-agent"}, {Name: coordinator, Machine: "m-coordinator"}}
	sort.Slice(want, func(a, b int) bool { return want[a].Name < want[b].Name })
	if code, got := held(t, store); code != http.StatusOK || !reflect.DeepEqual(got.Machines, want) {
		t.Fatalf("held = %d %+v, want %+v", code, got.Machines, want)
	}
	mustReconcile(t, store, observed(ObservedInstance{Name: coordinator, Machine: "m-coordinator", State: "running"}))
	if code, got := held(t, store); code != http.StatusOK || !reflect.DeepEqual(got.Machines, []HeldMachine{{Name: coordinator, Machine: "m-coordinator"}}) {
		t.Fatalf("after the agent's machine was given up on: %d %+v, want the coordinator's alone", code, got.Machines)
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

// A run closed and reopened is wanted again by its rows alone: once its close's wind-down passed
// and its machines were released, an agent and a coordinator of a running run with no machine are
// asked for one, with no restart spent and nothing read as lost.
func TestAnAgentAndACoordinatorWhoseMachinesWereReleasedAreAskedForAgainOnceTheirRunGoesOn(t *testing.T) {
	store := oneAgentAndACoordinator()
	first := mustReconcile(t, store, observed())
	var running []ObservedInstance
	for _, instance := range first.Instances {
		running = append(running, ObservedInstance{Name: instance.Name, State: "running"})
	}
	mustReconcile(t, store, observed(running...))

	store.ByLauncher["main"][0].Ended, store.Coordinators[0].Ending = domain.RunEndClosed, true
	if closing := mustReconcile(t, store, observed(running...)); len(closing.Instances) != 0 || len(closing.Ending) != 2 {
		t.Fatalf("closed: answer = %+v, want both machines ending", closing)
	}
	store.Over = map[string]domain.RunEnd{"pe-1/ag-1": domain.RunEndClosed}
	mustReconcile(t, store, observed())
	store.Coordinators[0].Instance.Name, store.Coordinators[0].Instance.EndReason = "", string(db.InstanceReleased)

	store.Over, store.ByLauncher["main"][0].Ended, store.Coordinators[0].Ending = nil, "", false
	again := mustReconcile(t, store, observed())
	if len(again.Instances) != 2 || len(again.Ending) != 0 {
		t.Fatalf("reopened: answer = %+v, want a machine for the agent and one for the coordinator", again)
	}
	for _, agent := range []string{"ag-1", ""} {
		if recorded := store.Find("pe-1", agent); recorded.Name == "" || recorded.Restarts != 0 || recorded.Stopped || strings.Contains(recorded.ExitLog, "lost") {
			t.Fatalf("%q: recorded %+v, want a machine asked for with no restart spent", agent, recorded)
		}
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
		if recorded := store.Find("pe-1", agent); recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts || recorded.Launch == launch || recorded.Name == "" {
			t.Fatalf("%q: recorded %+v, want a fresh budget under the new launch and the restarts counted still", agent, recorded)
		}
	}
	// The budget is whole again, and no more than whole.
	for exit := 1; exit <= domain.MaxAgentInstanceRestarts+1; exit++ {
		body := rotated
		body.Instances = []ObservedInstance{{Name: answer.Instances[0].Name, State: "exited"}, {Name: answer.Instances[len(answer.Instances)-1].Name, State: "running"}}
		answer = mustReconcile(t, store, body)
	}
	if recorded := store.Find("pe-1", "ag-1"); !recorded.Stopped || recorded.Restarts != 2*domain.MaxAgentInstanceRestarts {
		t.Fatalf("after the new budget was spent: %+v, want every restart of both launches counted", recorded)
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
