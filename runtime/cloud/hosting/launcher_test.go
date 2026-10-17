package hosting

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/hostingapi"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider/fake"
)

const agentToken = "hls_agent-token"

// controlPlane is the platform as the launcher sees it, answering with whatever the test last set.
type controlPlane struct {
	mu      sync.Mutex
	desired hostingapi.Desired
	err     error
	// asked holds every question, as the launcher asked it.
	asked []hostingapi.Observed
	// held is what Held answers besides the pool, heldErr a failure in its place.
	held    []hostingapi.HeldMachine
	heldErr error
	// pool is the warm pool the launcher last reported, which Held answers too, as the decider
	// does.
	pool []hostingapi.PoolMachine
}

func (cp *controlPlane) Held(context.Context) ([]hostingapi.HeldMachine, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.heldErr != nil {
		return nil, cp.heldErr
	}
	held := append([]hostingapi.HeldMachine{}, cp.held...)
	for _, m := range cp.pool {
		held = append(held, hostingapi.HeldMachine{Name: m.Name, Machine: m.Machine})
	}
	return held, nil
}

func (cp *controlPlane) Reconcile(_ context.Context, observed hostingapi.Observed) (*hostingapi.Desired, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.asked = append(cp.asked, observed)
	if cp.err != nil {
		return nil, cp.err
	}
	cp.pool = observed.Pool
	answer := hostingapi.Desired{Instances: []hostingapi.Instance{}, Ending: append([]string{}, cp.desired.Ending...), Image: cp.desired.Image}
	for _, i := range cp.desired.Instances {
		i.Env = maps.Clone(i.Env)
		answer.Instances = append(answer.Instances, i)
	}
	return &answer, nil
}

// holds sets the machines the platform holds.
func (cp *controlPlane) holds(machines ...hostingapi.HeldMachine) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.held = machines
}

func (cp *controlPlane) lastQuestion() hostingapi.Observed {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.asked[len(cp.asked)-1]
}

type entry = hostingapi.Instance

func agent(name string) entry {
	return entry{Name: name, Image: "registry.example.invalid/agent:1", Env: map[string]string{"AGENT_ID": name, "HL_TOKEN": agentToken}}
}

func (cp *controlPlane) want(instances []entry, ending []string) {
	cp.answer(hostingapi.Desired{Instances: instances, Ending: ending})
}

// idle answers that no agent is wanted and that the platform's image is image.
func (cp *controlPlane) idle(image string) {
	cp.answer(hostingapi.Desired{Image: image})
}

func (cp *controlPlane) answer(desired hostingapi.Desired) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.desired, cp.err = desired, nil
}

// fail has every answer be err.
func (cp *controlPlane) fail(err error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.err = err
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
	h := &harness{t: t, cp: &controlPlane{}, rented: fake.New()}
	launcher, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	h.launcher = launcher
	return h
}

func (h *harness) config() Config {
	return Config{
		Name:     "main",
		Command:  []string{"uv", "run", "agent"},
		Size:     Size{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		Provider: h.rented,
		Platform: h.cp,
		Interval: time.Second,
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
	want := provider.Spec{
		Name:        full("a"),
		Image:       "registry.example.invalid/agent:1",
		Command:     []string{"uv", "run", "agent"},
		Env:         map[string]string{"AGENT_ID": "a", "HL_TOKEN": agentToken},
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

func TestAnEndingInstanceIsToldToStopOnEveryPassUntilItsCommandExits(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b"), agent("c")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Exit(full("b"))
	idsA, idsB, idsC := h.rented.IDs(full("a")), h.rented.IDs(full("b")), h.rented.IDs(full("c"))

	h.cp.want(nil, []string{"a", "b", "c", "never-rented"})
	h.mustPass()
	h.mustPass()
	// One whose command already exited has had its last turn and goes at once, needing no
	// signal.
	h.names(full("a"), full("c"))
	if !reflect.DeepEqual(h.rented.IDs(full("a")), idsA) || !reflect.DeepEqual(h.rented.IDs(full("c")), idsC) || len(idsB) != 1 {
		t.Fatal("an ending instance was replaced")
	}
	if h.rented.Calls("Create") != 3 || h.rented.Calls("Destroy") != 1 {
		t.Fatalf("creates %d destroys %d, want 3 and the exited one", h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
	if a, c := h.rented.Stops(full("a")), h.rented.Stops(full("c")); a != 2 || c != 2 || h.rented.Calls("Stop") != 4 {
		t.Fatalf("stops: running %d, starting %d, all %d, want one each per pass and none to the exited one", a, c, h.rented.Calls("Stop"))
	}

	h.rented.Exit(full("c"))
	h.mustPass()
	if a := h.rented.Stops(full("a")); a != 3 || h.rented.Calls("Stop") != 5 {
		t.Fatalf("stops: running %d, all %d, want 3 and none to the exited one", a, h.rented.Calls("Stop"))
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
	// The next pass stops both again.
	h.rented.Fail = nil
	h.mustPass()
	if a, b := h.rented.Stops(full("a")), h.rented.Stops(full("b")); a != 1 || b != 2 {
		t.Fatalf("stops after a failed one: %d and %d, want 1 and 2", a, b)
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
		want := []hostingapi.ObservedInstance{
			{Name: "a", Machine: crashed[0], State: "exited", Log: "starting\nTraceback (most recent call last):\nSystemExit: agent: the reason"},
			{Name: "b", Machine: h.rented.IDs(full("b"))[0], State: "running", Progress: new(string)},
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
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []hostingapi.ObservedInstance{{Name: "a", Machine: "fake-0001", State: "exited"}}) {
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
	want := []hostingapi.ObservedInstance{{Name: "a", Machine: "fake-0002", State: "starting"}, {Name: "b", Machine: "fake-0003", State: "running", Progress: new(string)}}
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
	again, err := h.config().launchDigest()
	if err != nil || again != digest {
		t.Fatalf("a second launcher of the same config digests to %q, %v", again, err)
	}
	for what, edit := range map[string]func(*Config){
		"command": func(c *Config) { c.Command = []string{"uv", "run", "another"} },
		"cpu":     func(c *Config) { c.Size.CPUMillis = 4000 },
		"memory":  func(c *Config) { c.Size.MemoryBytes = 8 << 30 },
		"disk":    func(c *Config) { c.Size.DiskBytes = 5 << 30 },
	} {
		cfg := h.config()
		edit(&cfg)
		if changed, err := cfg.launchDigest(); err != nil || changed == digest {
			t.Errorf("a changed %s left the digest as it was (%v)", what, err)
		}
	}
	for what, edit := range map[string]func(*Config){
		"interval": func(c *Config) { c.Interval = time.Hour },
	} {
		cfg := h.config()
		edit(&cfg)
		if same, _ := cfg.launchDigest(); same != digest {
			t.Errorf("a changed %s changed the digest, which would replace every running agent", what)
		}
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

func TestAnAnswerThatCannotBeActedOnWholeChangesNothing(t *testing.T) {
	with := func(desired hostingapi.Desired) func(*controlPlane) {
		return func(cp *controlPlane) { cp.answer(desired) }
	}
	one := func(name string) hostingapi.Instance {
		return hostingapi.Instance{Name: name, Image: "i", Env: map[string]string{}}
	}
	for name, set := range map[string]func(*controlPlane){
		"an error":                func(cp *controlPlane) { cp.fail(errors.New("database down")) },
		"instance without a name": with(hostingapi.Desired{Instances: []hostingapi.Instance{{Image: "i"}}}),
		"instance without image":  with(hostingapi.Desired{Instances: []hostingapi.Instance{{Name: "a"}}}),
		"name outside the rules":  with(hostingapi.Desired{Instances: []hostingapi.Instance{one("A_b")}}),
		"name too long":           with(hostingapi.Desired{Instances: []hostingapi.Instance{one(strings.Repeat("x", 120))}}),
		"one name twice":          with(hostingapi.Desired{Instances: []hostingapi.Instance{one("a"), one("a")}}),
		"running and ending":      with(hostingapi.Desired{Instances: []hostingapi.Instance{one("a")}, Ending: []string{"a"}}),
		"ending without a name":   with(hostingapi.Desired{Ending: []string{""}}),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cp.want([]entry{agent("kept")}, nil)
			h.mustPass()
			h.rented.Start(full("kept"))
			set(h.cp)
			if err := h.pass(); err == nil || !strings.Contains(err.Error(), "desired state") {
				t.Fatalf("err = %v", err)
			}
			h.names(full("kept"))
			if h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 || h.rented.Calls("Stop") != 0 {
				t.Fatal("the provider was changed after an answer that was not acted on")
			}
		})
	}
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

// An instance is started with the control plane's environment and nothing of the launcher's own:
// a model credential in the launcher's environment, as an older deployment had, never reaches a
// machine, created or started from the warm pool.
func TestNoInstanceIsGivenACredentialOfTheLaunchersEnvironment(t *testing.T) {
	const credential = "sk-ant-oat01-a-credential-the-launcher-holds"
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "MODEL_ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, credential)
	}
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	for _, name := range []string{"a", "b"} {
		spec := h.rented.Spec(full(name))
		if want := map[string]string{"AGENT_ID": name, "HL_TOKEN": agentToken}; !reflect.DeepEqual(spec.Env, want) {
			t.Fatalf("%s: env = %v, want exactly the control plane's %v", name, spec.Env, want)
		}
		for key, value := range spec.Env {
			if strings.Contains(value, credential) {
				t.Fatalf("%s: %s holds the launcher's credential", name, key)
			}
		}
	}
}

func TestAnExitedInstancesOutputIsSentAsItEnded(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Exit(full("a"), "Traceback", "SystemExit: the reason")
	h.mustPass()
	sent := h.cp.lastQuestion().Instances
	if len(sent) != 1 || sent[0].Log != "Traceback\nSystemExit: the reason" {
		t.Fatalf("sent %+v", sent)
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
	for _, secret := range []string{agentToken} {
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
	h.cp.fail(errors.New("database down"))
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
		"no name":        func(c *Config) { c.Name = "" },
		"name with dash": func(c *Config) { c.Name = "team-a" },
		"no command":     func(c *Config) { c.Command = nil },
		"no cpu":         func(c *Config) { c.Size.CPUMillis = 0 },
		"no memory":      func(c *Config) { c.Size.MemoryBytes = 0 },
		"no disk":        func(c *Config) { c.Size.DiskBytes = 0 },
		"no interval":    func(c *Config) { c.Interval = 0 },
		"negative pause": func(c *Config) { c.CreatePause = -time.Second },
		"no provider":    func(c *Config) { c.Provider = nil },
		"no platform":    func(c *Config) { c.Platform = nil },
		"no log":         func(c *Config) { c.Log = nil },
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
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []hostingapi.ObservedInstance{{Name: "a", Machine: "fake-0001", State: "unknown"}}) {
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
		if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, []hostingapi.ObservedInstance{{Name: "a", Machine: "fake-0001", State: "starting"}}) {
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
	refusal := fmt.Sprintf(`vendor: POST /machines: HTTP 422: {"error":"bad guest","request":{"env":{"HL_TOKEN":%q,"AGENT_ID":"a"}}}`, agentToken)
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
	for _, secret := range []string{agentToken} {
		if strings.Contains(said, secret) {
			t.Fatalf("a secret of the instance's environment was logged or returned: %s", said)
		}
	}
	if !strings.Contains(said, "<HL_TOKEN>") {
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
	want := []hostingapi.ObservedInstance{
		{Name: "a", Machine: "fake-0001", State: "running", Progress: &four},
		{Name: "b", Machine: "fake-0002", State: "running", Progress: &none},
		{Name: "c", Machine: "fake-0003", State: "starting"},
		{Name: "d", Machine: "fake-0004", State: "running"},
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
	h.rented.Print(full("c"), "hl-progress 1", "hl-activity running hl job wait "+strings.Repeat("é", 200))
	before := h.rented.Calls("Logs")
	h.mustPass()
	told := h.cp.lastQuestion().Instances
	if told[0].Activity != "running hl capacity" || told[0].Progress == nil || *told[0].Progress != "2" {
		t.Fatalf("a: told %+v, want its newest sentence beside its newest marker", told[0])
	}
	if told[1].Activity != "" {
		t.Fatalf("b, which printed no such line: told activity %q, want none", told[1].Activity)
	}
	if got := []rune(told[2].Activity); len(got) != hostingapi.MaxActivityLength || !strings.HasPrefix(told[2].Activity, "running hl job wait é") {
		t.Fatalf("c: told %d characters %q, want this launcher's own environment taken out and the sentence cut to %d", len(got), told[2].Activity, hostingapi.MaxActivityLength)
	}
	if reads := h.rented.Calls("Logs") - before; reads != 3 {
		t.Fatalf("the pass read the output of 3 running instances %d times, want once each", reads)
	}
}

// What an agent's harness says of itself (subagents, turn, the model provider's rate limit) lived
// only in its machine's log. The harness prints it with every beat and the launcher hands the
// newest line verbatim to the control plane from the same read as the progress.
func TestTheHarnessStateOfARunningAgentIsReportedWithItsProgress(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Start(full("b"))
	newest := "subagents=2 turn=waiting since=1759990000 model_status=rejected model_type=seven_day model_resets_at=1760000000"
	h.rented.Print(full("a"), "hl-progress 1", "hl-state subagents=0 turn=working since=1759980000", "hl-progress 2", "hl-state "+newest, "chatter")
	h.rented.Print(full("b"), "hl-progress 1", "hl-state", "not hl-state subagents=1 turn=working since=1759980000")
	h.mustPass()
	told := h.cp.lastQuestion().Instances
	if told[0].HarnessState != newest {
		t.Fatalf("a: told harness state %q, want its newest line", told[0].HarnessState)
	}
	if told[1].HarnessState != "" {
		t.Fatalf("b, which printed no such line: told harness state %q, want none", told[1].HarnessState)
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

// A machine the platform holds that the provider's list leaves out is read by its id, and
// reported as that read finds it: running, so nothing is created.
func TestAHeldMachineTheListLeavesOutIsReadByItsIDAndReportedAsFound(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	id := h.rented.IDs(full("a"))[0]
	h.cp.holds(hostingapi.HeldMachine{Name: "a", Machine: id})
	h.rented.Hide(full("a"))
	h.mustPass()
	want := []hostingapi.ObservedInstance{{Name: "a", Machine: id, State: "running", Progress: new(string)}}
	if got := h.cp.lastQuestion().Instances; !reflect.DeepEqual(got, want) {
		t.Fatalf("told the control plane %+v\nwant %+v", got, want)
	}
	if h.rented.Calls("Get") != 1 || h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("gets %d creates %d destroys %d", h.rented.Calls("Get"), h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
	if h.logged("instance "+id+" of "+full("a")+" is running and the provider's list left it out") != 1 {
		t.Fatalf("log = %q", h.log)
	}
	// Listed again, it is not read.
	h.rented.Show(full("a"))
	h.mustPass()
	if h.rented.Calls("Get") != 1 {
		t.Fatalf("gets = %d, want no read of a listed machine", h.rented.Calls("Get"))
	}
}

// Only a read that says the machine is gone leaves it out of the report.
func TestAHeldMachineThatIsGoneIsLeftOut(t *testing.T) {
	h := newHarness(t)
	h.cp.holds(hostingapi.HeldMachine{Name: "a", Machine: "fake-9999"})
	h.mustPass()
	if got := h.cp.lastQuestion().Instances; len(got) != 0 {
		t.Fatalf("told the control plane %+v, want nothing", got)
	}
	if h.rented.Calls("Get") != 1 {
		t.Fatalf("gets = %d", h.rented.Calls("Get"))
	}
}

// A read that fails, or a control plane that cannot say what it holds, fails the pass before
// anything is reported or changed.
func TestAPassThatCannotTellWhetherAHeldMachineIsGoneReportsNothing(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.cp.holds(hostingapi.HeldMachine{Name: "a", Machine: h.rented.IDs(full("a"))[0]})
	h.rented.Hide(full("a"))
	h.rented.Fail = func(method, _ string) error {
		if method == "Get" {
			return errors.New("vendor: 503")
		}
		return nil
	}
	questions := len(h.cp.asked)
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "is not listed and could not be read") {
		t.Fatalf("pass = %v, want the failed read", err)
	}
	h.rented.Fail = nil
	h.cp.mu.Lock()
	h.cp.heldErr = errors.New("database down")
	h.cp.mu.Unlock()
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "fetch the machines the platform holds") {
		t.Fatalf("pass = %v, want the failed fetch", err)
	}
	if len(h.cp.asked) != questions || h.rented.Calls("Create") != 1 || h.rented.Calls("Destroy") != 0 {
		t.Fatalf("questions %d (was %d), creates %d, destroys %d: nothing may be reported or changed", len(h.cp.asked), questions, h.rented.Calls("Create"), h.rented.Calls("Destroy"))
	}
}
