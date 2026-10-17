package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// sandboxedThing is a sandboxed agent or, with no agent id, a sandboxed coordinator, and how each is read
// back and aged.
type sandboxedThing struct {
	pe, agentID string
	instance    func() domain.AgentInstance
	age         func(column string, by time.Duration)
}

func sandboxedThings(t *testing.T, pool *Pool) map[string]sandboxedThing {
	t.Helper()
	ctx := context.Background()
	pes := NewStore(pool, 3)
	age := func(pe, agentID string) func(string, time.Duration) {
		return func(column string, by time.Duration) {
			t.Helper()
			if _, err := pool.pool.Exec(ctx, `UPDATE agentsandbox_instances SET `+column+` = now() - make_interval(secs => $1) WHERE platform_experiment_id = $2 AND agent_id = $3`, by.Seconds(), pe, agentID); err != nil {
				t.Fatal(err)
			}
		}
	}
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	agent := spawnTestAgents(t, pes, pe, domain.AgentSignup{Actor: testActor, InAgentSandbox: true})[0]
	coordinated := createTestPEWithCoordinator(t, pool)
	setPEStatus(t, pool, coordinated, domain.PlatformExpRunning, 0)
	return map[string]sandboxedThing{
		"agent": {pe, agent, func() domain.AgentInstance {
			t.Helper()
			signup, err := pes.GetAgentSignup(ctx, pe, agent)
			if err != nil || signup == nil || signup.Instance == nil {
				t.Fatalf("signup: got = %+v, %v", signup, err)
			}
			return *signup.Instance
		}, age(pe, agent)},
		"coordinator": {coordinated, "", func() domain.AgentInstance {
			t.Helper()
			got, err := pes.GetPlatformExperiment(ctx, coordinated)
			if err != nil || got == nil || got.Coordinator == nil {
				t.Fatalf("platform experiment: got = %+v, %v", got, err)
			}
			return got.Coordinator.Instance
		}, age(coordinated, "")},
	}
}

func TestAnExitedMachineIsRecordedRestartedAndFinallyStopsWhatItRan(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const maxRestarts = 2
	for kind, thing := range sandboxedThings(t, pool) {
		if got := thing.instance(); got != (domain.AgentInstance{}) {
			t.Fatalf("%s: a fresh instance: got = %+v, want nothing recorded", kind, got)
		}
		for _, name := range []string{"name-0", ""} {
			if _, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceExited, "boom", time.Minute, maxRestarts); err != nil || recorded {
				t.Fatalf("%s: an exit under %q, which was never asked for: got = %v, %v, want = false", kind, name, recorded, err)
			}
		}
		for exit := 1; exit <= maxRestarts+1; exit++ {
			name := "name-" + string(rune('0'+exit))
			if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
			if got := thing.instance(); got.Name != name || got.Launch != "launch-1" || got.SeenAt != nil {
				t.Fatalf("%s: asked for: got = %+v, want %q unseen under launch-1", kind, got, name)
			}
			if _, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, "another-name", InstanceExited, "boom", time.Minute, maxRestarts); err != nil || recorded {
				t.Fatalf("%s: exit %d under a stale name: got = %v, %v, want = false", kind, exit, recorded, err)
			}
			after, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceExited, "log of "+name, time.Minute, maxRestarts)
			if err != nil || !recorded {
				t.Fatalf("%s: exit %d: got = %v, %v, want = true", kind, exit, recorded, err)
			}
			if _, again, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceExited, "said twice", time.Minute, maxRestarts); err != nil || again {
				t.Fatalf("%s: exit %d reported again: got = %v, %v, want = false", kind, exit, again, err)
			}
			wantRestarts, wantStopped := exit, false
			if exit > maxRestarts {
				wantRestarts, wantStopped = maxRestarts, true
			}
			got := thing.instance()
			if got.Name != "" || got.Restarts != wantRestarts || got.Stopped != wantStopped || got.ExitLog != "log of "+name ||
				got.ExitedAt == nil || time.Since(*got.ExitedAt) > time.Minute {
				t.Fatalf("%s: after exit %d: got = %+v, want restarts %d stopped %v", kind, exit, got, wantRestarts, wantStopped)
			}
			// What the write returns is what it left, which a caller decides from.
			if after.Restarts != wantRestarts || after.Stopped != wantStopped || after.ExitLog != got.ExitLog {
				t.Fatalf("%s: exit %d returned %+v, stored %+v", kind, exit, after, got)
			}
		}

		// Stopped under one launch, it is asked for again under another with its budget whole.
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "same-launch", "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got := thing.instance(); !got.Stopped || got.Restarts != maxRestarts {
			t.Fatalf("%s: asked for again under the launch it was stopped under: got = %+v, want it still stopped", kind, got)
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "new-launch", "launch-2"); err != nil {
			t.Fatal(err)
		}
		if got := thing.instance(); got.Stopped || got.Restarts != maxRestarts || got.Name != "new-launch" || got.Launch != "launch-2" {
			t.Fatalf("%s: asked for under another launch: got = %+v, want a fresh budget and the restarts counted still", kind, got)
		}
	}
}

// Restarts are a rate: those spent longer ago than the window no longer count, so a long run
// that exits now and then keeps being restarted, and only a burst of exits stops it.
func TestRestartsOlderThanTheWindowNoLongerCount(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const maxRestarts = 2
	for kind, thing := range sandboxedThings(t, pool) {
		exit := func(name string) domain.AgentInstance {
			t.Helper()
			if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
			after, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceExited, "boom", time.Minute, maxRestarts)
			if err != nil || !recorded {
				t.Fatalf("%s: exit of %s: got = %v, %v, want = true", kind, name, recorded, err)
			}
			return after
		}
		for i := range maxRestarts {
			exit(fmt.Sprintf("early-%d", i))
		}
		if _, err := pool.pool.Exec(ctx, `UPDATE agentsandbox_instances
SET restarted_at = ARRAY(SELECT t - make_interval(secs => $1) FROM unnest(restarted_at) t)
WHERE platform_experiment_id = $2 AND agent_id = $3`, (domain.AgentInstanceRestartWindow + time.Minute).Seconds(), thing.pe, thing.agentID); err != nil {
			t.Fatal(err)
		}
		for i := range maxRestarts {
			if got := exit(fmt.Sprintf("late-%d", i)); got.Stopped || got.Restarts != maxRestarts+i+1 {
				t.Fatalf("%s: exit %d after the window: got = %+v, want restarted, %d restarts ever", kind, i, got, maxRestarts+i+1)
			}
		}
		if got := exit("burst"); !got.Stopped || got.Restarts != 2*maxRestarts {
			t.Fatalf("%s: one exit past the rate: got = %+v, want stopped", kind, got)
		}
	}
}

// A machine its provider ended at the provider's own lifetime is no fault of what it ran: it is
// counted as a renewal, however many came before, and neither spends a restart nor stops anything.
func TestAnExpiredMachineSpendsNoRestartAndNeverStopsWhatItRan(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const maxRestarts = 2
	for kind, thing := range sandboxedThings(t, pool) {
		end := func(name string, how InstanceEnd) domain.AgentInstance {
			t.Helper()
			if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
			after, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, how, "log of "+name, time.Minute, maxRestarts)
			if err != nil || !recorded {
				t.Fatalf("%s: %s of %s: got = %v, %v, want = true", kind, how, name, recorded, err)
			}
			if _, again, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, how, "said twice", time.Minute, maxRestarts); err != nil || again {
				t.Fatalf("%s: %s of %s reported again: got = %v, %v, want = false", kind, how, name, again, err)
			}
			got := thing.instance()
			if after.Restarts != got.Restarts || after.Renewals != got.Renewals || after.Stopped != got.Stopped || after.EndReason != got.EndReason {
				t.Fatalf("%s: %s of %s returned %+v, stored %+v", kind, how, name, after, got)
			}
			return got
		}
		for expiry := 1; expiry <= maxRestarts+2; expiry++ {
			got := end(fmt.Sprintf("expired-%d", expiry), InstanceExpired)
			if got.Name != "" || got.Restarts != 0 || got.Renewals != expiry || got.Stopped || got.EndReason != "expired" || got.ExitLog != fmt.Sprintf("log of expired-%d", expiry) || got.ExitedAt == nil {
				t.Fatalf("%s: after expiry %d: got = %+v, want a renewal and no restart", kind, expiry, got)
			}
		}
		// With every restart spent, one more expiry still stops nothing, and the next exit does.
		for exit := 1; exit <= maxRestarts; exit++ {
			if got := end(fmt.Sprintf("exited-%d", exit), InstanceExited); got.Restarts != exit || got.Renewals != maxRestarts+2 || got.Stopped {
				t.Fatalf("%s: after exit %d: got = %+v", kind, exit, got)
			}
		}
		if got := end("expired-last", InstanceExpired); got.Restarts != maxRestarts || got.Renewals != maxRestarts+3 || got.Stopped {
			t.Fatalf("%s: an expiry with no restart left: got = %+v, want it asked for again", kind, got)
		}
		if got := end("exited-last", InstanceExited); !got.Stopped || got.Restarts != maxRestarts || got.EndReason != "exited" {
			t.Fatalf("%s: an exit with no restart left: got = %+v, want it stopped", kind, got)
		}
	}
}

func TestAMachineIsGivenUpOnWhenItIsNeverSeenStaysStartingOrIsGone(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const bootLimit, maxRestarts = 5 * time.Minute, 3
	for kind, thing := range sandboxedThings(t, pool) {
		ask := func(name string) {
			t.Helper()
			if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
		}
		end := func(name string, how InstanceEnd) bool {
			t.Helper()
			_, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, how, "why", bootLimit, maxRestarts)
			if err != nil {
				t.Fatal(err)
			}
			return recorded
		}

		// Never seen: only once the boot limit has passed since it was asked for.
		ask("never-seen")
		thing.age("asked_at", bootLimit-10*time.Second)
		if end("never-seen", InstanceNeverSeen) || end("never-seen", InstanceGone) || end("never-seen", InstanceStillStarting) {
			t.Fatalf("%s: an unseen machine inside the limit was given up on", kind)
		}
		thing.age("asked_at", bootLimit+time.Second)
		if end("never-seen", InstanceGone) || end("never-seen", InstanceStillStarting) {
			t.Fatalf("%s: an unseen machine was recorded as gone or as starting", kind)
		}
		if !end("never-seen", InstanceNeverSeen) || thing.instance().Restarts != 1 {
			t.Fatalf("%s: an unseen machine past the limit: got = %+v, want one restart spent", kind, thing.instance())
		}

		// Seen: the limit of a machine that stays starting is counted from then, however long
		// ago it was asked for.
		ask("starting")
		thing.age("asked_at", 10*bootLimit)
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "another-name", "starting", nil, time.Hour, "", "machine-1"); err != nil {
			t.Fatal(err)
		}
		if thing.instance().SeenAt != nil {
			t.Fatalf("%s: seeing another name marked this one seen", kind)
		}
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "starting", "starting", nil, time.Hour, "", "machine-1"); err != nil {
			t.Fatal(err)
		}
		first := thing.instance().SeenAt
		if first == nil || end("starting", InstanceNeverSeen) || end("starting", InstanceStillStarting) {
			t.Fatalf("%s: a machine just seen: got = %+v, want it kept", kind, thing.instance())
		}
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "starting", "starting", nil, time.Hour, "", "machine-1"); err != nil || !thing.instance().SeenAt.Equal(*first) {
			t.Fatalf("%s: seen again moved the first sight: %v", kind, err)
		}
		thing.age("seen_at", bootLimit+time.Second)
		if !end("starting", InstanceStillStarting) || thing.instance().Restarts != 2 {
			t.Fatalf("%s: starting past the limit: got = %+v, want a second restart spent", kind, thing.instance())
		}

		// Seen and gone is an exit at once. Asked for again, the sight of the last name is gone.
		ask("gone")
		if thing.instance().SeenAt != nil {
			t.Fatalf("%s: a new name kept the last one's sight", kind)
		}
		if end("gone", InstanceGone) {
			t.Fatalf("%s: a machine never seen was recorded as gone", kind)
		}
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "gone", "starting", nil, time.Hour, "", "machine-1"); err != nil {
			t.Fatal(err)
		}
		if !end("gone", InstanceGone) || thing.instance().Restarts != 3 || thing.instance().ExitLog != "why" {
			t.Fatalf("%s: seen and gone: got = %+v, want a third restart spent", kind, thing.instance())
		}
	}
}

// The machine a held name was last reported by is answered until the name is given up on or
// asked for anew, and a report without one is refused.
func TestALauncherIsToldTheMachineOfEveryNameItHolds(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	for kind, thing := range sandboxedThings(t, pool) {
		name := "held-" + uuid.New().String()[:8]
		machineOf := func(names ...string) string {
			t.Helper()
			if names == nil {
				names = []string{name}
			}
			held, err := pes.ListLauncherMachines(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range held {
				if m.Name == names[0] {
					return m.Machine
				}
			}
			return ""
		}
		observe := func(machine string) error {
			return pes.ObserveInstance(ctx, thing.pe, thing.agentID, name, domain.InstanceRunning, nil, time.Hour, "", machine)
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got := machineOf(); got != "" {
			t.Fatalf("%s: a name never reported is held by %q", kind, got)
		}
		if err := observe(""); err == nil {
			t.Fatalf("%s: a report with no machine id was taken", kind)
		}
		if err := observe("m-1"); err != nil || machineOf() != "m-1" {
			t.Fatalf("%s: reported by m-1: %v, held by %q", kind, err, machineOf())
		}
		if err := observe("m-2"); err != nil || machineOf() != "m-2" {
			t.Fatalf("%s: reported again by m-2, same state: %v, held by %q", kind, err, machineOf())
		}
		if _, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceGone, "why", time.Minute, 3); err != nil || !recorded {
			t.Fatalf("%s: end: %v %v", kind, recorded, err)
		}
		if got := machineOf(); got != "" {
			t.Fatalf("%s: a name given up on is still held by %q", kind, got)
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
			t.Fatal(err)
		}
		if err := observe("m-3"); err != nil {
			t.Fatal(err)
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name+"-next", "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got := machineOf(name + "-next"); got != "" {
			t.Fatalf("%s: a name asked for anew answers the last one's machine %q", kind, got)
		}
	}
}

// Two reconciles of one launcher can overlap, and both report the exit they both saw.
func TestOneExitReportedByTwoReconcilesAtOnceSpendsOneRestart(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	for kind, thing := range sandboxedThings(t, pool) {
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "name-1", "launch-1"); err != nil {
			t.Fatal(err)
		}
		const reporters = 8
		recorded := make([]bool, reporters)
		errs := make([]error, reporters)
		var wg sync.WaitGroup
		for i := range reporters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, recorded[i], errs[i] = pes.EndInstance(ctx, thing.pe, thing.agentID, "name-1", InstanceExited, "boom", time.Minute, 3)
			}()
		}
		wg.Wait()
		count := 0
		for i := range reporters {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if recorded[i] {
				count++
			}
		}
		if count != 1 || thing.instance().Restarts != 1 {
			t.Fatalf("%s: %d of %d reports were recorded and %d restarts spent, want one of each", kind, count, reporters, thing.instance().Restarts)
		}
	}
}

// The bound is on machines, so a sandboxed baseline or reviewer counts as a competitor does, and so
// does the coordinator.
func TestSandboxedAgentsOfEveryRoleAndTheCoordinatorCountAgainstTheMachinesOfOneExperiment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "bounded", BudgetAcceleratorHours: 10,
		MaxAgents: 2, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now, MaxAgentSandboxes: 4,
		CoordinatorInAgentSandbox: true, Coordinator: &domain.AgentSandboxCoordinator{Model: "model-1"},
	}
	if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
		t.Fatal(err)
	}
	add := func(signup domain.AgentSignup) error {
		id, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			t.Fatal(err)
		}
		signup.AgentID = id
		agent := &domain.Agent{ID: id, Name: id, CreatedAt: now}
		return pes.SpawnAgent(ctx, pe.ID, agent, signup)
	}
	// max_agents is two competitors, which no baseline or reviewer counts against. Added one at
	// a time they would be without end, were machines not counted.
	for i, role := range []domain.SignupRole{domain.SignupRoleBaseline, domain.SignupRoleReviewer, domain.SignupRoleBaseline} {
		if err := add(domain.AgentSignup{Actor: testActor, Role: role, InAgentSandbox: true}); err != nil {
			t.Fatalf("sandboxed %s %d: got = %v, want = nil", role, i+1, err)
		}
	}
	for _, role := range []domain.SignupRole{domain.SignupRoleBaseline, domain.SignupRoleReviewer, domain.SignupRoleCompetitor} {
		if err := add(domain.AgentSignup{Actor: testActor, Role: role, InAgentSandbox: true}); !errors.Is(err, ErrMaxAgentSandboxesReached) {
			t.Fatalf("a fifth machine, a sandboxed %s: got = %v, want ErrMaxAgentSandboxesReached", role, err)
		}
	}
	// An agent its owner runs is no machine of the platform's.
	if err := add(domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleCompetitor}); err != nil {
		t.Fatalf("a self-run agent in a full house: got = %v, want = nil", err)
	}
	if joined, err := pes.ListAgentSignups(ctx, pe.ID); err != nil || len(joined) != 4 {
		t.Fatalf("joined = %d, %v, want the three sandboxed and the self-run one", len(joined), err)
	}
}

// Nothing starts without a baseline, so one machine is the baseline's: the agents that do not
// measure it leave it free, and one that finished without a usable baseline gives it back.
func TestOneMachineIsKeptForTheBaselineAndAFinishedAgentHoldsNone(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "kept", BudgetAcceleratorHours: 10,
		MaxAgents: 5, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now, MaxAgentSandboxes: 4,
		CoordinatorInAgentSandbox: true, Coordinator: &domain.AgentSandboxCoordinator{Model: "model-1"},
	}
	if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
		t.Fatal(err)
	}
	add := func(role domain.SignupRole) (string, error) {
		id, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			t.Fatal(err)
		}
		agent := &domain.Agent{ID: id, Name: id, CreatedAt: now}
		return id, pes.SpawnAgent(ctx, pe.ID, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role, InAgentSandbox: true})
	}
	for i := range 2 {
		if _, err := add(domain.SignupRoleCompetitor); err != nil {
			t.Fatalf("researcher %d beside the coordinator: got = %v, want = nil", i+1, err)
		}
	}
	for _, role := range []domain.SignupRole{domain.SignupRoleCompetitor, domain.SignupRoleReviewer} {
		if _, err := add(role); !errors.Is(err, ErrMaxAgentSandboxesReached) {
			t.Fatalf("a sandboxed %s for the machine kept for the baseline: got = %v, want ErrMaxAgentSandboxesReached", role, err)
		}
	}
	first, err := add(domain.SignupRoleBaseline)
	if err != nil {
		t.Fatalf("the baseline agent in the machine kept for it: got = %v, want = nil", err)
	}
	if _, err := add(domain.SignupRoleBaseline); !errors.Is(err, ErrMaxAgentSandboxesReached) {
		t.Fatalf("a second baseline agent while the first holds the machine: got = %v, want ErrMaxAgentSandboxesReached", err)
	}
	if err := pes.FinishAgent(ctx, pe.ID, first, "the control does not run"); err != nil {
		t.Fatal(err)
	}
	if _, err := add(domain.SignupRoleBaseline); err != nil {
		t.Fatalf("a second baseline agent once the first finished: got = %v, want = nil", err)
	}
	if _, err := add(domain.SignupRoleCompetitor); !errors.Is(err, ErrMaxAgentSandboxesReached) {
		t.Fatalf("a researcher in a full house: got = %v, want ErrMaxAgentSandboxesReached", err)
	}
}

func TestARunningMachinesProgressIsStampedOnThisClockAndItsSilenceEndsIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const limit, maxRestarts = 8 * time.Minute, 3
	marker := func(m string) *string { return &m }
	for kind, thing := range sandboxedThings(t, pool) {
		observe := func(state string, progress *string) {
			t.Helper()
			if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "name-1", state, progress, limit, "", "machine-1"); err != nil {
				t.Fatal(err)
			}
		}
		silent := func() bool {
			t.Helper()
			_, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, "name-1", InstanceSilent, "no progress", limit, maxRestarts)
			if err != nil {
				t.Fatal(err)
			}
			return recorded
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "name-1", "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := json.Marshal(thing.instance()); !strings.Contains(string(got), `"state":"requested"`) {
			t.Fatalf("%s: asked for and not listed: %s", kind, got)
		}

		// Starting, however long, is the boot limit's to judge.
		observe("starting", nil)
		thing.age("seen_at", 10*limit)
		if got := thing.instance(); got.Shown() != "starting" || got.ProgressAt != nil || silent() {
			t.Fatalf("%s: starting: got = %+v, want no progress and no silence", kind, got)
		}

		// Running and never a marker: the limit runs from the first sight.
		observe("running", marker(""))
		if got := thing.instance(); got.Shown() != "running" || got.ProgressAt != nil {
			t.Fatalf("%s: running with no marker: got = %+v", kind, got)
		}
		thing.age("seen_at", limit-10*time.Second)
		if silent() {
			t.Fatalf("%s: a machine inside the limit of its first sight was ended as silent", kind)
		}

		// A marker is stamped when it is first seen, and a report that could not read one
		// changes nothing.
		observe("running", marker("7"))
		first := thing.instance().ProgressAt
		if first == nil || time.Since(*first) > time.Minute {
			t.Fatalf("%s: first marker: progress_at = %v, want now", kind, first)
		}
		observe("running", nil)
		observe("running", marker("7"))
		if got := thing.instance().ProgressAt; !got.Equal(*first) {
			t.Fatalf("%s: the same marker moved progress_at from %v to %v", kind, first, got)
		}

		// A new marker is written down at most four times a limit.
		thing.age("progress_at", limit/4-10*time.Second)
		aged := thing.instance().ProgressAt
		observe("running", marker("8"))
		if got := thing.instance().ProgressAt; !got.Equal(*aged) {
			t.Fatalf("%s: a marker inside a quarter of the limit was written down", kind)
		}
		thing.age("progress_at", limit/4+time.Second)
		observe("running", marker("8"))
		if got := thing.instance().ProgressAt; time.Since(*got) > time.Minute {
			t.Fatalf("%s: a new marker a quarter of the limit later: progress_at = %v, want now", kind, got)
		}

		// The marker stops changing: nothing inside the limit, an end past it.
		thing.age("progress_at", limit-10*time.Second)
		observe("running", marker("8"))
		if silent() {
			t.Fatalf("%s: ended as silent inside the limit", kind)
		}
		thing.age("progress_at", limit+time.Second)
		observe("running", marker("8"))
		if !silent() {
			t.Fatalf("%s: a machine whose marker stood for the limit was not ended", kind)
		}
		got := thing.instance()
		if got.Restarts != 1 || got.EndReason != "silent" || got.ExitLog != "no progress" || got.Shown() != "requested" {
			t.Fatalf("%s: after the silence: got = %+v", kind, got)
		}

		// The next machine starts with no progress of the last one's.
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "name-1", "launch-1"); err != nil {
			t.Fatal(err)
		}
		observe("running", marker("8"))
		if got := thing.instance(); got.ProgressAt == nil || time.Since(*got.ProgressAt) > time.Minute || got.EndReason != "silent" {
			t.Fatalf("%s: the replacement: got = %+v, want its own progress and the last end still shown", kind, got)
		}
	}
}

// A running machine whose progress could not be read is ended by the clock a silent one is: the
// last progress recorded, or its first sight. That end is a renewal and spends no restart.
func TestARunningMachineWhoseProgressIsUnreadableIsEndedByTheSilentClockAndSpendsNoRestart(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const limit, maxRestarts = 8 * time.Minute, 1
	marker := "7"
	for kind, thing := range sandboxedThings(t, pool) {
		observe := func(name, state string, progress *string) {
			t.Helper()
			if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, name, state, progress, limit, "", "machine-1"); err != nil {
				t.Fatal(err)
			}
		}
		unreadable := func(name string) bool {
			t.Helper()
			_, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, name, InstanceProgressUnreadable, "unread", limit, maxRestarts)
			if err != nil {
				t.Fatal(err)
			}
			return recorded
		}
		ask := func(name string) {
			t.Helper()
			if err := pes.AskInstance(ctx, thing.pe, thing.agentID, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
		}

		// Never read: the limit runs from the first sight, and only for a running machine.
		ask("name-1")
		observe("name-1", "starting", nil)
		thing.age("seen_at", 10*limit)
		if unreadable("name-1") {
			t.Fatalf("%s: a starting machine was ended for its progress", kind)
		}
		observe("name-1", "running", nil)
		thing.age("seen_at", limit-10*time.Second)
		if unreadable("name-1") {
			t.Fatalf("%s: ended inside the limit of its first sight", kind)
		}
		thing.age("seen_at", limit+time.Second)
		if !unreadable("name-1") {
			t.Fatalf("%s: unread for the limit since its first sight and not ended", kind)
		}
		if got := thing.instance(); got.Restarts != 0 || got.Renewals != 1 || got.Stopped || got.EndReason != "progress_unreadable" || got.ExitLog != "unread" || got.Shown() != "requested" {
			t.Fatalf("%s: after the end: got = %+v, want a renewal and no restart", kind, got)
		}

		// Read once: the limit runs from that progress, however long ago the machine was seen.
		ask("name-2")
		observe("name-2", "running", &marker)
		thing.age("seen_at", 10*limit)
		thing.age("progress_at", limit-10*time.Second)
		observe("name-2", "running", nil)
		if unreadable("name-2") {
			t.Fatalf("%s: ended inside the limit of its last progress", kind)
		}
		thing.age("progress_at", limit+time.Second)
		if !unreadable("name-2") {
			t.Fatalf("%s: unread for the limit since its last progress and not ended", kind)
		}

		// With every restart spent it is still asked for again.
		ask("name-3")
		if _, recorded, err := pes.EndInstance(ctx, thing.pe, thing.agentID, "name-3", InstanceExited, "boom", limit, maxRestarts); err != nil || !recorded {
			t.Fatalf("%s: exit: %v, %v", kind, recorded, err)
		}
		ask("name-4")
		observe("name-4", "running", nil)
		thing.age("seen_at", limit+time.Second)
		if !unreadable("name-4") {
			t.Fatalf("%s: not ended with no restart left", kind)
		}
		if got := thing.instance(); got.Restarts != maxRestarts || got.Renewals != 3 || got.Stopped {
			t.Fatalf("%s: with no restart left: got = %+v, want it asked for again", kind, got)
		}
	}
}

// A machine the launcher is no longer asked to keep is taken down by the launcher. That is the
// platform's doing and is written down as such, with which end of the run it was, so that it is
// never read as a machine the provider lost: a planner whose draft's goal changed is asked for a
// new machine with no restart spent.
func TestAMachineThePlatformNoLongerAsksForIsRecordedAsReleasedAndNeverAsLost(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const windDown, coordinatorWindDown = 2 * time.Minute, 5 * time.Minute
	running := "running"
	rent := func(pe, agent, name string) {
		t.Helper()
		if err := pes.AskInstance(ctx, pe, agent, name, "launch-1"); err != nil {
			t.Fatal(err)
		}
		if err := pes.ObserveInstance(ctx, pe, agent, name, domain.InstanceRunning, &running, time.Hour, "", "machine-1"); err != nil {
			t.Fatal(err)
		}
	}
	release := func() {
		t.Helper()
		if err := pes.ReleaseInstances(ctx, windDown, coordinatorWindDown); err != nil {
			t.Fatal(err)
		}
	}
	instanceOfAgent := func(pe, agent string) domain.AgentInstance {
		t.Helper()
		signup, err := pes.GetAgentSignup(ctx, pe, agent)
		if err != nil || signup == nil || signup.Instance == nil {
			t.Fatalf("signup: got = %+v, %v", signup, err)
		}
		return *signup.Instance
	}

	draft, planner := createTestDraft(t, pool)
	rent(draft, planner, "planner-machine")
	closing := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, closing, 10)
	agent := spawnTestAgents(t, pes, closing, domain.AgentSignup{Actor: testActor, InAgentSandbox: true, AgentType: "generalist"})[0]
	setPEStatus(t, pool, closing, domain.PlatformExpRunning, time.Hour)
	rent(closing, agent, "agent-machine")
	coordinated := createTestPEWithCoordinator(t, pool)
	rent(coordinated, "", "coordinator-machine")

	// Everything is still asked for: nothing is released.
	release()
	if got := instanceOfAgent(draft, planner); got.Name != "planner-machine" || got.EndReason != "" || got.ExitedAt != nil || got.AskedAt == nil || time.Since(*got.AskedAt) > time.Minute {
		t.Fatalf("a planner at work was released: %+v", got)
	}
	if got := instanceOfAgent(closing, agent); got.Name != "agent-machine" || got.EndReason != "" {
		t.Fatalf("an agent of a running experiment was released: %+v", got)
	}

	// The planner proposed and its wind-down passed, and both experiments closed and theirs did.
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET proposed_at = now() - make_interval(secs => $2), planner_ends_at = now() - make_interval(secs => $2) WHERE id = $1`, draft, (windDown + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	setPEStatus(t, pool, closing, domain.PlatformExpClosed, windDown+time.Second)
	setPEStatus(t, pool, coordinated, domain.PlatformExpClosed, coordinatorWindDown+time.Second)
	// Within its wind-down, which outlasts an agent's, a coordinator's machine is still the
	// launcher's to keep.
	fresh := createTestPEWithCoordinator(t, pool)
	rent(fresh, "", "fresh-coordinator-machine")
	setPEStatus(t, pool, fresh, domain.PlatformExpClosed, windDown+time.Second)
	release()
	release()

	for what, got := range map[string]domain.AgentInstance{"planner": instanceOfAgent(draft, planner), "agent": instanceOfAgent(closing, agent)} {
		if got.Name != "" || got.EndReason != string(InstanceReleased) || got.ExitedAt == nil || got.Restarts != 0 || got.Stopped {
			t.Fatalf("%s: got = %+v, want released with no restart spent", what, got)
		}
	}
	if log := instanceOfAgent(draft, planner).ExitLog; log != domain.RunEndPlanned.ReleasedBecause() {
		t.Fatalf("planner's exit log: %q", log)
	}
	if log := instanceOfAgent(closing, agent).ExitLog; log != domain.RunEndClosed.ReleasedBecause() {
		t.Fatalf("agent's exit log: %q", log)
	}
	closed, err := pes.GetPlatformExperiment(ctx, coordinated)
	if err != nil || closed.Coordinator == nil || closed.Coordinator.Instance.Name != "" || closed.Coordinator.Instance.EndReason != string(InstanceReleased) ||
		closed.Coordinator.Instance.ExitLog != domain.RunEndClosed.ReleasedBecause() || closed.Coordinator.Instance.Restarts != 0 {
		t.Fatalf("coordinator of a closed experiment: got = %+v, %v", closed.Coordinator, err)
	}
	winding, err := pes.GetPlatformExperiment(ctx, fresh)
	if err != nil || winding.Coordinator.Instance.Name != "fresh-coordinator-machine" || winding.Coordinator.Instance.EndReason != "" || winding.Coordinator.Instance.AskedAt == nil {
		t.Fatalf("coordinator inside its wind-down: got = %+v, %v", winding.Coordinator, err)
	}

	// The draft's goal changes: the planner is asked for again, and the machine that was released
	// is not one that was seen and is gone.
	if err := pes.SetGoal(ctx, draft, "it must fit one accelerator", time.Now().UTC().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, recorded, err := pes.EndInstance(ctx, draft, planner, "planner-machine", InstanceGone, "lost", time.Minute, 3); err != nil || recorded {
		t.Fatalf("the released machine was recorded as lost: %v, %v", recorded, err)
	}
	release()
	if got := instanceOfAgent(draft, planner); got.Restarts != 0 || got.EndReason != string(InstanceReleased) {
		t.Fatalf("after the change of the goal: got = %+v", got)
	}
}

// A planner whose machine dies is asked for again with its session, like any agent, and a draft
// whose planner was given up on read planning until its limit, then planning_limit_reached, with
// nobody to answer it. The write that gives the planner up closes the draft and says why. A
// planner that had answered leaves its proposal waiting for the person.
func TestADraftWhosePlannerIsGivenUpOnIsClosedSayingSo(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	const maxRestarts = 2
	die := func(draft, planner string, times int) (after domain.AgentInstance) {
		t.Helper()
		for i := 0; i < times; i++ {
			name := fmt.Sprintf("planner-machine-%d", i)
			if err := pes.AskInstance(ctx, draft, planner, name, "launch-1"); err != nil {
				t.Fatal(err)
			}
			var recorded bool
			var err error
			if after, recorded, err = pes.EndInstance(ctx, draft, planner, name, InstanceExited, "out of memory", time.Minute, maxRestarts); err != nil || !recorded {
				t.Fatalf("machine %d ending: recorded %v, %v", i, recorded, err)
			}
		}
		return after
	}

	draft, planner := createTestDraft(t, pool)
	if after := die(draft, planner, maxRestarts); after.Stopped || after.Restarts != maxRestarts {
		t.Fatalf("after %d machines: got = %+v, want every restart spent and the planner still asked for", maxRestarts, after)
	}
	if got, err := pes.GetPlatformExperiment(ctx, draft); err != nil || got.Status != domain.PlatformExpDraft {
		t.Fatalf("a draft whose planner is being restarted: got = %+v, %v, want it a draft still", got, err)
	}
	if wanted := launcherAgentsOf(t, pes, time.Minute, draft); len(wanted) != 1 || wanted[0].AgentID != planner {
		t.Fatalf("the launcher is asked for %+v, want the planner again", wanted)
	}
	if after := die(draft, planner, 1); !after.Stopped {
		t.Fatalf("one machine more: got = %+v, want the planner stopped", after)
	}
	got, err := pes.GetPlatformExperiment(ctx, draft)
	if err != nil || got.Status != domain.PlatformExpClosed || got.CloseReason != domain.ClosePlannerStopped || got.ClosedBy != nil || got.ClosedAt == nil {
		t.Fatalf("the draft of a stopped planner: got = %s %q closed by %v at %v, %v, want it closed planner_stopped by the platform", got.Status, got.CloseReason, got.ClosedBy, got.ClosedAt, err)
	}

	answered, answering := createTestDraft(t, pool)
	if err := pes.SetProposal(ctx, answered, 1, []byte(`{}`), Proposed{WaitUntil: time.Now().UTC().Add(time.Hour), KeepPlanner: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if after := die(answered, answering, maxRestarts+1); !after.Stopped {
		t.Fatalf("got = %+v, want the planner stopped", after)
	}
	if got, err := pes.GetPlatformExperiment(ctx, answered); err != nil || got.Status != domain.PlatformExpDraft || got.ProposedAt == nil {
		t.Fatalf("a draft whose planner stopped after it proposed: got = %+v, %v, want its proposal still waiting", got, err)
	}
}

// A person watching an accepted experiment saw machines that were "running" and nothing of what
// their agents did. The latest sentence each machine said is kept on its row with the time it
// was first read, and answered on the agent, on the coordinator and, for a planner, on its draft.
// It is what the machine asked for now says: a new machine, an ended one and a released one say
// nothing.
func TestWhatASandboxedAgentIsDoingIsKeptOnItsRowAndAnsweredWhereItIsRead(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := createTestPEWithCoordinator(t, pool)
	agent := spawnTestAgents(t, pes, pe, domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, InAgentSandbox: true, AgentType: "baseline"})[0]
	draft, planner := createTestDraft(t, pool)

	said := func(platformExpID, agentID string) *domain.AgentActivity {
		t.Helper()
		if agentID == "" {
			got, err := pes.GetPlatformExperiment(ctx, platformExpID)
			if err != nil || got.Coordinator == nil {
				t.Fatalf("coordinator of %s: %+v, %v", platformExpID, got, err)
			}
			return got.Coordinator.Activity
		}
		signup, err := pes.GetAgentSignup(ctx, platformExpID, agentID)
		if err != nil || signup == nil {
			t.Fatalf("signup of %s: %v", agentID, err)
		}
		return signup.Activity
	}
	for _, one := range []struct{ pe, agent string }{{pe, agent}, {pe, ""}, {draft, planner}} {
		if err := pes.AskInstance(ctx, one.pe, one.agent, "machine-1", "launch-1"); err != nil {
			t.Fatal(err)
		}
		observe := func(name, activity string) {
			t.Helper()
			if err := pes.ObserveInstance(ctx, one.pe, one.agent, name, domain.InstanceRunning, nil, time.Hour, activity, "machine-1"); err != nil {
				t.Fatal(err)
			}
		}
		observe("machine-1", "")
		if got := said(one.pe, one.agent); got != nil {
			t.Fatalf("%q before it said anything: got = %+v, want none", one.agent, got)
		}
		observe("machine-1", "reading train.py")
		first := said(one.pe, one.agent)
		if first == nil || first.Text != "reading train.py" || time.Since(first.At) > time.Minute {
			t.Fatalf("%q: got = %+v, want its sentence and when it was read", one.agent, first)
		}
		// The same sentence again keeps its time, one read with none keeps the sentence, and
		// another machine's says nothing of this one.
		observe("machine-1", "reading train.py")
		observe("machine-1", "")
		observe("another-machine", "running hl capacity")
		if again := said(one.pe, one.agent); again == nil || *again != *first {
			t.Fatalf("%q after reports that change nothing: got = %+v, want %+v", one.agent, again, first)
		}
		observe("machine-1", "running hl capacity")
		if next := said(one.pe, one.agent); next == nil || next.Text != "running hl capacity" || next.At.Before(first.At) {
			t.Fatalf("%q after its next sentence: got = %+v", one.agent, next)
		}
	}
	// The draft answers its planner's, and an experiment that is no draft answers none of its own.
	if got, err := pes.GetPlatformExperiment(ctx, draft); err != nil || got.Activity == nil || got.Activity.Text != "running hl capacity" {
		t.Fatalf("the draft: activity %+v, %v, want its planner's", got.Activity, err)
	}
	if got, err := pes.GetPlatformExperiment(ctx, pe); err != nil || got.Activity != nil {
		t.Fatalf("an open experiment: activity %+v, %v, want none beside its roster", got.Activity, err)
	}
	listed, err := pes.ListAgentSignups(ctx, pe)
	if err != nil || len(listed) != 1 || listed[0].Activity == nil || listed[0].Activity.Text != "running hl capacity" {
		t.Fatalf("the roster: %+v, %v, want the agent's activity on its entry", listed, err)
	}

	// A machine that ended says nothing, nor does the one asked for in its place until it speaks.
	if _, recorded, err := pes.EndInstance(ctx, pe, agent, "machine-1", InstanceExited, "boom", time.Minute, 3); err != nil || !recorded {
		t.Fatalf("end: %v, %v", recorded, err)
	}
	if got := said(pe, agent); got != nil {
		t.Fatalf("after its machine ended: got = %+v, want none", got)
	}
	if err := pes.AskInstance(ctx, pe, "", "machine-2", "launch-1"); err != nil {
		t.Fatal(err)
	}
	if got := said(pe, ""); got != nil {
		t.Fatalf("a coordinator asked for on a new machine: got = %+v, want none", got)
	}
	// Released with its draft's answer, the planner's machine says nothing either.
	if err := pes.SetProposal(ctx, draft, 1, []byte(`{}`), Proposed{WaitUntil: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := pes.ReleaseInstances(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := pes.GetPlatformExperiment(ctx, draft); err != nil || got.Activity != nil || said(draft, planner) != nil {
		t.Fatalf("a released planner: activity %+v, %v, want none", got.Activity, err)
	}
}

// The warm pool is held exactly as the launcher last reported it, and a report with a machine
// missing its id is refused whole.
func TestTheWarmPoolIsHeldAsLastReported(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	name := "pool-" + uuid.New().String()[:8]
	m1, m2 := "m1-"+name, "m2-"+name
	poolOf := func() []LauncherMachine {
		t.Helper()
		held, err := pes.ListLauncherMachines(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var mine []LauncherMachine
		for _, m := range held {
			if m.Name == name {
				mine = append(mine, m)
			}
		}
		return mine
	}
	if err := pes.HoldPool(ctx, []LauncherMachine{{Name: name, Machine: m1}, {Name: name, Machine: m2}}); err != nil {
		t.Fatal(err)
	}
	if got, want := poolOf(), []LauncherMachine{{Name: name, Machine: m1}, {Name: name, Machine: m2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("held = %+v, want %+v", got, want)
	}
	if err := pes.HoldPool(ctx, []LauncherMachine{{Name: name, Machine: m2}, {Name: name}}); err == nil {
		t.Fatal("a pool machine with no id was taken")
	}
	if err := pes.HoldPool(ctx, []LauncherMachine{{Name: name, Machine: m2}}); err != nil {
		t.Fatal(err)
	}
	if got, want := poolOf(), []LauncherMachine{{Name: name, Machine: m2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after m1 was reported no more: held = %+v, want %+v", got, want)
	}
	if err := pes.HoldPool(ctx, nil); err != nil || len(poolOf()) != 0 {
		t.Fatalf("an empty pool: %v, held %+v", err, poolOf())
	}
}
