package agentlaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

const (
	launcherToken = "hls_launcher-token"
	oauthToken    = "oauth-secret-value"
	agentToken    = "hls_agent-token"
)

// controlPlane answers the reconcile call with whatever the test last set.
type controlPlane struct {
	mu     sync.Mutex
	status int
	body   string
	calls  []*http.Request
	// asked holds the body of every call, as the launcher sent it.
	asked  []reconcileQuestion
	server *httptest.Server
}

func (cp *controlPlane) lastQuestion() reconcileQuestion {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.asked[len(cp.asked)-1]
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{status: http.StatusOK}
	cp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.calls = append(cp.calls, r)
		var question reconcileQuestion
		if err := json.NewDecoder(r.Body).Decode(&question); err != nil {
			t.Errorf("the reconcile call carries no readable question: %v", err)
		}
		cp.asked = append(cp.asked, question)
		w.WriteHeader(cp.status)
		_, _ = w.Write([]byte(cp.body))
	}))
	t.Cleanup(cp.server.Close)
	cp.want(nil, nil)
	return cp
}

type entry struct {
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Env    map[string]string `json:"env"`
	EndsAt time.Time         `json:"ends_at"`
}

// testEndsAt is the moment every instance of these tests must be gone by.
var testEndsAt = time.Now().Add(36 * time.Hour).UTC().Truncate(time.Second)

func agent(name string) entry {
	return entry{Name: name, Image: "registry.example.invalid/agent:1", Env: map[string]string{"AGENT_ID": name, "HL_TOKEN": agentToken}, EndsAt: testEndsAt}
}

func (cp *controlPlane) want(instances []entry, ending []string) {
	cp.answer(http.StatusOK, answerOf(instances, ending, ""))
}

// refuse answers with the lists and a refusal of the launcher's configuration.
func (cp *controlPlane) refuse(instances []entry, ending []string, message string) {
	cp.answer(http.StatusOK, answerOf(instances, ending, message))
}

// idle answers that no agent is wanted and that the platform's image is image.
func (cp *controlPlane) idle(image string) {
	answer := map[string]any{"instances": []entry{}, "ending": []string{}, "image": image}
	raw, _ := json.Marshal(answer)
	cp.answer(http.StatusOK, string(raw))
}

func answerOf(instances []entry, ending []string, refusal string) string {
	if instances == nil {
		instances = []entry{}
	}
	if ending == nil {
		ending = []string{}
	}
	answer := map[string]any{"instances": instances, "ending": ending}
	if refusal != "" {
		answer["refusal"] = map[string]string{"reason": "pass_does_not_fit", "message": refusal}
	}
	raw, _ := json.Marshal(answer)
	return string(raw)
}

func (cp *controlPlane) answer(status int, body string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.status, cp.body = status, body
}

type harness struct {
	t        *testing.T
	cp       *controlPlane
	rented   *fake.Provider
	launcher *Launcher
	logMu    sync.Mutex
	log      []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, cp: newControlPlane(t), rented: fake.New()}
	launcher, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	h.launcher = launcher
	return h
}

func (h *harness) config() Config {
	return Config{
		Name:          "main",
		APIURL:        h.cp.server.URL,
		Token:         launcherToken,
		Command:       []string{"uv", "run", "agent"},
		Size:          Size{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		ModelProvider: "example",
		Env:           map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": oauthToken},
		Provider:      h.rented,
		HTTPClient:    h.cp.server.Client(),
		Interval:      time.Second,
		Log: func(format string, args ...any) {
			h.logMu.Lock()
			defer h.logMu.Unlock()
			h.log = append(h.log, fmt.Sprintf(format, args...))
		},
	}
}

func (h *harness) pass() error { return h.launcher.Pass(context.Background()) }

func (h *harness) mustPass() {
	h.t.Helper()
	if err := h.pass(); err != nil {
		h.t.Fatalf("pass: %v", err)
	}
}

func (h *harness) names(want ...string) {
	h.t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := h.rented.Names(); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("instances = %v, want %v", got, want)
	}
}

func full(name string) string { return "hl-agent-main-" + name }

func TestAMissingInstanceIsCreatedFromTheListAndTheLaunchersOwnConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.names(full("a"), full("b"))

	spec := h.rented.Spec(full("a"))
	if left := time.Until(testEndsAt); spec.Lifetime > left || spec.Lifetime < left-time.Minute || spec.Lifetime%time.Second != 0 {
		t.Fatalf("lifetime = %s, want the whole seconds left until ends_at, %s", spec.Lifetime, left)
	}
	spec.Lifetime = 0
	want := provider.Spec{
		Name:        full("a"),
		Image:       "registry.example.invalid/agent:1",
		Command:     []string{"uv", "run", "agent"},
		Env:         map[string]string{"AGENT_ID": "a", "HL_TOKEN": agentToken, "CLAUDE_CODE_OAUTH_TOKEN": oauthToken},
		CPUMillis:   2000,
		MemoryBytes: 4 << 30,
		DiskBytes:   20 << 30,
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec = %+v\nwant   %+v", spec, want)
	}
	if h.rented.Calls("List") != 1 {
		t.Errorf("List calls in one pass = %d, want 1", h.rented.Calls("List"))
	}
}

// The lifetime is what ends a machine when this launcher is gone or cut off. It is the time left
// until the moment the control plane names for that instance, and nothing of the launcher's own.
func TestEachInstanceIsRentedWithTheTimeLeftUntilItsOwnEnd(t *testing.T) {
	h := newHarness(t)
	short, long := agent("a"), agent("b")
	short.EndsAt, long.EndsAt = time.Now().Add(2*time.Hour), time.Now().Add(100*time.Hour)
	h.cp.want([]entry{short, long}, nil)
	h.mustPass()
	for name, want := range map[string]time.Duration{"a": 2 * time.Hour, "b": 100 * time.Hour} {
		if got := h.rented.Spec(full(name)).Lifetime; got > want || got < want-time.Minute || got%time.Second != 0 {
			t.Errorf("%s is rented with a lifetime of %s, want whole seconds just under %s", name, got, want)
		}
	}
}

// A machine with no time left would be rented with no bound at all, so it is not rented.
func TestAnInstanceWhoseEndHasPassedIsNotRentedAndTheRestOfThePassRuns(t *testing.T) {
	h := newHarness(t)
	late := agent("a")
	late.EndsAt = time.Now().Add(-time.Minute)
	h.cp.want([]entry{late, agent("b")}, nil)
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "create "+full("a")+": its ends_at") {
		t.Fatalf("pass = %v, want the create refused for its passed ends_at", err)
	}
	h.names(full("b"))
	if h.rented.Calls("Create") != 1 {
		t.Fatalf("creates = %d", h.rented.Calls("Create"))
	}
}

func TestTheReconcileCallCarriesTheLaunchersTokenAndName(t *testing.T) {
	h := newHarness(t)
	h.mustPass()
	call := h.cp.calls[0]
	if call.Method != http.MethodPost || call.URL.Path != "/internal/launchers/main/reconcile" {
		t.Errorf("call = %s %s", call.Method, call.URL.Path)
	}
	if got := call.Header.Get("Authorization"); got != "Bearer "+launcherToken {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAPassThatFindsEverythingInPlaceChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	before := h.rented.IDs(full("a"))
	for range 3 {
		h.mustPass()
	}
	if got := h.rented.IDs(full("a")); !reflect.DeepEqual(got, before) {
		t.Fatalf("ids = %v, want %v", got, before)
	}
	if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("creates %d destroys %d", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
}

func TestAnInstanceStillStartingIsNotRentedAgain(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.mustPass()
	h.mustPass()
	h.names(full("a"))
	if h.rented.Calls("Create") != 1 {
		t.Fatalf("creates = %d", h.rented.Calls("Create"))
	}
}

func TestAnInstanceTheListNoLongerNamesIsDestroyed(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.cp.want([]entry{agent("b")}, nil)
	h.mustPass()
	h.names(full("b"))
	h.cp.want(nil, nil)
	h.mustPass()
	h.names()
}

func TestAnEndingInstanceIsToldToStopOnceAndAgainOnlyWhileItKeepsRunning(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.launcher.now = func() time.Time { return now }
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Exit(full("b"))
	idsA, idsB, idsC := h.rented.IDs(full("a")), h.rented.IDs(full("b")), h.rented.IDs(full("c"))

	h.cp.want(nil, []string{"a", "b", "c", "never-rented"})
	h.mustPass()
	h.mustPass()
	// One whose command already exited has had its last turn and goes at once, needing no
	// signal. Run 9's coordinator was kept four minutes after it.
	h.names(full("a"), full("c"))
	if !reflect.DeepEqual(h.rented.IDs(full("a")), idsA) || !reflect.DeepEqual(h.rented.IDs(full("c")), idsC) || len(idsB) != 1 {
		t.Fatal("an ending instance was replaced")
	}
	if h.rented.Calls("Create") != 3 || h.rented.Calls("Destroy") != 1 {
		t.Fatalf("creates %d destroys %d, want 3 and the exited one", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
	// The signal is what starts the agent's last turn. Running or still starting, an ending
	// instance gets it once. Run 7's launcher sent it to one machine on every pass, 33 times in
	// a minute.
	if a, c := h.rented.Stops(full("a")), h.rented.Stops(full("c")); a != 1 || c != 1 || h.rented.Calls("Stop") != 2 {
		t.Fatalf("stops: running %d, starting %d, all %d, want 1, 1 and none to the exited one", a, c, h.rented.Calls("Stop"))
	}
	now = now.Add(stopRetry - time.Second)
	h.mustPass()
	if a, c := h.rented.Stops(full("a")), h.rented.Stops(full("c")); a != 1 || c != 1 {
		t.Fatalf("stops before the retry: running %d, starting %d, want 1 each", a, c)
	}
	// One still running after the retry interval is told again; one that exited is destroyed.
	h.rented.Exit(full("c"))
	now = now.Add(time.Second)
	h.mustPass()
	if a := h.rented.Stops(full("a")); a != 2 || h.rented.Calls("Stop") != 3 {
		t.Fatalf("stops after the retry: running %d, all %d, want 2 and none to the exited one", a, h.rented.Calls("Stop"))
	}
	h.names(full("a"))

	h.cp.want(nil, nil)
	h.mustPass()
	h.names()
}

func TestAnInstanceThatIsNotEndingIsNeverToldToStop(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.mustPass()
	h.mustPass()
	if h.rented.Calls("Stop") != 0 {
		t.Fatalf("stops = %d", h.rented.Calls("Stop"))
	}
}

func TestAStopThatFailsIsReportedAndTheRestOfThePassRuns(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Start(full("b"))
	h.cp.want([]entry{agent("new")}, []string{"a", "b"})
	h.rented.Fail = func(method, name string) error {
		if method == "Stop" && name == full("a") {
			return errors.New("vendor refused")
		}
		return nil
	}
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "stop "+full("a")) {
		t.Fatalf("err = %v", err)
	}
	h.names(full("a"), full("b"), full("new"))
	if h.rented.Stops(full("b")) != 1 {
		t.Fatal("the other ending instance was not told to stop")
	}
	// The failed stop is sent again on the next pass, and the one that was taken is not.
	h.rented.Fail = nil
	h.mustPass()
	if a, b := h.rented.Stops(full("a")), h.rented.Stops(full("b")); a != 1 || b != 1 {
		t.Fatalf("stops after a failed one: %d and %d, want the failed one sent again and the other left", a, b)
	}
}

func TestAnExitedInstanceIsReportedWithTheEndOfItsOutputAndNeverRentedAgainFromHere(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Start(full("b"))
	crashed := h.rented.IDs(full("a"))
	h.rented.Exit(full("a"), "starting", "Traceback (most recent call last):", "SystemExit: agent: the reason")

	// The control plane has not taken the exit in yet and still asks for the same name.
	for pass := 1; pass <= 3; pass++ {
		h.mustPass()
		question := h.cp.lastQuestion()
		want := []observedInstance{
			{Name: "a", State: "exited", Log: "starting\nTraceback (most recent call last):\nSystemExit: agent: the reason"},
			{Name: "b", State: "running", Progress: new(string)},
		}
		if !reflect.DeepEqual(question.Instances, want) {
			t.Fatalf("pass %d told the control plane %+v\nwant %+v", pass, question.Instances, want)
		}
	}
	if got := h.rented.IDs(full("a")); !reflect.DeepEqual(got, crashed) || h.rented.Calls("Create") != 2 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("instances of a %v (was %v), creates %d, destroys %d: an exited instance is the control plane's to decide", got, crashed, h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
	if h.logged("SystemExit") != 0 || h.logged("Traceback") != 0 {
		t.Fatalf("the instance's output reached the launcher's log: %q", h.log)
	}
	if h.logged(full("a")+" exited and is still asked for") != 3 {
		t.Fatalf("log = %q", h.log)
	}

	// Its answer to the exit is another name: the old machine goes and a new one is rented.
	h.cp.want([]entry{agent("a-again"), agent("b")}, nil)
	h.mustPass()
	h.names(full("a-again"), full("b"))

	// Or no name at all: the machine goes and nothing takes its place.
	h.rented.Start(full("a-again"))
	h.rented.Exit(full("a-again"), "the last words")
	h.cp.want([]entry{agent("b")}, nil)
	h.mustPass()
	h.names(full("b"))
	if h.rented.Calls("Create") != 3 {
		t.Fatalf("creates = %d", h.rented.Calls("Create"))
	}
}

func TestOnlyTheEndOfAnExitedInstancesOutputIsSent(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %03d %s", i, strings.Repeat("x", 200))
	}
	h.rented.Exit(full("a"), lines...)
	h.mustPass()
	sent := h.cp.lastQuestion().Instances[0].Log
	if len(sent) > exitLogBytes || !strings.HasSuffix(sent, lines[499]) || strings.Contains(sent, "line 400") {
		t.Fatalf("sent %d bytes ending %q", len(sent), sent[max(0, len(sent)-30):])
	}
}

func TestAnExitWhoseOutputCannotBeReadIsStillReported(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Exit(full("a"), "unreadable")
	h.rented.Fail = func(method, _ string) error {
		if method == "Logs" {
			return errors.New("vendor log API down")
		}
		return nil
	}
	h.mustPass()
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []observedInstance{{Name: "a", State: "exited"}}) {
		t.Fatalf("told the control plane %+v", got)
	}
	if h.logged("read the output of exited "+full("a")) != 1 {
		t.Fatalf("log = %q", h.log)
	}
}

func TestANameIsReportedOnceAndAsLiveWhileAnyInstanceOfItIs(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	rent(t, h.rented, full("a"))
	h.rented.Exit(full("a"), "the first one crashed")
	rent(t, h.rented, full("a"))
	rent(t, h.rented, full("b"))
	h.rented.Start(full("b"))
	rent(t, h.rented, full("b"))
	rent(t, h.rented, "hl-agent-other-zzz")
	h.mustPass()
	want := []observedInstance{{Name: "a", State: "starting"}, {Name: "b", State: "running", Progress: new(string)}}
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, want) {
		t.Fatalf("told the control plane %+v\nwant %+v", got, want)
	}
	if h.rented.Calls("Logs") != 1 {
		t.Fatalf("output was read %d times, want once, for the progress of the one running instance", h.rented.Calls("Logs"))
	}
}

func TestALauncherWithNothingRentedSaysSoWithAnEmptyList(t *testing.T) {
	h := newHarness(t)
	h.mustPass()
	h.cp.mu.Lock()
	defer h.cp.mu.Unlock()
	if h.cp.asked[0].Instances == nil || len(h.cp.asked[0].Instances) != 0 {
		t.Fatalf("instances = %#v, want an empty list and never null", h.cp.asked[0].Instances)
	}
}

func TestTheLaunchDigestFollowsTheLaunchersOwnInputsAndHoldsNoSecret(t *testing.T) {
	h := newHarness(t)
	h.mustPass()
	h.mustPass()
	digest := h.cp.lastQuestion().LaunchDigest
	h.cp.mu.Lock()
	first := h.cp.asked[0].LaunchDigest
	h.cp.mu.Unlock()
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || digest != first {
		t.Fatalf("digest %q (first pass %q): want 64 hex characters, the same on every pass", digest, first)
	}
	if strings.Contains(digest, oauthToken) || strings.Contains(digest, launcherToken) {
		t.Fatal("the digest holds a secret")
	}
	again, err := h.config().launchDigest()
	if err != nil || again != digest {
		t.Fatalf("a second launcher of the same config digests to %q, %v", again, err)
	}
	for what, edit := range map[string]func(*Config){
		"command":        func(c *Config) { c.Command = []string{"uv", "run", "another"} },
		"cpu":            func(c *Config) { c.Size.CPUMillis = 4000 },
		"memory":         func(c *Config) { c.Size.MemoryBytes = 8 << 30 },
		"disk":           func(c *Config) { c.Size.DiskBytes = 5 << 30 },
		"credential":     func(c *Config) { c.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "a-rotated-credential"} },
		"credential key": func(c *Config) { c.Env = map[string]string{"ANOTHER_KEY": oauthToken} },
		"model provider": func(c *Config) { c.ModelProvider = "another" },
	} {
		cfg := h.config()
		edit(&cfg)
		if changed, err := cfg.launchDigest(); err != nil || changed == digest {
			t.Errorf("a changed %s left the digest as it was (%v)", what, err)
		}
	}
	for what, edit := range map[string]func(*Config){
		"token":    func(c *Config) { c.Token = "hls_another" },
		"interval": func(c *Config) { c.Interval = time.Hour },
	} {
		cfg := h.config()
		edit(&cfg)
		if same, _ := cfg.launchDigest(); same != digest {
			t.Errorf("a changed %s changed the digest, which would replace every running agent", what)
		}
	}
}

func TestAnAnswerLargerThanTheBoundChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("kept")}, nil)
	h.mustPass()
	h.launcher.maxAnswer = 256
	big := agent("big")
	big.Env["AGENT_HYPERPARAMETERS"] = strings.Repeat("x", 1024)
	h.cp.want([]entry{big}, nil)
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "larger than 256 bytes") {
		t.Fatalf("err = %v", err)
	}
	h.names(full("kept"))
	if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatal("the provider was changed after an answer that was not read whole")
	}
}

func rent(t *testing.T, rented *fake.Provider, name string) string {
	t.Helper()
	id, err := rented.Create(context.Background(), provider.Spec{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOfTwoInstancesOfOneNameTheOlderLiveOneIsKept(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	first := rent(t, h.rented, full("a"))
	rent(t, h.rented, full("a"))
	rent(t, h.rented, full("a"))
	h.mustPass()
	if got := h.rented.IDs(full("a")); !reflect.DeepEqual(got, []string{first}) {
		t.Fatalf("kept %v, want %v", got, []string{first})
	}
	if h.rented.Calls("Create") != 3 {
		t.Fatal("a duplicate was answered with another create")
	}
}

func TestOfAnExitedAndALiveInstanceOfOneNameTheLiveOneIsKept(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	rent(t, h.rented, full("a"))
	h.rented.Exit(full("a"))
	live := rent(t, h.rented, full("a"))
	h.mustPass()
	if got := h.rented.IDs(full("a")); !reflect.DeepEqual(got, []string{live}) {
		t.Fatalf("kept %v, want %v", got, []string{live})
	}
}

func TestDuplicatesOfAnUnwantedNameAllGo(t *testing.T) {
	h := newHarness(t)
	rent(t, h.rented, full("gone"))
	rent(t, h.rented, full("gone"))
	h.mustPass()
	h.names()
}

// lostAnswer rents the instance and then reports the call as failed, once.
type lostAnswer struct {
	provider.Provider
	lost bool
}

func (p *lostAnswer) Create(ctx context.Context, spec provider.Spec) (string, error) {
	id, err := p.Provider.Create(ctx, spec)
	if err == nil && !p.lost {
		p.lost = true
		return "", context.DeadlineExceeded
	}
	return id, err
}

func TestACreateThatFailedAfterItSucceededIsNotRepeated(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Provider = &lostAnswer{Provider: h.rented}
	launcher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.cp.want([]entry{agent("a")}, nil)
	if err := launcher.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "create "+full("a")) {
		t.Fatalf("err = %v, want the failed create reported", err)
	}
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.names(full("a"))
	if h.rented.Calls("Create") != 1 {
		t.Fatal("the instance was rented twice")
	}
}

func TestAnImageChangeArrivesAsANewNameAndReplacesTheInstance(t *testing.T) {
	h := newHarness(t)
	old := agent("a-img1")
	h.cp.want([]entry{old}, nil)
	h.mustPass()
	h.rented.Start(full("a-img1"))

	changed := agent("a-img2")
	changed.Image = "registry.example.invalid/agent:2"
	h.cp.want([]entry{changed}, nil)
	h.mustPass()
	h.names(full("a-img2"))
	if got := h.rented.Spec(full("a-img2")).Image; got != changed.Image {
		t.Fatalf("image = %q", got)
	}
}

func TestOneInstanceFailingDoesNotStopThePass(t *testing.T) {
	h := newHarness(t)
	rent(t, h.rented, full("stale-1"))
	rent(t, h.rented, full("stale-2"))
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	h.rented.Fail = func(method, name string) error {
		if (method == "Create" && name == full("b")) || (method == "Destroy" && name == full("stale-1")) {
			return errors.New("vendor refused")
		}
		return nil
	}
	err := h.pass()
	if err == nil || !strings.Contains(err.Error(), "create "+full("b")) || !strings.Contains(err.Error(), "destroy "+full("stale-1")) {
		t.Fatalf("err = %v, want both failures named", err)
	}
	h.names(full("a"), full("c"), full("stale-1"))

	h.rented.Fail = nil
	h.mustPass()
	h.names(full("a"), full("b"), full("c"))
}

func TestAFailedListChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.cp.want([]entry{agent("b")}, nil)
	h.rented.Fail = func(method, _ string) error {
		if method == "List" {
			return errors.New("vendor down")
		}
		return nil
	}
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "list instances") {
		t.Fatalf("err = %v", err)
	}
	h.names(full("a"))
	if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatal("a pass that could not list acted anyway")
	}
}

func TestAnAnswerThatIsNotACompleteListChangesNothing(t *testing.T) {
	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"server error":            {http.StatusInternalServerError, `{"error":"database down"}`},
		"refused":                 {http.StatusUnauthorized, `{"error":"unauthenticated"}`},
		"forbidden":               {http.StatusForbidden, `{"instances":[],"ending":[]}`},
		"not json":                {http.StatusOK, `<html>bad gateway</html>`},
		"empty body":              {http.StatusOK, ``},
		"empty object":            {http.StatusOK, `{}`},
		"null":                    {http.StatusOK, `null`},
		"an error with 200":       {http.StatusOK, `{"error":"something"}`},
		"instances null":          {http.StatusOK, `{"instances":null,"ending":[]}`},
		"no ending list":          {http.StatusOK, `{"instances":[]}`},
		"truncated":               {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}`},
		"instance without a name": {http.StatusOK, `{"instances":[{"image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"instance without image":  {http.StatusOK, `{"instances":[{"name":"a","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"name outside the rules":  {http.StatusOK, `{"instances":[{"name":"A_b","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"name too long":           {http.StatusOK, `{"instances":[{"name":"` + strings.Repeat("x", 120) + `","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"one name twice":          {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"},{"name":"a","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"running and ending":      {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{},"ends_at":"2099-01-01T00:00:00Z"}],"ending":["a"]}`},
		"ending without a name":   {http.StatusOK, `{"instances":[],"ending":[""]}`},
		"sets the launcher's env": {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{"CLAUDE_CODE_OAUTH_TOKEN":"x"},"ends_at":"2099-01-01T00:00:00Z"}],"ending":[]}`},
		"instance without an end": {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{}}],"ending":[]}`},
		"an end that is no time":  {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{},"ends_at":"in three days"}],"ending":[]}`},
		"refusal without message": {http.StatusOK, `{"instances":[],"ending":[],"refusal":{"reason":"pass_does_not_fit"}}`},
		"conflict":                {http.StatusConflict, `{"instances":[],"ending":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cp.want([]entry{agent("kept")}, nil)
			h.mustPass()
			h.rented.Start(full("kept"))
			h.cp.answer(answer.status, answer.body)
			if err := h.pass(); err == nil || !strings.Contains(err.Error(), "fetch desired state") {
				t.Fatalf("err = %v", err)
			}
			h.names(full("kept"))
			if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 || h.rented.Calls("Stop") != 0 {
				t.Fatal("the provider was changed after a failed fetch")
			}
		})
	}
}

func TestAnUnreachableControlPlaneChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("kept")}, nil)
	h.mustPass()
	h.cp.server.Close()
	if err := h.pass(); err == nil {
		t.Fatal("no error")
	}
	h.names(full("kept"))
}

func TestAnExplicitlyEmptyListDestroysEverythingUnderTheLaunchersNameOnly(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	rent(t, h.rented, "hl-agent-other-a")
	rent(t, h.rented, "hl-job-main-exp-a1-cpu-0")
	rent(t, h.rented, "hl-agent-main2-a")
	h.cp.want(nil, nil)
	h.mustPass()
	h.names("hl-agent-main2-a", "hl-agent-other-a", "hl-job-main-exp-a1-cpu-0")
}

func TestNothingIsKeptBetweenPasses(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()

	// A second launcher process over the same account and control plane converges the same way.
	restarted, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	h.cp.want([]entry{agent("b"), agent("c")}, nil)
	if err := restarted.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.names(full("b"), full("c"))
}

// A restarted process learns of a failed create the way a running one does: the name is asked
// for again, since the control plane counts from when it asked and from what is listed.
func TestAFailedCreateIsRetriedByWhicheverProcessIsAskedNext(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	failing := true
	h.rented.Fail = func(method, _ string) error {
		if method == "Create" && failing {
			return errors.New("vendor: no capacity")
		}
		return nil
	}
	if err := h.pass(); err == nil {
		t.Fatal("the pass with a failed create returned no error")
	}
	failing = false
	restarted, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.names(full("a"))
	for i, question := range h.cp.asked {
		if len(question.Instances) != 0 {
			t.Fatalf("question %d reports %+v, want nothing: nothing was listed", i, question.Instances)
		}
	}
}

func TestEveryQuestionSaysHowTheLauncherPacesItsPasses(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Interval, cfg.CreatePause = 15*time.Second, 250*time.Millisecond
	launcher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.cp.lastQuestion(); got.ReconcileIntervalSeconds != 15 || got.CreatePauseMilliseconds != 250 {
		t.Fatalf("question = %+v, want interval 15s and pause 250ms", got)
	}
}

// The refusal stops creation and nothing else: the launcher's process is the only thing that
// stops and destroys what it holds, so the lists of a refused answer are acted on.
func TestARefusedPassStopsAndDestroysByTheListsAndCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("kept"), agent("over"), agent("winding-down")}, nil)
	h.mustPass()
	for _, name := range h.rented.Names() {
		h.rented.Start(name)
	}
	kept := h.rented.IDs(full("kept"))

	h.cp.refuse([]entry{agent("kept"), agent("new")}, []string{"winding-down"}, "this launcher's pass does not fit the platform's agents.boot_limit_seconds")
	for range 2 {
		err := h.pass()
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "pass_does_not_fit: this launcher's pass does not fit the platform's agents.boot_limit_seconds") {
			t.Fatalf("pass = %v, want the refusal in the control plane's words", err)
		}
	}
	h.names(full("kept"), full("winding-down"))
	if h.rented.Calls("Create") != 3 || h.rented.Calls("Destroy") != 1 || h.rented.Stops(full("winding-down")) != 1 || !reflect.DeepEqual(h.rented.IDs(full("kept")), kept) {
		t.Fatalf("refused passes: creates %d destroys %d stops %d, want nothing created, the one over destroyed and the ending one told to stop once",
			h.rented.Calls("Create"), h.rented.Calls("Destroy"), h.rented.Stops(full("winding-down")))
	}

	h.cp.want([]entry{agent("kept"), agent("new")}, nil)
	h.mustPass()
	h.names(full("kept"), full("new"))
}

func (h *harness) eventually(what string, reached func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !reached(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("not reached in time: %s", what)
		}
	}
}

// A launcher refused at its start may be the only thing left to end what an earlier process of
// its name rented, so its first pass is like any other: it ends by the lists and goes on passing.
func TestRunRefusedOnItsFirstPassEndsWhatAnEarlierProcessRentedAndKeepsPassing(t *testing.T) {
	h := newHarness(t)
	rent(t, h.rented, full("over"))
	rent(t, h.rented, full("winding-down"))
	h.cp.refuse([]entry{agent("new")}, []string{"winding-down"}, "is above agents.boot_limit_seconds 5m0s")
	launcher := h.launcherWith(func(*Config) {})
	launcher.cfg.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- launcher.Run(ctx) }()
	h.eventually("the earlier process's instances ended", func() bool {
		return h.rented.Stops(full("winding-down")) > 0 && reflect.DeepEqual(h.rented.Names(), []string{full("winding-down")})
	})
	h.eventually("the refusal said on every pass", func() bool { return h.logged("is above agents.boot_limit_seconds 5m0s") >= 3 })
	select {
	case err := <-done:
		t.Fatalf("Run returned %v on a refusal, with %v still rented", err, h.rented.Names())
	default:
	}
	if creates := h.rented.Calls("Create"); creates != 2 {
		t.Fatalf("creates = %d, want only the two of the earlier process", creates)
	}

	h.cp.want([]entry{agent("new")}, nil)
	h.eventually("the instance rented once the refusal is gone", func() bool { return reflect.DeepEqual(h.rented.Names(), []string{full("new")}) })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestAnExitedInstancesOutputIsSentWithoutTheLaunchersOwnCredential(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Exit(full("a"), "env: CLAUDE_CODE_OAUTH_TOKEN="+oauthToken, "SystemExit: the reason")
	h.mustPass()
	sent := h.cp.lastQuestion().Instances
	if len(sent) != 1 || strings.Contains(sent[0].Log, oauthToken) {
		t.Fatalf("sent %+v, which holds the model provider's credential", sent)
	}
	if sent[0].Log != "env: CLAUDE_CODE_OAUTH_TOKEN=<CLAUDE_CODE_OAUTH_TOKEN>\nSystemExit: the reason" {
		t.Fatalf("sent log %q", sent[0].Log)
	}
}

func TestNoSecretReachesTheLogOrAnInstanceName(t *testing.T) {
	h := newHarness(t)
	rent(t, h.rented, full("stale"))
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.rented.Fail = func(method, name string) error {
		if method == "Create" && name == full("b") {
			return errors.New("vendor refused")
		}
		return nil
	}
	err := h.pass()
	if err == nil {
		t.Fatal("no error")
	}
	spoken := strings.Join(h.log, "\n") + "\n" + err.Error() + "\n" + strings.Join(h.rented.Names(), "\n")
	for _, secret := range []string{launcherToken, oauthToken, agentToken} {
		if strings.Contains(spoken, secret) {
			t.Fatalf("%q appears in:\n%s", secret, spoken)
		}
	}
	if len(h.log) != 3 {
		t.Fatalf("log = %q, want one line for each of the destroy, the create and the failed create", h.log)
	}
}

func TestRunPassesUntilCancelledAndSurvivesAFailingPass(t *testing.T) {
	h := newHarness(t)
	launcher, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	launcher.cfg.Interval = 5 * time.Millisecond
	h.cp.answer(http.StatusInternalServerError, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = launcher.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	h.cp.want([]entry{agent("a")}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for len(h.rented.Names()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := h.rented.Names(); !reflect.DeepEqual(got, []string{full("a")}) {
		t.Fatalf("instances = %v", got)
	}
}

func TestAConfigThatCannotLaunchIsRefused(t *testing.T) {
	h := newHarness(t)
	for name, edit := range map[string]func(*Config){
		"no name":           func(c *Config) { c.Name = "" },
		"no api url":        func(c *Config) { c.APIURL = "" },
		"no token":          func(c *Config) { c.Token = "" },
		"name with dash":    func(c *Config) { c.Name = "team-a" },
		"interval of 1.5s":  func(c *Config) { c.Interval = 1500 * time.Millisecond },
		"pause of 1.5us":    func(c *Config) { c.CreatePause = 1500 * time.Nanosecond },
		"no command":        func(c *Config) { c.Command = nil },
		"no cpu":            func(c *Config) { c.Size.CPUMillis = 0 },
		"no memory":         func(c *Config) { c.Size.MemoryBytes = 0 },
		"no disk":           func(c *Config) { c.Size.DiskBytes = 0 },
		"no interval":       func(c *Config) { c.Interval = 0 },
		"no model provider": func(c *Config) { c.ModelProvider = "" },
		"no provider env":   func(c *Config) { c.Env = nil },
		"negative pause":    func(c *Config) { c.CreatePause = -time.Second },
		"empty env value":   func(c *Config) { c.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": ""} },
		"no provider":       func(c *Config) { c.Provider = nil },
		"no http client":    func(c *Config) { c.HTTPClient = nil },
		"no log":            func(c *Config) { c.Log = nil },
	} {
		cfg := h.config()
		edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func (h *harness) launcherWith(edit func(*Config)) *Launcher {
	h.t.Helper()
	cfg := h.config()
	edit(&cfg)
	launcher, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return launcher
}

func (h *harness) logged(text string) int {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	count := 0
	for _, line := range h.log {
		if strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// An instance whose host the provider cannot reach is told as unknown and left as it is: the
// control plane decides how long an agent it cannot see is kept.
func TestAnInstanceTheProviderCannotPlaceIsReportedAsUnknownAndLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Lose(full("a"))

	h.mustPass()
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []observedInstance{{Name: "a", State: "unknown"}}) {
		t.Fatalf("told the control plane %+v", got)
	}
	if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 || h.rented.Calls("Logs") != 0 {
		t.Fatalf("creates %d destroys %d log reads %d, want the instance left as it is", h.rented.Calls("Create"), h.rented.Calls("Destroy"), h.rented.Calls("Logs"))
	}
}

func TestAnInstanceThatStaysStartingIsReportedAndNeverReplacedFromHere(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	ids := h.rented.IDs(full("a"))
	for range 5 {
		h.mustPass()
		if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []observedInstance{{Name: "a", State: "starting"}}) {
			t.Fatalf("told the control plane %+v", got)
		}
	}
	if !reflect.DeepEqual(h.rented.IDs(full("a")), ids) || h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("a starting instance was replaced by the launcher: creates %d destroys %d", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}

	// The control plane gives up on it: another name, and the machine is replaced.
	h.cp.want([]entry{agent("a-again")}, nil)
	h.mustPass()
	h.names(full("a-again"))
}

// timed records when each create reached the provider.
type timed struct {
	provider.Provider
	mu sync.Mutex
	at []time.Time
}

func (p *timed) Create(ctx context.Context, spec provider.Spec) (string, error) {
	p.mu.Lock()
	p.at = append(p.at, time.Now())
	p.mu.Unlock()
	return p.Provider.Create(ctx, spec)
}

func TestCreatesOfOnePassAreSpacedByThePauseAndAFailedOneDoesNotEndThem(t *testing.T) {
	const pause = 60 * time.Millisecond
	h := newHarness(t)
	recorded := &timed{Provider: h.rented}
	launcher := h.launcherWith(func(c *Config) { c.Provider = recorded; c.CreatePause = pause })
	h.rented.Start(rentNamed(t, h.rented, full("here")))
	h.cp.want([]entry{agent("a"), agent("b"), agent("c"), agent("d"), agent("here")}, nil)
	h.rented.Fail = func(method, name string) error {
		if method == "Create" && (name == full("a") || name == full("b")) {
			return errors.New("vendor refused")
		}
		return nil
	}
	started := time.Now()
	err := launcher.Pass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create "+full("a")) || !strings.Contains(err.Error(), "create "+full("b")) {
		t.Fatalf("err = %v", err)
	}
	h.names(full("c"), full("d"), full("here"))
	if len(recorded.at) != 4 {
		t.Fatalf("creates = %d, want one for each missing instance", len(recorded.at))
	}
	if first := recorded.at[0].Sub(started); first >= pause {
		t.Errorf("the first create waited %s", first)
	}
	for i := 1; i < len(recorded.at); i++ {
		if gap := recorded.at[i].Sub(recorded.at[i-1]); gap < pause {
			t.Errorf("creates %d and %d are %s apart, want at least %s", i, i+1, gap, pause)
		}
	}
	if h.logged("NO INSTANCE COULD BE CREATED") != 0 {
		t.Errorf("a pass that started some agents says none could be: %q", h.log)
	}
}

func rentNamed(t *testing.T, rented *fake.Provider, name string) string {
	t.Helper()
	rent(t, rented, name)
	return name
}

func TestAPassCancelledInAPauseStopsRenting(t *testing.T) {
	h := newHarness(t)
	launcher := h.launcherWith(func(c *Config) { c.CreatePause = time.Hour })
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := launcher.Pass(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	h.names(full("a"))
}

func TestEveryCreateFailingIsLoudAndLeavesNothingRented(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	h.rented.Fail = func(method, _ string) error {
		if method == "Create" {
			return errors.New("the size asked for is above the plan: 20 GiB of disk")
		}
		return nil
	}
	for pass := 1; pass <= 3; pass++ {
		err := h.pass()
		for _, name := range []string{"a", "b", "c"} {
			if err == nil || !strings.Contains(err.Error(), "create "+full(name)) {
				t.Fatalf("pass %d: err = %v, want every failed create named", pass, err)
			}
		}
		h.names()
		if got := h.logged("NO INSTANCE COULD BE CREATED: all 3 creates of this pass failed, the last with: the size asked for is above the plan"); got != pass {
			t.Fatalf("pass %d: the loud line was logged %d times: %q", pass, got, h.log)
		}
	}
	if h.rented.Calls("Create") != 9 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("creates %d destroys %d", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
}

// halfRented fails every create after renting the instance, as a vendor does whose start
// succeeded and whose command then could not be launched: the instance is listed as exited.
type halfRented struct {
	*fake.Provider
}

func (p halfRented) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if _, err := p.Provider.Create(ctx, spec); err != nil {
		return "", err
	}
	p.Provider.Exit(spec.Name)
	return "", errors.New("the command could not be launched")
}

func TestACreateThatFailsAndLeavesAnExitedInstanceIsNotRepeated(t *testing.T) {
	h := newHarness(t)
	launcher := h.launcherWith(func(c *Config) { c.Provider = halfRented{h.rented} })
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	if err := launcher.Pass(context.Background()); err == nil {
		t.Fatal("the failed creates were not reported")
	}
	for pass := 2; pass <= 5; pass++ {
		if err := launcher.Pass(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := h.cp.lastQuestion().Instances; len(got) != 2 || got[0].State != "exited" || got[1].State != "exited" {
			t.Fatalf("pass %d told the control plane %+v", pass, got)
		}
	}
	if h.rented.Calls("Create") != 2 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("creates %d destroys %d, want each instance rented once and left for the control plane", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
}

func TestAVendorErrorThatQuotesTheRequestBackReachesNoLogWithItsSecrets(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	refusal := fmt.Sprintf(`vendor: POST /machines: HTTP 422: {"error":"bad guest","request":{"env":{"HL_TOKEN":%q,"CLAUDE_CODE_OAUTH_TOKEN":%q,"AGENT_ID":"a"}}}`, agentToken, oauthToken)
	h.rented.Fail = func(method, _ string) error {
		if method == "Create" {
			return fmt.Errorf("create machine: %w", errors.New(refusal))
		}
		return nil
	}
	err := h.pass()
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") || !strings.Contains(err.Error(), "bad guest") {
		t.Fatalf("err = %v, want the vendor's refusal", err)
	}
	said := err.Error() + strings.Join(h.log, "\n")
	for _, secret := range []string{agentToken, oauthToken} {
		if strings.Contains(said, secret) {
			t.Fatalf("a secret of the instance's environment was logged or returned: %s", said)
		}
	}
	if !strings.Contains(said, "<HL_TOKEN>") || !strings.Contains(said, "<CLAUDE_CODE_OAUTH_TOKEN>") {
		t.Fatalf("the error does not say what was taken out: %s", said)
	}
	if h.logged("NO INSTANCE COULD BE CREATED") != 1 {
		t.Fatalf("log = %q", h.log)
	}
}

func TestTheNewestProgressMarkerOfEachRunningInstanceIsReported(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b"), agent("c"), agent("d")}, nil)
	h.mustPass()
	for _, name := range []string{"a", "b", "d"} {
		h.rented.Start(full(name))
	}
	h.rented.Print(full("a"), "hl-progress 3", "2026-01-01T00:00:00Z a say hello", "hl-progress 4", "a line after it")
	h.rented.Print(full("b"), "the harness is starting", "hl-progress", "hl-progress 4 and more", " hl-progress 9", "not hl-progress 5")
	h.rented.Print(full("c"), "hl-progress 7")
	h.rented.Print(full("d"), "hl-progress 1")
	h.rented.Fail = func(method, name string) error {
		if method == "Logs" && name == full("d") {
			return errors.New("vendor: the log API is away")
		}
		return nil
	}
	h.mustPass()
	four, none := "4", ""
	want := []observedInstance{
		{Name: "a", State: "running", Progress: &four},
		{Name: "b", State: "running", Progress: &none},
		{Name: "c", State: "starting"},
		{Name: "d", State: "running"},
	}
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, want) {
		t.Fatalf("told the control plane %+v\nwant %+v", got, want)
	}
	if h.logged("read the progress of running "+full("d")) != 1 {
		t.Fatalf("log = %q, want the failed read said once", h.log)
	}
	if h.logged("hl-progress") != 0 || h.logged("say hello") != 0 {
		t.Fatalf("an instance's output reached the launcher's log: %q", h.log)
	}
}

// What an agent is doing was read from its machine's log by whoever could reach the machine. The
// harness says it in a line beside its progress marker, and the launcher hands the newest to the
// control plane with the report it already makes: no second read, and nothing of its own
// environment in it.
func TestWhatARunningAgentSaysItIsDoingIsReportedWithItsProgress(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	h.mustPass()
	for _, name := range []string{"a", "b", "c"} {
		h.rented.Start(full(name))
	}
	h.rented.Print(full("a"), "hl-progress 1", "hl-activity thinking", "hl-progress 2", "hl-activity running hl capacity", "2026-01-01T00:00:00Z a tool Bash: hl capacity")
	h.rented.Print(full("b"), "hl-progress 1", "hl-activity", " hl-activity indented", "not hl-activity either")
	h.rented.Print(full("c"), "hl-progress 1", "hl-activity running "+oauthToken+" "+strings.Repeat("é", 200))
	before := h.rented.Calls("Logs")
	h.mustPass()
	told := h.cp.lastQuestion().Instances
	if told[0].Activity != "running hl capacity" || told[0].Progress == nil || *told[0].Progress != "2" {
		t.Fatalf("a: told %+v, want its newest sentence beside its newest marker", told[0])
	}
	if told[1].Activity != "" {
		t.Fatalf("b, which printed no such line: told activity %q, want none", told[1].Activity)
	}
	if got := []rune(told[2].Activity); len(got) != maxActivityLength || !strings.HasPrefix(told[2].Activity, "running <CLAUDE_CODE_OAUTH_TOKEN> é") {
		t.Fatalf("c: told %d characters %q, want this launcher's own environment taken out and the sentence cut to %d", len(got), told[2].Activity, maxActivityLength)
	}
	if reads := h.rented.Calls("Logs") - before; reads != 3 {
		t.Fatalf("the pass read the output of 3 running instances %d times, want once each", reads)
	}
}

// The model provider's rate limit an agent last read lived only in its machine's log. The harness
// prints it with every beat and the launcher hands the newest to the control plane from the same
// read as the progress.
func TestTheRateLimitARunningAgentLastReadIsReportedWithItsProgress(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Start(full("b"))
	h.rented.Print(full("a"), "hl-progress 1", "hl-rate-limit status=allowed_warning utilization=0.9", "hl-progress 2", "hl-rate-limit status=rejected type=seven_day resets_at=1760000000", "chatter")
	h.rented.Print(full("b"), "hl-progress 1", "hl-rate-limit", "not hl-rate-limit status=rejected")
	h.mustPass()
	told := h.cp.lastQuestion().Instances
	if told[0].RateLimit != "status=rejected type=seven_day resets_at=1760000000" {
		t.Fatalf("a: told rate limit %q, want its newest line", told[0].RateLimit)
	}
	if told[1].RateLimit != "" {
		t.Fatalf("b, which printed no such line: told rate limit %q, want none", told[1].RateLimit)
	}
}

func TestAMarkerFurtherBackThanTheLinesReadIsNotReported(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	lines := []string{"hl-progress 1"}
	for range progressLines {
		lines = append(lines, "chatter")
	}
	h.rented.Print(full("a"), lines...)
	h.mustPass()
	if got := h.cp.lastQuestion().Instances[0].Progress; got == nil || *got != "" {
		t.Fatalf("progress = %v, want read and none found", got)
	}
}
