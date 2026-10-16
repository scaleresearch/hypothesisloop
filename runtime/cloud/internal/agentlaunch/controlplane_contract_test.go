package agentlaunch

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/launcherapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/launcherapi/launchertest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

const contractSecret = "a-signing-secret-only-this-test-knows"

const contractBootLimit = 5 * time.Minute

var contractSettings = launcherapi.Settings{
	Image: "registry.example.invalid/agent:1", APIURL: "https://api.example.invalid",
	SigningSecret: contractSecret, BootLimit: contractBootLimit, ProgressLimit: 4 * contractBootLimit, MaxNewPerAnswer: 50,
}

func hostedAgent(pe, agent string) db.LauncherAgent {
	return db.LauncherAgent{AgentSignup: domain.AgentSignup{
		PlatformExperimentID: pe, AgentID: agent, Hosted: true, Launcher: "main",
		Flavor: "data-search", Model: "model-1", Hyperparameters: map[string]any{"batch_size": float64(64)},
		Instance: &domain.AgentInstance{},
	}}
}

// registered is the authenticator's directory with the launchers table in it and nothing else:
// a launcher's token asks it nothing more.
type registered struct {
	auth.Directory
	launchers map[string]bool
}

func (r *registered) LauncherKnown(_ context.Context, name string) (bool, error) {
	return r.launchers[name], nil
}

type contract struct {
	t          *testing.T
	store      *launchertest.Store
	metrics    *launchertest.Metrics
	rented     *fake.Provider
	launcher   *Launcher
	registered *registered
}

func newContract(t *testing.T, agents ...db.LauncherAgent) *contract {
	t.Helper()
	return newContractUnder(t, contractSettings, agents...)
}

// newContractUnder runs the launcher's pass against the control plane's own handler behind its
// own authenticator, over HTTP, so a field renamed on either side fails here.
func newContractUnder(t *testing.T, settings launcherapi.Settings, agents ...db.LauncherAgent) *contract {
	t.Helper()
	c := &contract{
		t: t, rented: fake.New(), registered: &registered{launchers: map[string]bool{"main": true}},
		store:   &launchertest.Store{ByLauncher: map[string][]db.LauncherAgent{"main": agents}},
		metrics: &launchertest.Metrics{},
	}
	authenticator, err := auth.NewAuthenticator(config.AuthConfig{SigningSecret: contractSecret}, c.registered)
	if err != nil {
		t.Fatal(err)
	}
	launchers := chi.NewRouter()
	doc := apidocs.New(launchers, "launcher", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	launcherapi.RegisterHuma(doc, launcherapi.NewHandler(c.store, c.metrics, settings, zap.NewNop()))
	root := chi.NewRouter()
	root.Use(auth.Gate(authenticator, zap.NewNop(), nil))
	root.Mount("/internal/launchers", launchers)
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)

	token, err := auth.Mint(contractSecret, auth.LauncherScope("main"))
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := New(Config{
		Name: "main", APIURL: server.URL, Token: token,
		Command: []string{"run-agent"}, Size: Size{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		ModelProvider: "example", Env: map[string]string{"MODEL_CREDENTIAL": oauthToken},
		Provider: c.rented, HTTPClient: server.Client(), Interval: time.Second,
		Log: func(format string, args ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.launcher = launcher
	return c
}

func (c *contract) pass() {
	c.t.Helper()
	if err := c.launcher.Pass(context.Background()); err != nil {
		c.t.Fatalf("pass: %v", err)
	}
}

func (c *contract) instance(agentID string) domain.AgentInstance {
	return *c.store.Find("pe-1", agentID)
}

func TestTheLaunchersPassAgainstTheControlPlanesOwnHandler(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"), hostedAgent("pe-1", "ag-two"))
	c.pass()
	names := c.rented.Names()
	if len(names) != 2 || !strings.HasPrefix(names[0], "hl-agent-main-pe1-agone-") || !strings.HasPrefix(names[1], "hl-agent-main-pe1-agtwo-") {
		t.Fatalf("rented %v", names)
	}
	spec := c.rented.Spec(names[0])
	token, err := auth.Mint(contractSecret, auth.AgentScope("pe-1", "ag-one"))
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{"API_URL": "https://api.example.invalid", "HL_TOKEN": token, "MODEL_CREDENTIAL": oauthToken}
	if spec.Image != "registry.example.invalid/agent:1" || !reflect.DeepEqual(spec.Env, wantEnv) || !reflect.DeepEqual(spec.Command, []string{"run-agent"}) {
		t.Fatalf("spec = %+v\nwant env %v", spec, wantEnv)
	}
	if got := c.instance("ag-one").Name; "hl-agent-main-"+got != names[0] {
		t.Fatalf("the control plane recorded the name %q for the instance rented as %q", got, names[0])
	}

	// Nothing changed: the second pass rents and returns nothing.
	c.rented.Start(names[0])
	c.rented.Start(names[1])
	c.pass()
	if c.rented.Calls("Create") != 2 || c.rented.Calls("Destroy") != 0 || !reflect.DeepEqual(c.rented.Names(), names) {
		t.Fatalf("a pass with nothing to do: creates %d destroys %d names %v", c.rented.Calls("Create"), c.rented.Calls("Destroy"), c.rented.Names())
	}
}

func TestAnIdleControlPlaneHasTheLauncherWarmThePlatformsImage(t *testing.T) {
	c := newContract(t)
	c.launcher.cfg.WarmPool = 1
	c.pass()
	if want := []string{c.launcher.poolName(contractSettings.Image)}; !reflect.DeepEqual(c.rented.Names(), want) {
		t.Fatalf("rented %v, want the pool warmed for the platform's image %v", c.rented.Names(), want)
	}
}

func TestAnExitTravelsToTheControlPlaneWhichRestartsTheAgentAFewTimesAndThenStopsIt(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-crashing"), hostedAgent("pe-1", "ag-steady"))
	c.pass()
	steady := c.rented.Names()[1]
	c.rented.Start(steady)
	steadyIDs := c.rented.IDs(steady)

	seen := map[string]bool{}
	for exit := 1; exit <= domain.MaxAgentInstanceRestarts+1; exit++ {
		crashing := c.rented.Names()[0]
		if seen[crashing] || !strings.HasPrefix(crashing, "hl-agent-main-pe1-agcrashing-") {
			t.Fatalf("exit %d: the agent runs as %q, seen before = %v", exit, crashing, seen[crashing])
		}
		seen[crashing] = true
		c.rented.Start(crashing)
		c.rented.Exit(crashing, "Traceback", "SystemExit: the reason")
		c.pass()

		recorded := c.instance("ag-crashing")
		if recorded.ExitLog != "Traceback\nSystemExit: the reason" || recorded.ExitedAt == nil {
			t.Fatalf("exit %d: recorded %+v", exit, recorded)
		}
		if exit <= domain.MaxAgentInstanceRestarts {
			if recorded.Restarts != exit || recorded.Stopped || len(c.rented.Names()) != 2 {
				t.Fatalf("exit %d: recorded %+v with %v rented, want a restart", exit, recorded, c.rented.Names())
			}
			continue
		}
		if !recorded.Stopped || !reflect.DeepEqual(c.rented.Names(), []string{steady}) {
			t.Fatalf("after the last restart: recorded %+v with %v rented, want the agent stopped and its machine gone", recorded, c.rented.Names())
		}
	}
	for range 3 {
		c.pass()
	}
	if creates := c.rented.Calls("Create"); creates != 2+domain.MaxAgentInstanceRestarts {
		t.Fatalf("creates = %d, want the two first machines and %d restarts, ever", creates, domain.MaxAgentInstanceRestarts)
	}
	if !reflect.DeepEqual(c.rented.IDs(steady), steadyIDs) {
		t.Fatal("the agent that never exited was replaced")
	}
}

// A vendor that rents an instance for an hour at most ends a healthy agent's every hour. Each
// such end is the vendor's doing: the agent gets another machine every time, and its restarts
// stay for the crashes they are meant for.
func TestAnInstanceItsVendorsLifetimeEndsIsRentedAgainEveryTimeAndSpendsNoRestart(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	seen := map[string]bool{}
	for expiry := 1; expiry <= domain.MaxAgentInstanceRestarts+1; expiry++ {
		name := c.rented.Names()[0]
		if seen[name] {
			t.Fatalf("expiry %d: the agent runs as %q again, under which its expired machine would be kept", expiry, name)
		}
		seen[name] = true
		c.rented.Start(name)
		c.rented.Print(name, "hl-progress 1")
		c.pass()
		c.rented.Expire(name, "the last line before the lifetime ended")
		c.pass()

		recorded := c.instance("ag-one")
		if recorded.Restarts != 0 || recorded.Stopped || recorded.EndReason != string(db.InstanceExpired) || recorded.ExitLog != "hl-progress 1\nthe last line before the lifetime ended" {
			t.Fatalf("expiry %d: recorded %+v, want it expired with no restart spent", expiry, recorded)
		}
		if names := c.rented.Names(); len(names) != 1 || seen[names[0]] || c.rented.Calls("Create") != expiry+1 {
			t.Fatalf("expiry %d: rented %v with %d creates, want one new machine", expiry, names, c.rented.Calls("Create"))
		}
	}
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.pass()
	if recorded := c.instance("ag-one"); recorded.Reported != domain.InstanceRunning || recorded.Restarts != 0 {
		t.Fatalf("after %d expiries: recorded %+v, want the agent running with every restart left", domain.MaxAgentInstanceRestarts+1, recorded)
	}

	// A crash after them is the first restart, under a name no expired machine had.
	c.rented.Exit(name, "Traceback")
	c.pass()
	if recorded, names := c.instance("ag-one"), c.rented.Names(); recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceExited) || len(names) != 1 || seen[names[0]] || names[0] == name {
		t.Fatalf("a crash after the expiries: recorded %+v, rented %v", recorded, names)
	}
}

func TestAnAgentWhoseRunIsOverIsToldToStopAndItsNormalExitIsNoCrash(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	ids := c.rented.IDs(name)

	c.store.ByLauncher["main"][0].Ended = domain.RunEndClosed
	c.pass()
	c.pass()
	if c.rented.Stops(name) != 1 || !reflect.DeepEqual(c.rented.IDs(name), ids) {
		t.Fatalf("stops %d, instances %v (was %v)", c.rented.Stops(name), c.rented.IDs(name), ids)
	}
	c.rented.Exit(name, "last turn done")
	c.pass()
	if recorded := c.instance("ag-one"); recorded.Restarts != 0 || recorded.ExitedAt != nil || recorded.Name == "" {
		t.Fatalf("an ending agent's exit was recorded as a crash: %+v", recorded)
	}
	// Its last turn is over, so its machine goes before the wind-down ends, and nothing is
	// rented for it while the control plane still names it ending.
	if len(c.rented.Names()) != 0 || c.rented.Calls("Create") != 1 {
		t.Fatalf("after its exit: rented %v with %d creates, want it destroyed and none rented again", c.rented.Names(), c.rented.Calls("Create"))
	}
	c.pass()
	if recorded := c.instance("ag-one"); len(c.rented.Names()) != 0 || recorded.Restarts != 0 || recorded.ExitedAt != nil {
		t.Fatalf("a pass after it was destroyed: rented %v, recorded %+v", c.rented.Names(), recorded)
	}

	// Its wind-down has passed: the control plane no longer lists it.
	c.store.ByLauncher = nil
	c.pass()
	if len(c.rented.Names()) != 0 {
		t.Fatalf("still rented: %v", c.rented.Names())
	}
}

func TestARotatedCredentialReplacesEveryRunningInstance(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	before := c.rented.Names()[0]
	c.rented.Start(before)

	cfg := c.launcher.cfg
	cfg.Env = map[string]string{"MODEL_CREDENTIAL": "the-rotated-credential"}
	rotated, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotated.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := c.rented.Names()
	if len(names) != 1 || names[0] == before {
		t.Fatalf("rented %v after the rotation, want one instance under a new name (was %q)", names, before)
	}
	if got := c.rented.Spec(names[0]).Env["MODEL_CREDENTIAL"]; got != "the-rotated-credential" {
		t.Fatalf("the new instance runs with %q", got)
	}
	if strings.Contains(names[0], "rotated") {
		t.Fatal("the credential is in the instance name")
	}
}

func TestAHostedCoordinatorIsRentedRestartedAndEndedThroughTheSamePass(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.store.Coordinators = []db.LauncherCoordinator{{PlatformExperimentID: "pe-1", HostedCoordinator: domain.HostedCoordinator{Launcher: "main", Model: "model-2"}}}
	c.pass()
	names := c.rented.Names()
	if len(names) != 2 || !strings.HasPrefix(names[0], "hl-agent-main-pe1-agone-") || !strings.HasPrefix(names[1], "hl-agent-main-pe1-coordinator-") {
		t.Fatalf("rented %v, want the agent and the coordinator", names)
	}
	agent, coordinator := names[0], names[1]
	token, err := auth.Mint(contractSecret, auth.CoordinatorScope("pe-1"))
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{"API_URL": "https://api.example.invalid", "HL_TOKEN": token, "MODEL_CREDENTIAL": oauthToken}
	if spec := c.rented.Spec(coordinator); !reflect.DeepEqual(spec.Env, wantEnv) || strings.Contains(spec.Name, token) {
		t.Fatalf("coordinator's env = %v, want %v", spec.Env, wantEnv)
	}
	c.rented.Start(agent)
	agentIDs := c.rented.IDs(agent)

	// Its harness exits: the control plane asks for it again under a new name, and the agent's
	// machine is not touched.
	c.rented.Start(coordinator)
	c.rented.Exit(coordinator, "SystemExit: the reason")
	c.pass()
	recorded := c.instance("")
	replacement := c.rented.Names()[1]
	if recorded.Restarts != 1 || recorded.ExitLog != "SystemExit: the reason" || replacement == coordinator || !strings.HasPrefix(replacement, "hl-agent-main-pe1-coordinator-") {
		t.Fatalf("after an exit: recorded %+v, rented %v", recorded, c.rented.Names())
	}
	if !reflect.DeepEqual(c.rented.IDs(agent), agentIDs) {
		t.Fatal("the agent was replaced with the coordinator")
	}

	// The platform experiment closes: both are told to stop and left to finish.
	c.rented.Start(replacement)
	ids := c.rented.IDs(replacement)
	c.store.ByLauncher["main"][0].Ended = domain.RunEndClosed
	c.store.Coordinators[0].Ending = true
	c.pass()
	if c.rented.Stops(replacement) != 1 || c.rented.Stops(agent) != 1 || !reflect.DeepEqual(c.rented.IDs(replacement), ids) {
		t.Fatalf("stops: coordinator %d, agent %d", c.rented.Stops(replacement), c.rented.Stops(agent))
	}
	c.rented.Exit(replacement, "last turn done")
	c.pass()
	if after := c.instance(""); after.Restarts != 1 || c.rented.Calls("Create") != 3 {
		t.Fatalf("an ending coordinator's exit was counted or its machine rented again: %+v, %d creates", after, c.rented.Calls("Create"))
	}

	c.store.ByLauncher, c.store.Coordinators = nil, nil
	c.pass()
	if len(c.rented.Names()) != 0 {
		t.Fatalf("still rented: %v", c.rented.Names())
	}
}

func TestAMachineThatStaysStartingIsReplacedByTheControlPlanesDecisionAndCountedAsARestart(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-slow"))
	c.pass()
	for restart := 1; restart <= domain.MaxAgentInstanceRestarts+1; restart++ {
		name := c.rented.Names()[0]
		ids := c.rented.IDs(name)
		// First listed in this pass, which is when its time to start begins.
		c.pass()
		c.store.Advance(contractBootLimit - time.Second)
		c.pass()
		if !reflect.DeepEqual(c.rented.IDs(name), ids) {
			t.Fatalf("restart %d: a machine inside the boot limit was replaced", restart)
		}
		c.store.Advance(time.Second)
		c.pass()
		recorded := c.instance("ag-slow")
		if restart > domain.MaxAgentInstanceRestarts {
			if !recorded.Stopped || len(c.rented.Names()) != 0 {
				t.Fatalf("after the last restart: recorded %+v, rented %v, want the agent stopped and nothing rented", recorded, c.rented.Names())
			}
			break
		}
		if names := c.rented.Names(); len(names) != 1 || names[0] == name || recorded.Restarts != restart || recorded.Stopped {
			t.Fatalf("restart %d: rented %v (was %q), recorded %+v", restart, names, name, recorded)
		}
	}
	if creates := c.rented.Calls("Create"); creates != 1+domain.MaxAgentInstanceRestarts {
		t.Fatalf("creates = %d, want %d ever", creates, 1+domain.MaxAgentInstanceRestarts)
	}

	// A machine that came up is never held to the limit.
	running := newContract(t, hostedAgent("pe-1", "ag-up"))
	running.pass()
	running.rented.Start(running.rented.Names()[0])
	running.pass()
	running.store.Advance(2 * contractBootLimit)
	running.pass()
	if running.instance("ag-up").Restarts != 0 || running.rented.Calls("Create") != 1 {
		t.Fatal("a running machine was held to the boot limit")
	}
}

// A host its provider cannot reach for a pass costs a running agent nothing, long after its boot
// limit: the launcher says unknown and the control plane keeps the machine. A host that stays
// away for the progress limit is given up on, and the machine replaced.
func TestARunningAgentWhoseHostIsUnreachableIsKeptUntilTheProgressLimit(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	ids := c.rented.IDs(name)
	c.rented.Start(name)
	c.rented.Print(name, "hl-progress 1")
	c.pass()

	c.store.Advance(2 * contractBootLimit)
	c.rented.Lose(name)
	c.pass()
	if recorded := c.instance("ag-one"); recorded.Restarts != 0 || recorded.Reported != domain.InstanceRunning || !reflect.DeepEqual(c.rented.IDs(name), ids) {
		t.Fatalf("unreachable for a pass, past the boot limit: recorded %+v, rented %v, want the machine kept as running", recorded, c.rented.Names())
	}

	c.rented.Start(name)
	c.rented.Print(name, "hl-progress 2")
	c.pass()
	if recorded := c.instance("ag-one"); recorded.Restarts != 0 || c.rented.Calls("Create") != 1 {
		t.Fatalf("reachable again: recorded %+v with %d creates", recorded, c.rented.Calls("Create"))
	}

	c.rented.Lose(name)
	c.store.Advance(contractSettings.ProgressLimit - time.Second)
	c.pass()
	if !reflect.DeepEqual(c.rented.IDs(name), ids) {
		t.Fatal("a machine unreachable inside the progress limit was replaced")
	}
	c.store.Advance(time.Second)
	c.pass()
	names := c.rented.Names()
	if recorded := c.instance("ag-one"); len(names) != 1 || names[0] == name || recorded.Restarts != 1 || recorded.EndReason != string(db.InstanceSilent) {
		t.Fatalf("unreachable for the progress limit: rented %v (was %q), recorded %+v", names, name, recorded)
	}
}

// The launcher says nothing of a create that failed. The control plane sees a name it asked for
// and was never shown, and decides from that.
func TestACreateThatFailsOnEveryPassSpendsTheAgentsRestartsAndStopsIt(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-unrentable"), hostedAgent("pe-1", "ag-fine"))
	c.rented.Fail = func(method, name string) error {
		if method == "Create" && strings.Contains(name, "agunrentable") {
			return fmt.Errorf("vendor: HTTP 422: no such size, sent %s", oauthToken)
		}
		return nil
	}
	passes := 0
	pass := func() {
		// The agent whose machine came up keeps working through all of it.
		passes++
		for _, name := range c.rented.Names() {
			c.rented.Print(name, fmt.Sprintf("hl-progress %d", passes))
		}
		_ = c.launcher.Pass(context.Background())
	}
	pass()
	fine := c.rented.Names()
	if len(fine) != 1 || !strings.Contains(fine[0], "agfine") {
		t.Fatalf("rented %v, want the agent whose create works", fine)
	}
	c.rented.Start(fine[0])
	first := c.instance("ag-unrentable").Name

	pass()
	c.store.Advance(contractBootLimit - time.Second)
	pass()
	if recorded := c.instance("ag-unrentable"); recorded.Restarts != 0 || recorded.Name != first {
		t.Fatalf("inside the boot limit: recorded %+v", recorded)
	}
	c.store.Advance(time.Second)
	for restart := 1; restart <= domain.MaxAgentInstanceRestarts; restart++ {
		pass()
		recorded := c.instance("ag-unrentable")
		if recorded.Restarts != restart || recorded.Stopped || !strings.Contains(recorded.ExitLog, "no machine was listed") {
			t.Fatalf("restart %d: recorded %+v", restart, recorded)
		}
		c.store.Advance(contractBootLimit)
	}
	pass()
	if recorded := c.instance("ag-unrentable"); !recorded.Stopped {
		t.Fatalf("after the last restart: recorded %+v, want the agent stopped", recorded)
	}
	creates := c.rented.Calls("Create")
	c.store.Advance(contractBootLimit)
	pass()
	pass()
	if c.rented.Calls("Create") != creates {
		t.Fatal("a stopped agent's machine is still asked of the provider")
	}
	if !reflect.DeepEqual(c.rented.Names(), fine) || c.instance("ag-fine").Restarts != 0 {
		t.Fatalf("rented %v, want only %v and nothing spent on it", c.rented.Names(), fine)
	}

	// The operator fixes the size, which changes the launcher's digest: the agent is asked for
	// again with a whole restart budget, and the restarts spent under the old launch still counted.
	cfg := c.launcher.cfg
	cfg.Size.CPUMillis = 500
	c.rented.Fail = nil
	fixed, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixed.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorded := c.instance("ag-unrentable"); recorded.Stopped || recorded.Restarts != domain.MaxAgentInstanceRestarts || len(c.rented.Names()) != 2 {
		t.Fatalf("after the launcher's config changed: recorded %+v, rented %v", recorded, c.rented.Names())
	}
}

func TestAMachineThatRanAndIsNoLongerListedIsRentedAgainAndCountedAsARestart(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.pass()
	// The provider took the machine away: a host failed, or its lifetime ran out.
	if err := c.rented.Destroy(context.Background(), c.rented.IDs(name)[0]); err != nil {
		t.Fatal(err)
	}
	c.pass()
	names := c.rented.Names()
	recorded := c.instance("ag-one")
	if len(names) != 1 || names[0] == name || recorded.Restarts != 1 || !strings.Contains(recorded.ExitLog, "no longer lists it") {
		t.Fatalf("rented %v (was %q), recorded %+v", names, name, recorded)
	}
}

// The provider's list once left out two running machines for over a minute, and both
// agents were replaced while their machines ran on. A machine the platform holds is read by its
// id before it is reported missing.
func TestARunningMachineTheProvidersListLeavesOutIsKeptAndRunsOnce(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.pass()
	c.rented.Hide(name)
	for range 3 {
		c.pass()
	}
	recorded := c.instance("ag-one")
	if names := c.rented.Names(); len(names) != 1 || names[0] != name || recorded.Restarts != 0 || recorded.Name != name[len("hl-agent-main-"):] {
		t.Fatalf("rented %v (was %q), recorded %+v, want the one machine kept", names, name, recorded)
	}
	if c.rented.Calls("Create") != 1 || c.rented.Calls("Destroy") != 0 {
		t.Fatalf("creates %d destroys %d", c.rented.Calls("Create"), c.rented.Calls("Destroy"))
	}
	// Gone for real, which the read by id says: replaced as any machine the provider lost.
	c.rented.Show(name)
	if err := c.rented.Destroy(context.Background(), c.rented.IDs(name)[0]); err != nil {
		t.Fatal(err)
	}
	c.pass()
	if recorded := c.instance("ag-one"); recorded.Restarts != 1 || !strings.Contains(recorded.ExitLog, "no longer lists it") {
		t.Fatalf("recorded %+v, want the gone machine counted", recorded)
	}
}

// Creates are serial and paused, so a long list takes a pass longer than the boot limit. The
// control plane asks for a few new machines per answer, and none spends a restart waiting.
func TestAListLongerThanOnePassCanCreateIsRentedOverSeveralPassesAndSpendsNoRestart(t *testing.T) {
	settings := contractSettings
	settings.MaxNewPerAnswer = 2
	var agents []db.LauncherAgent
	for i := range 5 {
		agents = append(agents, hostedAgent("pe-1", fmt.Sprintf("ag-%d", i)))
	}
	c := newContractUnder(t, settings, agents...)
	var rented []int
	for range 4 {
		c.pass()
		for _, name := range c.rented.Names() {
			c.rented.Start(name)
		}
		rented = append(rented, len(c.rented.Names()))
		// Every pass takes the whole boot limit.
		c.store.Advance(contractBootLimit)
	}
	if !reflect.DeepEqual(rented, []int{2, 4, 5, 5}) {
		t.Fatalf("rented after each pass = %v, want two more each", rented)
	}
	for i := range 5 {
		if recorded := c.instance(fmt.Sprintf("ag-%d", i)); recorded.Restarts != 0 {
			t.Fatalf("ag-%d spent a restart waiting its turn: %+v", i, recorded)
		}
	}
	if c.rented.Calls("Create") != 5 || c.rented.Calls("Destroy") != 0 {
		t.Fatalf("creates %d, destroys %d", c.rented.Calls("Create"), c.rented.Calls("Destroy"))
	}
}

// The control plane refuses a launcher whose pass cannot fit its limits. That stops creation and
// nothing else: what the launcher holds is still ended by the lists, from its first refused pass,
// by a process that rented none of it.
func TestARefusedLauncherEndsWhatItHoldsByTheControlPlanesListsAndRentsNothing(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-kept"), hostedAgent("pe-1", "ag-over"), hostedAgent("pe-1", "ag-winding-down"))
	c.pass()
	names := c.rented.Names()
	if len(names) != 3 {
		t.Fatalf("rented %v", names)
	}
	kept, over, windingDown := names[0], names[1], names[2]
	for _, name := range names {
		c.rented.Start(name)
	}
	keptIDs := c.rented.IDs(kept)

	c.store.ByLauncher["main"][2].Ended = domain.RunEndClosed
	c.store.ByLauncher["main"] = append(c.store.ByLauncher["main"][:1], c.store.ByLauncher["main"][2], hostedAgent("pe-2", "ag-new"))
	cfg := c.launcher.cfg
	cfg.CreatePause = 4 * time.Second
	slow, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		err = slow.Pass(context.Background())
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "pass_does_not_fit") || !strings.Contains(err.Error(), "create_pause 4s") || !strings.Contains(err.Error(), "agents.boot_limit_seconds 5m0s") {
			t.Fatalf("refused pass %d = %v, want the control plane's refusal in its own words", pass, err)
		}
		if !reflect.DeepEqual(c.rented.Names(), []string{kept, windingDown}) || c.rented.Stops(windingDown) != 1 || !reflect.DeepEqual(c.rented.IDs(kept), keptIDs) {
			t.Fatalf("refused pass %d: rented %v with %d stops of the ending one, want %q destroyed, %q told to stop and %q untouched",
				pass, c.rented.Names(), c.rented.Stops(windingDown), over, windingDown, kept)
		}
	}
	if c.rented.Calls("Create") != 3 || c.store.Find("pe-2", "ag-new").Name != "" {
		t.Fatalf("a refused launcher rented or was asked for a new instance: %d creates, asked %q", c.rented.Calls("Create"), c.store.Find("pe-2", "ag-new").Name)
	}

	// Its wind-down passes while the launcher is still refused: the last machine of the run goes.
	c.store.ByLauncher["main"] = []db.LauncherAgent{c.store.ByLauncher["main"][0], c.store.ByLauncher["main"][2]}
	if err := slow.Pass(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("pass = %v", err)
	}
	if !reflect.DeepEqual(c.rented.Names(), []string{kept}) {
		t.Fatalf("rented %v, want only %q", c.rented.Names(), kept)
	}

	// The config is corrected: the agent that waited is rented.
	c.pass()
	if got := c.rented.Names(); len(got) != 2 || c.rented.Calls("Create") != 4 || c.store.Find("pe-2", "ag-new").Name == "" {
		t.Fatalf("once it fits: rented %v with %d creates", got, c.rented.Calls("Create"))
	}
}

// A launcher is taken out of service by giving its agents to none: its token works while it is
// registered, so its last passes end what it holds. Removing its row first would leave them.
func TestARegisteredLauncherNoAgentIsGivenToDestroysWhatItHoldsAndAnUnregisteredOneCanDoNothing(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"), hostedAgent("pe-1", "ag-two"))
	c.pass()
	if len(c.rented.Names()) != 2 {
		t.Fatalf("rented %v", c.rented.Names())
	}
	c.store.ByLauncher["main"] = c.store.ByLauncher["main"][:1]
	c.pass()
	if len(c.rented.Names()) != 1 {
		t.Fatalf("rented %v, want the one agent still listed", c.rented.Names())
	}

	c.registered.launchers["main"] = false
	c.store.ByLauncher = nil
	err := c.launcher.Pass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a pass of a launcher that is not registered: got = %v, want its token refused", err)
	}
	if len(c.rented.Names()) != 1 || c.rented.Calls("Destroy") != 1 {
		t.Fatalf("rented %v: a refused launcher changed what it holds", c.rented.Names())
	}

	c.registered.launchers["main"] = true
	c.pass()
	if len(c.rented.Names()) != 0 {
		t.Fatalf("still rented: %v, want nothing once the empty list reached the launcher", c.rented.Names())
	}
}

// The agent's harness prints a progress marker while it works. The launcher carries the newest
// one to the control plane, which alone decides that a machine whose marker stands still is hung.
func TestAHungAgentIsReplacedByTheControlPlanesDecisionAndAWorkingOneIsKept(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-hung"), hostedAgent("pe-1", "ag-working"))
	c.pass()
	names := c.rented.Names()
	hung, working := names[0], names[1]
	for _, name := range names {
		c.rented.Start(name)
		c.rented.Print(name, "hl-progress 1")
	}
	workingIDs := c.rented.IDs(working)
	c.pass()
	limit := contractSettings.ProgressLimit
	for step := 2; step <= 4; step++ {
		c.store.Advance(limit / 4)
		c.rented.Print(working, fmt.Sprintf("hl-progress %d", step))
		c.rented.Print(hung, "a line that is no progress")
		c.pass()
		if !reflect.DeepEqual(c.rented.Names(), names) {
			t.Fatalf("inside the progress limit: rented %v, want %v", c.rented.Names(), names)
		}
	}
	c.store.Advance(limit / 4)
	c.rented.Print(working, "hl-progress 5")
	c.pass()
	after := c.rented.Names()
	recorded := c.instance("ag-hung")
	if len(after) != 2 || after[0] == hung || recorded.Restarts != 1 || recorded.EndReason != "silent" {
		t.Fatalf("a limit after its last progress: rented %v (was %v), recorded %+v", after, names, recorded)
	}
	if !reflect.DeepEqual(c.rented.IDs(working), workingIDs) || c.instance("ag-working").Restarts != 0 {
		t.Fatal("the agent that kept making progress was replaced")
	}
	if c.instance("ag-working").ProgressAt == nil {
		t.Fatal("the working agent's progress was not recorded")
	}
}

// With the vendor's log down the launcher reads no marker and says so by leaving it out. The
// control plane holds such a machine to the progress limit as well, so a hung agent does not run
// on unseen, and counts no restart for it, so an outage however long stops no agent.
func TestAnAgentWhoseOutputCannotBeReadIsReplacedAtTheProgressLimitAndNeverStopped(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	limit := contractSettings.ProgressLimit
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.rented.Print(name, "hl-progress 1")
	c.pass()

	c.rented.Fail = func(method, _ string) error {
		if method == "Logs" {
			return errors.New("vendor: HTTP 503: logs are unavailable")
		}
		return nil
	}
	seen := map[string]bool{}
	for outage := 1; outage <= domain.MaxAgentInstanceRestarts+2; outage++ {
		seen[name] = true
		c.store.Advance(limit - time.Second)
		c.pass()
		if got := c.rented.Names(); len(got) != 1 || got[0] != name {
			t.Fatalf("outage %d: rented %v inside the progress limit, want %q kept", outage, got, name)
		}
		c.store.Advance(time.Second)
		c.pass()
		names, recorded := c.rented.Names(), c.instance("ag-one")
		if len(names) != 1 || seen[names[0]] || recorded.Restarts != 0 || recorded.Stopped || recorded.EndReason != string(db.InstanceProgressUnreadable) {
			t.Fatalf("outage %d: rented %v (was %q), recorded %+v, want a new machine and no restart spent", outage, names, name, recorded)
		}
		name = names[0]
		c.rented.Start(name)
		c.pass()
	}

	// The log is back and the agent works: it is kept.
	c.rented.Fail = nil
	creates := c.rented.Calls("Create")
	for step := 1; step <= 8; step++ {
		c.rented.Print(name, fmt.Sprintf("hl-progress %d", step))
		c.pass()
		c.store.Advance(limit / 4)
	}
	if recorded := c.instance("ag-one"); c.rented.Calls("Create") != creates || recorded.Restarts != 0 || recorded.Reported != domain.InstanceRunning {
		t.Fatalf("readable again and working: %d creates since, recorded %+v", c.rented.Calls("Create")-creates, recorded)
	}
}

// What an agent is doing reaches the control plane on the question the launcher already asks,
// and is kept as the latest sentence with the time it was first read: a pass that says the same
// writes nothing, the machine's own token never arrives, and a machine that ends says nothing.
func TestWhatAnAgentIsDoingIsKeptAsItsLatestSentenceUntilItsMachineEnds(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.rented.Print(name, "hl-progress 1", "hl-activity reading   train.py")
	c.pass()
	first, said := c.store.Activity["pe-1/ag-one"]
	if !said || first.Text != "reading train.py" || c.store.ActivityWrites != 1 {
		t.Fatalf("after its first sentence: kept %+v with %d writes, want it once, as one line", first, c.store.ActivityWrites)
	}

	c.store.Advance(time.Minute)
	c.rented.Print(name, "hl-progress 2", "hl-activity reading train.py")
	c.pass()
	if again := c.store.Activity["pe-1/ag-one"]; again != first || c.store.ActivityWrites != 1 {
		t.Fatalf("the same sentence a minute later: kept %+v with %d writes, want %+v and nothing written", again, c.store.ActivityWrites, first)
	}

	token, err := auth.Mint(contractSecret, auth.AgentScope("pe-1", "ag-one"))
	if err != nil {
		t.Fatal(err)
	}
	c.rented.Print(name, "hl-progress 3", "hl-activity running hl quota "+token)
	c.pass()
	if next := c.store.Activity["pe-1/ag-one"]; next.Text != "running hl quota <HL_TOKEN>" || !next.At.After(first.At) {
		t.Fatalf("its next sentence: kept %+v, want it without the machine's token and with its own time", next)
	}

	c.rented.Exit(name, "boom")
	c.pass()
	if left, said := c.store.Activity["pe-1/ag-one"]; said {
		t.Fatalf("after its machine exited: kept %+v, want nothing said of a machine that is gone", left)
	}
}

// The state line a harness prints reaches the run's health series on the question the launcher
// already asks, read by the control plane as the harness wrote it.
func TestWhatAHarnessSaysOfItselfReachesTheRunsHealth(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	c.rented.Print(name, "hl-progress 1", "hl-state subagents=4 turn=waiting since=1759990000 instructions_digest=0123456789ab model_status=allowed_warning model_type=five_hour model_utilization=0.8 model_resets_at=1760000000")
	c.pass()
	utilization, resets := 0.8, time.Unix(1760000000, 0).UTC()
	want := []domain.HarnessState{{PlatformExperimentID: "pe-1", AgentID: "ag-one", Subagents: 4, TurnWaiting: true, TurnSince: time.Unix(1759990000, 0).UTC(), InstructionsDigest: "0123456789ab",
		Model: domain.RateLimit{Utilization: &utilization, ResetsAt: &resets}}}
	if got := c.metrics.Written(); !reflect.DeepEqual(got, want) {
		t.Fatalf("written %+v, want %+v", got, want)
	}
}
