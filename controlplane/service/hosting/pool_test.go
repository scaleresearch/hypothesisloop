package hosting

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

const image1, image2 = "registry.example.invalid/agent:1", "registry.example.invalid/agent:2"

func newPoolHarness(t *testing.T, size int) *harness {
	t.Helper()
	h := newHarness(t)
	cfg := h.config()
	cfg.WarmPool = size
	launcher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.launcher = launcher
	return h
}

// warmPool runs a pass that fills the pool for image and makes what it warmed ready.
func (h *harness) warmPool(image string) string {
	h.t.Helper()
	h.mustPass()
	name := h.launcher.poolName(image)
	h.rented.Ready(name)
	return name
}

// calls records the provider's calls in order, as "method name".
func (h *harness) calls() func() []string {
	var mu sync.Mutex
	var seen []string
	h.rented.Fail = func(method, name string) error {
		mu.Lock()
		defer mu.Unlock()
		if method != "List" && method != "Logs" {
			seen = append(seen, method+" "+name)
		}
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestThePoolIsFilledForTheImageAgentsAreAskedForAtTheLaunchersSizeAndIsNeverReported(t *testing.T) {
	h := newPoolHarness(t, 2)
	h.mustPass()
	h.names()

	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	pool := h.launcher.poolName(image1)
	h.names(full("a"), pool, pool)
	spec := h.rented.Spec(pool)
	want := provider.Spec{Name: pool, Image: image1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("pool spec = %+v\nwant %+v", spec, want)
	}
	if !strings.HasPrefix(pool, "hl-agent-main-pool-") {
		t.Fatalf("pool name %q is not under the launcher's prefix", pool)
	}

	h.rented.Ready(pool)
	h.mustPass()
	for _, observed := range h.cp.lastQuestion().Instances {
		if observed.Name != "a" {
			t.Fatalf("the control plane was told of %+v", observed)
		}
	}
	h.names(full("a"), pool, pool)
}

func TestAMissingAgentIsStartedOnAWarmInstanceAndThePoolToppedUp(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	pool := h.warmPool(image1)
	warmID := h.rented.IDs(pool)[0]

	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	order := h.calls()
	h.mustPass()
	if got, want := order(), []string{"StartWarm " + full("b"), "Warm " + pool}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if ids := h.rented.IDs(full("b")); !reflect.DeepEqual(ids, []string{warmID}) {
		t.Fatalf("b runs on %v, want the warm instance %s", ids, warmID)
	}
	spec := h.rented.Spec(full("b"))
	if !reflect.DeepEqual(spec.Env, map[string]string{"AGENT_ID": "b", "HL_TOKEN": agentToken}) || !reflect.DeepEqual(spec.Command, []string{"uv", "run", "agent"}) {
		t.Fatalf("the warm instance was started with %+v", spec)
	}
	if h.logged("started "+full("b")+" (asked for and not rented) on warm instance "+warmID) != 1 {
		t.Fatalf("log = %q", h.log)
	}
	h.names(full("a"), full("b"), pool)
}

func TestAnEndingAgentThatExitedGoesAndThePoolStaysFull(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	pool := h.warmPool(image1)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Ready(pool)
	h.rented.Start(full("b"))
	h.names(full("a"), full("b"), pool)

	h.cp.want([]entry{agent("a")}, []string{"b"})
	h.mustPass()
	h.rented.Exit(full("b"))
	h.mustPass()
	h.names(full("a"), pool)
	h.mustPass()
	h.names(full("a"), pool)
}

func TestAWarmInstanceThatFailsToStartFailsThePassAndIsDestroyed(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	pool := h.warmPool(image1)
	h.rented.Start(full("a"))
	warmID := h.rented.IDs(pool)[0]
	creates := h.rented.Calls("Create")

	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.rented.Fail = func(method, name string) error {
		if method == "StartWarm" {
			return errors.New("vendor: refused, token " + agentToken)
		}
		return nil
	}
	err := h.pass()
	if err == nil || !strings.Contains(err.Error(), "start "+full("b")) || strings.Contains(err.Error(), agentToken) {
		t.Fatalf("pass = %v, want the failed start without the agent's token", err)
	}
	h.names(full("a"), pool)
	if ids := h.rented.IDs(pool); len(ids) != 1 || ids[0] == warmID {
		t.Fatalf("pool = %v, want a new instance in place of %s", ids, warmID)
	}
	if h.rented.Calls("Create") != creates {
		t.Fatal("the agent was created in place of the warm start")
	}
	if h.logged(agentToken) != 0 {
		t.Fatalf("the agent's token reached the log: %q", h.log)
	}

	h.rented.Fail = nil
	h.mustPass()
	h.names(full("a"), full("b"), pool)
}

func TestAnAgentOfAnotherImageIsCreatedAndThePoolFollowsTheImageAsked(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	old := h.warmPool(image1)
	h.rented.Start(full("a"))

	// While an agent is asked for under the pool's image, the pool keeps it.
	other := agent("b")
	other.Image = image2
	h.cp.want([]entry{agent("a"), other, func() entry { e := agent("c"); e.Image = image2; return e }()}, nil)
	order := h.calls()
	h.mustPass()
	if got := order(); !reflect.DeepEqual(got, []string{"Create " + full("b"), "Create " + full("c")}) {
		t.Fatalf("calls = %v, want both agents of another image created and the pool kept", got)
	}

	// Once none is, the pool is replaced with the image most agents are asked for under.
	h.cp.want([]entry{other}, nil)
	h.mustPass()
	h.names(full("b"), h.launcher.poolName(image2))
	if h.logged("destroyed "+old+" (a pool instance for another image or launch)") != 1 {
		t.Fatalf("log = %q", h.log)
	}

	// An answer that asks for nothing leaves the pool as it is.
	h.cp.want(nil, nil)
	h.mustPass()
	h.names(h.launcher.poolName(image2))
}

func TestWithNoAgentAskedForThePoolHoldsThePlatformsImage(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.idle(image1)
	h.mustPass()
	pool := h.launcher.poolName(image1)
	h.names(pool)
	h.rented.Ready(pool)

	// The planner, the first agent of an experiment, starts on it.
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	if h.rented.Calls("Create") != 0 || h.logged("on warm instance") != 1 {
		t.Fatalf("creates = %d, log = %q, want a warm start", h.rented.Calls("Create"), h.log)
	}

	// An idle platform whose image changed moves the pool to the new one.
	h.cp.idle(image2)
	h.mustPass()
	h.names(h.launcher.poolName(image2))
}

func TestThePlatformsImageNeverOutweighsTheAgentsAskedFor(t *testing.T) {
	h := newPoolHarness(t, 1)
	other := agent("a")
	other.Image = image2
	h.cp.answer(Desired{Instances: []Instance{other}, Image: image1})
	h.mustPass()
	h.names(full("a"), h.launcher.poolName(image2))
}

func TestPoolInstancesThatRanOrHungOrAreTooManyGoAndAPoolOfNoneEmptiesIt(t *testing.T) {
	h := newPoolHarness(t, 2)
	h.cp.want([]entry{agent("a")}, nil)
	pool := h.warmPool(image1)
	ids := h.rented.IDs(pool)
	if len(ids) != 2 {
		t.Fatalf("pool = %v", ids)
	}

	// Something started them: a pool instance is never left running.
	h.rented.Start(pool)
	h.mustPass()
	h.rented.Ready(pool)
	for _, id := range h.rented.IDs(pool) {
		if id == ids[0] || id == ids[1] {
			t.Fatalf("pool instance %s ran and is still there", id)
		}
	}
	if h.logged("(a pool instance that is running)") != 2 {
		t.Fatalf("log = %q", h.log)
	}

	smaller := h.config()
	smaller.WarmPool = 1
	launcher, err := New(smaller)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.rented.IDs(pool)); got != 1 {
		t.Fatalf("a pool of 1 holds %d", got)
	}

	off := h.config()
	launcher, err = New(off)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.names(full("a"))
}

func TestAPoolInstanceThatNeverBecomesWarmIsReplaced(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	h.mustPass()
	pool := h.launcher.poolName(image1)
	stuck := h.rented.IDs(pool)[0]
	h.rented.Age(pool, warmLimit+time.Minute)
	h.mustPass()
	if ids := h.rented.IDs(pool); len(ids) != 1 || ids[0] == stuck {
		t.Fatalf("pool = %v, want %s replaced", ids, stuck)
	}
}

func TestTheInstanceAReplacementReplacesIsDestroyedBeforeItStarts(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a-v1")}, nil)
	pool := h.warmPool(image1)
	h.rented.Start(full("a-v1"))

	h.cp.want([]entry{agent("a-v2")}, nil)
	order := h.calls()
	h.mustPass()
	want := []string{"Destroy " + full("a-v1"), "StartWarm " + full("a-v2"), "Warm " + pool}
	if got := order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// A warm start rents nothing, so the pause between rentals never comes before it.
func TestAWarmStartIsNeverPausedFor(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("a")}, nil)
	h.warmPool(image1)
	cfg := h.config()
	cfg.WarmPool, cfg.CreatePause = 1, time.Hour
	launcher, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := launcher.Pass(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the pass waited the create pause")
	}
	h.names(full("a"), full("b"), h.launcher.poolName(image1))
}

func TestRunDestroysThePoolWhenItEnds(t *testing.T) {
	h := newPoolHarness(t, 2)
	h.launcher.cfg.Interval = 5 * time.Millisecond
	h.cp.want([]entry{agent("a")}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.launcher.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.rented.Names()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	h.names(full("a"))
}

func TestAnAnswerNamingThePoolIsRefusedWhole(t *testing.T) {
	h := newPoolHarness(t, 1)
	h.cp.want([]entry{agent("pool-x")}, nil)
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "warm pool") {
		t.Fatalf("pass = %v", err)
	}
	h.cp.want(nil, []string{"pool-x"})
	if err := h.pass(); err == nil || !strings.Contains(err.Error(), "warm pool") {
		t.Fatalf("pass = %v", err)
	}
	h.names()
}

func TestAPoolNeedsAProviderThatWarms(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.WarmPool = 1
	cfg.Provider = struct{ provider.Provider }{h.rented}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "warm") {
		t.Fatalf("New = %v", err)
	}
	cfg.WarmPool = 0
	if _, err := New(cfg); err != nil {
		t.Fatalf("New without a pool: %v", err)
	}
}

// Every pass reads the output of every running and every exited instance afresh.
func TestOutputIsReadOnEveryPass(t *testing.T) {
	h := newHarness(t)
	h.cp.want([]entry{agent("a"), agent("b")}, nil)
	h.mustPass()
	h.rented.Start(full("a"))
	h.rented.Start(full("b"))
	h.rented.Print(full("a"), "hl-progress 1")
	h.mustPass()
	h.rented.Print(full("a"), "hl-progress 2")
	h.mustPass()
	if got := *h.cp.lastQuestion().Instances[0].Progress; got != "2" {
		t.Fatalf("progress = %q, want this pass's read", got)
	}
	if reads := h.rented.Calls("Logs"); reads != 4 {
		t.Fatalf("log reads = %d, want one per running instance and pass", reads)
	}

	h.rented.Exit(full("b"), "the end")
	h.mustPass()
	h.mustPass()
	if reads := h.rented.Calls("Logs"); reads != 8 {
		t.Fatalf("log reads = %d, want one per instance and pass", reads)
	}
	if got := h.cp.lastQuestion().Instances[1].Log; got != "the end" {
		t.Fatalf("exit log = %q", got)
	}
}
