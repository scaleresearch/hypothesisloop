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
	server *httptest.Server
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{status: http.StatusOK}
	cp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.calls = append(cp.calls, r)
		w.WriteHeader(cp.status)
		_, _ = w.Write([]byte(cp.body))
	}))
	t.Cleanup(cp.server.Close)
	cp.want(nil, nil)
	return cp
}

type entry struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
}

func agent(name string) entry {
	return entry{Name: name, Image: "registry.example.invalid/agent:1", Env: map[string]string{"AGENT_ID": name, "HL_TOKEN": agentToken}}
}

func (cp *controlPlane) want(instances []entry, ending []string) {
	if instances == nil {
		instances = []entry{}
	}
	if ending == nil {
		ending = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"instances": instances, "ending": ending})
	cp.answer(http.StatusOK, string(raw))
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
		Name:       "main",
		APIURL:     h.cp.server.URL,
		Token:      launcherToken,
		Namespace:  "ns",
		Command:    []string{"uv", "run", "agent"},
		Size:       Size{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		Env:        map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": oauthToken},
		Provider:   h.rented,
		HTTPClient: h.cp.server.Client(),
		Interval:   time.Second,
		BootLimit:  2 * time.Minute,
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

func full(name string) string { return "hl-agent-ns-" + name }

func TestAMissingInstanceIsCreatedFromTheListAndTheLaunchersOwnConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.names(full("a"), full("b"))

	spec := h.rented.Spec(full("a"))
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

func TestAnEndingInstanceIsLeftAloneWhateverItsState(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Exit(full("b"))
	idsA, idsB := h.rented.IDs(full("a")), h.rented.IDs(full("b"))

	h.cp.want(nil, []string{"a", "b", "never-rented"})
	h.mustPass()
	h.mustPass()
	h.names(full("a"), full("b"))
	if !reflect.DeepEqual(h.rented.IDs(full("a")), idsA) || !reflect.DeepEqual(h.rented.IDs(full("b")), idsB) {
		t.Fatal("an ending instance was replaced")
	}
	if h.rented.Calls("Create") != 2 || h.rented.Calls("Destroy") != 0 || h.rented.Stops(full("a")) != 0 {
		t.Fatalf("creates %d destroys %d", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}

	h.cp.want(nil, nil)
	h.mustPass()
	h.names()
}

func TestAnInstanceWhoseCommandExitedIsReplaced(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	crashed := h.rented.IDs(full("a"))
	h.rented.Exit(full("a"), "Traceback")

	h.mustPass()
	h.names(full("a"))
	if got := h.rented.IDs(full("a")); reflect.DeepEqual(got, crashed) {
		t.Fatal("the exited instance is still the one rented")
	}
}

func TestAnExitedInstanceThatCannotBeDestroyedIsNotRentedBeside(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Exit(full("a"))
	h.rented.Fail = func(method, _ string) error {
		if method == "Destroy" {
			return errors.New("vendor down")
		}
		return nil
	}
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "destroy "+full("a")) {
		t.Fatalf("err = %v", err)
	}
	h.names(full("a"))
	if h.rented.Calls("Create") != 1 {
		t.Fatal("a second instance was rented beside one that could not be destroyed")
	}
	h.rented.Fail = nil
	h.mustPass()
	h.names(full("a"))
	if h.rented.Calls("Create") != 2 {
		t.Fatalf("creates = %d, want the replacement once the destroy went through", h.rented.Calls("Create"))
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
		"truncated":               {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{}}`},
		"instance without a name": {http.StatusOK, `{"instances":[{"image":"i","env":{}}],"ending":[]}`},
		"instance without image":  {http.StatusOK, `{"instances":[{"name":"a","env":{}}],"ending":[]}`},
		"name outside the rules":  {http.StatusOK, `{"instances":[{"name":"A_b","image":"i","env":{}}],"ending":[]}`},
		"name too long":           {http.StatusOK, `{"instances":[{"name":"` + strings.Repeat("x", 120) + `","image":"i","env":{}}],"ending":[]}`},
		"one name twice":          {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{}},{"name":"a","image":"i","env":{}}],"ending":[]}`},
		"running and ending":      {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{}}],"ending":["a"]}`},
		"ending without a name":   {http.StatusOK, `{"instances":[],"ending":[""]}`},
		"sets the launcher's env": {http.StatusOK, `{"instances":[{"name":"a","image":"i","env":{"CLAUDE_CODE_OAUTH_TOKEN":"x"}}],"ending":[]}`},
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
			if h.rented.Calls("List") != 1 || h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
				t.Fatal("the provider was touched after a failed fetch")
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

func TestAnExplicitlyEmptyListDestroysEverythingOfTheNamespaceOnly(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	rent(t, h.rented, "hl-agent-other-a")
	rent(t, h.rented, "hl-job-ns-exp-a1-cpu-0")
	rent(t, h.rented, "hl-agent-ns2-a")
	h.cp.want(nil, nil)
	h.mustPass()
	h.names("hl-agent-ns2-a", "hl-agent-other-a", "hl-job-ns-exp-a1-cpu-0")
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
	cfg := h.config()
	cfg.Interval = 5 * time.Millisecond
	launcher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.cp.answer(http.StatusInternalServerError, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { launcher.Run(ctx); close(done) }()

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
		"no name":             func(c *Config) { c.Name = "" },
		"no api url":          func(c *Config) { c.APIURL = "" },
		"no token":            func(c *Config) { c.Token = "" },
		"namespace with dash": func(c *Config) { c.Namespace = "team-a" },
		"no namespace":        func(c *Config) { c.Namespace = "" },
		"no command":          func(c *Config) { c.Command = nil },
		"no cpu":              func(c *Config) { c.Size.CPUMillis = 0 },
		"no memory":           func(c *Config) { c.Size.MemoryBytes = 0 },
		"no disk":             func(c *Config) { c.Size.DiskBytes = 0 },
		"no interval":         func(c *Config) { c.Interval = 0 },
		"no boot limit":       func(c *Config) { c.BootLimit = 0 },
		"negative pause":      func(c *Config) { c.CreatePause = -time.Second },
		"empty env value":     func(c *Config) { c.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": ""} },
		"no provider":         func(c *Config) { c.Provider = nil },
		"no http client":      func(c *Config) { c.HTTPClient = nil },
		"no log":              func(c *Config) { c.Log = nil },
	} {
		cfg := h.config()
		edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// since lists what the wrapped provider rents with the start times a test gives by name, as an
// adapter that reports when an instance was created does.
type since struct {
	provider.Provider
	started map[string]time.Time
}

func (p *since) List(ctx context.Context, prefix string) ([]provider.Instance, error) {
	listed, err := p.Provider.List(ctx, prefix)
	for i := range listed {
		if at, set := p.started[listed[i].Name]; set {
			listed[i].StartedAt = at
		}
	}
	return listed, err
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

func TestAnInstanceStartingPastTheBootLimitIsDestroyedAndRentedAgain(t *testing.T) {
	h := newHarness(t)
	clock := &since{Provider: h.rented, started: map[string]time.Time{}}
	launcher := h.launcherWith(func(c *Config) { c.Provider = clock })
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	stuck := h.rented.IDs(full("a"))
	slow := h.rented.IDs(full("b"))
	clock.started[full("a")] = time.Now().Add(-2*time.Minute - time.Second)
	clock.started[full("b")] = time.Now().Add(-2*time.Minute + 10*time.Second)

	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.names(full("a"), full("b"))
	if got := h.rented.IDs(full("a")); len(got) != 1 || got[0] == stuck[0] {
		t.Fatalf("the stuck instance was not replaced: %v, was %v", got, stuck)
	}
	if got := h.rented.IDs(full("b")); !reflect.DeepEqual(got, slow) {
		t.Fatalf("an instance inside the boot limit was replaced: %v, was %v", got, slow)
	}
	if h.logged("still starting after") != 1 || h.logged("the boot limit is 2m0s") != 1 {
		t.Fatalf("log = %q", h.log)
	}
}

func TestOnlyAStartingInstanceWithAKnownAgeIsHeldToTheBootLimit(t *testing.T) {
	long := time.Now().Add(-24 * time.Hour)
	for name, tc := range map[string]struct {
		started time.Time
		running bool
	}{
		"no start time reported":      {started: time.Time{}},
		"the epoch for no start time": {started: time.Unix(0, 0)},
		"a start time in the future":  {started: time.Now().Add(time.Hour)},
		"running for a day":           {started: long, running: true},
	} {
		h := newHarness(t)
		clock := &since{Provider: h.rented, started: map[string]time.Time{}}
		launcher := h.launcherWith(func(c *Config) { c.Provider = clock })
		h.cp.want([]entry{agent("a")}, nil)
		if err := launcher.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if tc.running {
			h.rented.Start(full("a"))
		}
		clock.started[full("a")] = tc.started
		for range 3 {
			if err := launcher.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
			t.Errorf("%s: creates %d destroys %d, want the instance left alone", name, h.rented.Calls("Create"), h.rented.Calls("Destroy"))
		}
	}
}

func TestAStuckInstanceThatCannotBeDestroyedIsNotRentedBeside(t *testing.T) {
	h := newHarness(t)
	clock := &since{Provider: h.rented, started: map[string]time.Time{}}
	launcher := h.launcherWith(func(c *Config) { c.Provider = clock })
	h.cp.want([]entry{agent("a")}, nil)
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.started[full("a")] = time.Now().Add(-time.Hour)
	h.rented.Fail = func(method, _ string) error {
		if method == "Destroy" {
			return errors.New("vendor refused")
		}
		return nil
	}
	if err := launcher.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "destroy "+full("a")) {
		t.Fatalf("err = %v", err)
	}
	if got := h.rented.IDs(full("a")); len(got) != 1 || h.rented.Calls("Create") != 1 {
		t.Fatalf("instances %v, creates %d", got, h.rented.Calls("Create"))
	}
}

func TestAnEndingInstanceIsNotHeldToTheBootLimit(t *testing.T) {
	h := newHarness(t)
	clock := &since{Provider: h.rented, started: map[string]time.Time{full("a"): time.Now().Add(-time.Hour)}}
	launcher := h.launcherWith(func(c *Config) { c.Provider = clock })
	rent(t, h.rented, full("a"))
	h.cp.want(nil, []string{"a"})
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.rented.Calls("Destroy") != 0 || h.rented.Calls("Create") != 1 {
		t.Fatalf("destroys %d creates %d", h.rented.Calls("Destroy"), h.rented.Calls("Create"))
	}
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
	if h.logged("NO AGENT COULD BE STARTED") != 0 {
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
		if got := h.logged("NO AGENT COULD BE STARTED: all 3 creates of this pass failed, the last with: the size asked for is above the plan"); got != pass {
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

func TestACreateThatFailsAndLeavesAnExitedInstanceNeverLeavesMoreThanOne(t *testing.T) {
	h := newHarness(t)
	launcher := h.launcherWith(func(c *Config) { c.Provider = halfRented{h.rented} })
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	for pass := 1; pass <= 4; pass++ {
		if err := launcher.Pass(context.Background()); err == nil {
			t.Fatalf("pass %d: no error", pass)
		}
		if a, b := h.rented.IDs(full("a")), h.rented.IDs(full("b")); len(a) != 1 || len(b) != 1 {
			t.Fatalf("pass %d: instances of a %v, of b %v, want one each", pass, a, b)
		}
	}
	if h.rented.Calls("Destroy") != 6 {
		t.Fatalf("destroys = %d, want each leftover destroyed before its replacement", h.rented.Calls("Destroy"))
	}
}
