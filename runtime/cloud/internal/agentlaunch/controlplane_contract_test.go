package agentlaunch

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/launcherapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

// The launcher and the control plane's handler are written against each other's JSON. These
// tests run the launcher's pass against the handler itself, over HTTP, so a field renamed on
// either side fails here instead of in a deployment.

const contractSecret = "a-signing-secret-only-this-test-knows"

// signups holds hosted signups as the database does, with the store's own conditions on the two
// writes the handler makes.
type signups struct {
	agents []db.LauncherAgent
	// askedAgo is how long ago each agent's current instance name was first asked for.
	askedAgo map[string]time.Duration
}

const contractBootLimit = 5 * time.Minute

func (s *signups) RecordAgentInstanceStuck(ctx context.Context, pe, agentID, name string, bootLimit time.Duration, maxRestarts int) (bool, error) {
	if s.askedAgo[agentID] <= bootLimit {
		return false, nil
	}
	return s.RecordAgentInstanceExit(ctx, pe, agentID, name, "still starting past the boot limit", maxRestarts)
}

func (s *signups) ListLauncherAgents(_ context.Context, launcher string, _ time.Duration) ([]db.LauncherAgent, error) {
	listed := []db.LauncherAgent{}
	for _, agent := range s.agents {
		if agent.Launcher != launcher || agent.Instance.Stopped {
			continue
		}
		instance := *agent.Instance
		agent.Instance = &instance
		listed = append(listed, agent)
	}
	return listed, nil
}

func (s *signups) instance(agentID string) *domain.AgentInstance {
	for i := range s.agents {
		if s.agents[i].AgentID == agentID {
			return s.agents[i].Instance
		}
	}
	panic("no agent " + agentID)
}

func (s *signups) SetAgentInstanceName(_ context.Context, _, agentID, name string) error {
	s.instance(agentID).Name = name
	delete(s.askedAgo, agentID)
	return nil
}

func (s *signups) RecordAgentInstanceExit(_ context.Context, _, agentID, name, exitLog string, maxRestarts int) (bool, error) {
	instance := s.instance(agentID)
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

func hostedAgent(pe, agent string) db.LauncherAgent {
	return db.LauncherAgent{AgentSignup: domain.AgentSignup{
		PlatformExperimentID: pe, AgentID: agent, Hosted: true, Launcher: "main",
		Flavor: "data-search", Model: "model-1", Hyperparameters: map[string]any{"batch_size": float64(64)},
		Instance: &domain.AgentInstance{},
	}}
}

// onlyTheLauncher admits the launcher's own token as that launcher and nothing else.
type onlyTheLauncher struct{}

func (onlyTheLauncher) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	if token != launcherToken {
		return auth.Principal{}, errors.New("not the launcher's token")
	}
	return auth.Principal{Kind: auth.KindLauncher, LauncherName: "main"}, nil
}

type contract struct {
	t        *testing.T
	store    *signups
	rented   *fake.Provider
	launcher *Launcher
	log      []string
}

func newContract(t *testing.T, agents ...db.LauncherAgent) *contract {
	t.Helper()
	c := &contract{t: t, store: &signups{agents: agents, askedAgo: map[string]time.Duration{}}, rented: fake.New()}
	launchers := chi.NewRouter()
	doc := apidocs.New(launchers, "launcher", "1.0.0", "", onlyTheLauncher{}, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	launcherapi.RegisterHuma(doc, launcherapi.NewHandler(c.store, "registry.example.invalid/agent:1", "https://api.example.invalid", contractSecret, contractBootLimit, zap.NewNop()))
	root := chi.NewRouter()
	root.Mount("/internal/launchers", launchers)
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)

	launcher, err := New(Config{
		Name: "main", APIURL: server.URL, Token: launcherToken, Namespace: "ns",
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

func TestTheLaunchersPassAgainstTheControlPlanesOwnHandler(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"), hostedAgent("pe-1", "ag-two"))
	c.pass()
	names := c.rented.Names()
	if len(names) != 2 || !strings.HasPrefix(names[0], "hl-agent-ns-pe1-agone-") || !strings.HasPrefix(names[1], "hl-agent-ns-pe1-agtwo-") {
		t.Fatalf("rented %v", names)
	}
	spec := c.rented.Spec(names[0])
	token, err := auth.Mint(contractSecret, auth.AgentScope("pe-1", "ag-one"))
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		"API_URL": "https://api.example.invalid", "AGENT_ID": "ag-one", "PLATFORM_EXPERIMENT_ID": "pe-1",
		"AGENT_FLAVOR": "data-search", "AGENT_MODEL": "model-1", "AGENT_HYPERPARAMETERS": `{"batch_size":64}`,
		"HL_TOKEN": token, "MODEL_CREDENTIAL": oauthToken,
	}
	if spec.Image != "registry.example.invalid/agent:1" || !reflect.DeepEqual(spec.Env, wantEnv) || !reflect.DeepEqual(spec.Command, []string{"run-agent"}) {
		t.Fatalf("spec = %+v\nwant env %v", spec, wantEnv)
	}
	if got := c.store.instance("ag-one").Name; "hl-agent-ns-"+got != names[0] {
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

func TestAnExitTravelsToTheControlPlaneWhichRestartsTheAgentAFewTimesAndThenStopsIt(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-crashing"), hostedAgent("pe-1", "ag-steady"))
	c.pass()
	steady := c.rented.Names()[1]
	c.rented.Start(steady)
	steadyIDs := c.rented.IDs(steady)

	seen := map[string]bool{}
	for exit := 1; exit <= domain.MaxAgentInstanceRestarts+1; exit++ {
		crashing := c.rented.Names()[0]
		if seen[crashing] || !strings.HasPrefix(crashing, "hl-agent-ns-pe1-agcrashing-") {
			t.Fatalf("exit %d: the agent runs as %q, seen before = %v", exit, crashing, seen[crashing])
		}
		seen[crashing] = true
		c.rented.Start(crashing)
		c.rented.Exit(crashing, "Traceback", "SystemExit: the reason")
		c.pass()

		recorded := c.store.instance("ag-crashing")
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

func TestAnAgentWhoseRunIsOverIsToldToStopAndItsNormalExitIsNoCrash(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-one"))
	c.pass()
	name := c.rented.Names()[0]
	c.rented.Start(name)
	ids := c.rented.IDs(name)

	c.store.agents[0].Ending = true
	c.pass()
	c.pass()
	if c.rented.Stops(name) != 2 || !reflect.DeepEqual(c.rented.IDs(name), ids) {
		t.Fatalf("stops %d, instances %v (was %v)", c.rented.Stops(name), c.rented.IDs(name), ids)
	}
	c.rented.Exit(name, "last turn done")
	c.pass()
	if recorded := c.store.instance("ag-one"); recorded.Restarts != 0 || recorded.ExitedAt != nil || recorded.Name == "" {
		t.Fatalf("an ending agent's exit was recorded as a crash: %+v", recorded)
	}
	if !reflect.DeepEqual(c.rented.IDs(name), ids) || c.rented.Calls("Create") != 1 {
		t.Fatal("an ending instance was destroyed or rented again")
	}

	// Its wind-down has passed: the control plane no longer lists it.
	c.store.agents = nil
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

func TestAMachineThatNeverStartsIsReplacedByTheControlPlanesDecisionAndCountedAsARestart(t *testing.T) {
	c := newContract(t, hostedAgent("pe-1", "ag-slow"))
	c.pass()
	first := c.rented.Names()[0]
	firstIDs := c.rented.IDs(first)

	c.store.askedAgo["ag-slow"] = contractBootLimit - time.Second
	c.pass()
	if !reflect.DeepEqual(c.rented.IDs(first), firstIDs) {
		t.Fatal("a machine inside the boot limit was replaced")
	}

	for restart := 1; restart <= domain.MaxAgentInstanceRestarts; restart++ {
		before := c.rented.Names()[0]
		c.store.askedAgo["ag-slow"] = contractBootLimit + time.Second
		c.pass()
		names := c.rented.Names()
		if len(names) != 1 || names[0] == before {
			t.Fatalf("restart %d: rented %v, want one machine under a new name (was %q)", restart, names, before)
		}
		if recorded := c.store.instance("ag-slow"); recorded.Restarts != restart || recorded.Stopped {
			t.Fatalf("restart %d: recorded %+v", restart, recorded)
		}
	}
	c.store.askedAgo["ag-slow"] = contractBootLimit + time.Second
	c.pass()
	if recorded := c.store.instance("ag-slow"); !recorded.Stopped || len(c.rented.Names()) != 0 {
		t.Fatalf("after the last restart: recorded %+v, rented %v, want the agent stopped and nothing rented", recorded, c.rented.Names())
	}
	if creates := c.rented.Calls("Create"); creates != 1+domain.MaxAgentInstanceRestarts {
		t.Fatalf("creates = %d, want %d ever", creates, 1+domain.MaxAgentInstanceRestarts)
	}

	// A machine that came up is never held to the limit.
	running := newContract(t, hostedAgent("pe-1", "ag-up"))
	running.pass()
	name := running.rented.Names()[0]
	running.rented.Start(name)
	running.store.askedAgo["ag-up"] = 24 * time.Hour
	running.pass()
	if running.store.instance("ag-up").Restarts != 0 || running.rented.Calls("Create") != 1 {
		t.Fatal("a running machine was held to the boot limit")
	}
}
