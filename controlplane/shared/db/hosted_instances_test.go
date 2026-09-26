package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// hostedThing is a hosted agent or, with no agent id, a hosted coordinator, and how each is read
// back and aged.
type hostedThing struct {
	pe, agentID string
	instance    func() domain.AgentInstance
	age         func(column string, by time.Duration)
}

func hostedThings(t *testing.T, pool *Pool) map[string]hostedThing {
	t.Helper()
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	age := func(table, where string, args ...any) func(string, time.Duration) {
		return func(column string, by time.Duration) {
			t.Helper()
			if _, err := pool.pool.Exec(ctx, `UPDATE `+table+` SET `+column+` = now() - make_interval(secs => $1) WHERE `+where, append([]any{by.Seconds()}, args...)...); err != nil {
				t.Fatal(err)
			}
		}
	}
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	agent := spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: true, Launcher: "main"})[0]
	coordinated := createTestPEWithCoordinator(t, pool, "main")
	return map[string]hostedThing{
		"agent": {pe, agent, func() domain.AgentInstance {
			t.Helper()
			signup, err := pes.GetAgentSignup(ctx, pe, agent)
			if err != nil || signup == nil || signup.Instance == nil {
				t.Fatalf("signup: got = %+v, %v", signup, err)
			}
			return *signup.Instance
		}, age("experiment_signups", "platform_experiment_id = $2 AND agent_id = $3", pe, agent)},
		"coordinator": {coordinated, "", func() domain.AgentInstance {
			t.Helper()
			got, err := pes.GetPlatformExperiment(ctx, coordinated)
			if err != nil || got == nil || got.Coordinator == nil {
				t.Fatalf("platform experiment: got = %+v, %v", got, err)
			}
			return got.Coordinator.Instance
		}, age("platform_experiments", "id = $2", coordinated)},
	}
}

func (h hostedThing) column(name string) string {
	if h.agentID == "" {
		return "coordinator_instance_" + name
	}
	return "instance_" + name
}

func TestAnExitedMachineIsRecordedRestartedAndFinallyStopsWhatItRan(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	const maxRestarts = 2
	for kind, thing := range hostedThings(t, pool) {
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

		// Stopped under one launch, it is asked for again under another with nothing spent.
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "same-launch", "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got := thing.instance(); !got.Stopped || got.Restarts != maxRestarts {
			t.Fatalf("%s: asked for again under the launch it was stopped under: got = %+v, want it still stopped", kind, got)
		}
		if err := pes.AskInstance(ctx, thing.pe, thing.agentID, "new-launch", "launch-2"); err != nil {
			t.Fatal(err)
		}
		if got := thing.instance(); got.Stopped || got.Restarts != 0 || got.Name != "new-launch" || got.Launch != "launch-2" {
			t.Fatalf("%s: asked for under another launch: got = %+v, want a fresh budget", kind, got)
		}
	}
}

func TestAMachineIsGivenUpOnWhenItIsNeverSeenStaysStartingOrIsGone(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	const bootLimit, maxRestarts = 5 * time.Minute, 3
	for kind, thing := range hostedThings(t, pool) {
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
		thing.age(thing.column("asked_at"), bootLimit-10*time.Second)
		if end("never-seen", InstanceNeverSeen) || end("never-seen", InstanceGone) || end("never-seen", InstanceStillStarting) {
			t.Fatalf("%s: an unseen machine inside the limit was given up on", kind)
		}
		thing.age(thing.column("asked_at"), bootLimit+time.Second)
		if end("never-seen", InstanceGone) || end("never-seen", InstanceStillStarting) {
			t.Fatalf("%s: an unseen machine was recorded as gone or as starting", kind)
		}
		if !end("never-seen", InstanceNeverSeen) || thing.instance().Restarts != 1 {
			t.Fatalf("%s: an unseen machine past the limit: got = %+v, want one restart spent", kind, thing.instance())
		}

		// Seen: the limit of a machine that stays starting is counted from then, however long
		// ago it was asked for.
		ask("starting")
		thing.age(thing.column("asked_at"), 10*bootLimit)
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "another-name", "starting", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
		if thing.instance().SeenAt != nil {
			t.Fatalf("%s: seeing another name marked this one seen", kind)
		}
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "starting", "starting", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
		first := thing.instance().SeenAt
		if first == nil || end("starting", InstanceNeverSeen) || end("starting", InstanceStillStarting) {
			t.Fatalf("%s: a machine just seen: got = %+v, want it kept", kind, thing.instance())
		}
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "starting", "starting", nil, time.Hour); err != nil || !thing.instance().SeenAt.Equal(*first) {
			t.Fatalf("%s: seen again moved the first sight: %v", kind, err)
		}
		thing.age(thing.column("seen_at"), bootLimit+time.Second)
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
		if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "gone", "starting", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
		if !end("gone", InstanceGone) || thing.instance().Restarts != 3 || thing.instance().ExitLog != "why" {
			t.Fatalf("%s: seen and gone: got = %+v, want a third restart spent", kind, thing.instance())
		}
	}
}

// Two reconciles of one launcher can overlap, and both report the exit they both saw.
func TestOneExitReportedByTwoReconcilesAtOnceSpendsOneRestart(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	for kind, thing := range hostedThings(t, pool) {
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

func TestALaunchersTokenFollowsItsRow(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	name := "l" + uuid.New().String()[:8]
	known := func() bool {
		t.Helper()
		is, err := pes.LauncherKnown(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	if known() {
		t.Fatal("a launcher nobody registered is known")
	}
	// A signup naming it does not make it one.
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: true, Launcher: name})
	if known() {
		t.Fatal("a launcher a signup names and nobody registered is known")
	}
	for range 2 {
		if err := pes.RegisterLauncher(ctx, name); err != nil || !known() {
			t.Fatalf("register: got = %v, known %v", err, known())
		}
	}
	if removed, err := pes.RemoveLauncher(ctx, name); err != nil || !removed || known() {
		t.Fatalf("remove: got = %v, %v, still known %v", removed, err, known())
	}
	if removed, err := pes.RemoveLauncher(ctx, name); err != nil || removed {
		t.Fatalf("remove again: got = %v, %v, want false", removed, err)
	}
	for _, bad := range []string{"", "with-dash", "Upper", "seventeencharacter"} {
		if err := pes.RegisterLauncher(ctx, bad); err == nil {
			t.Errorf("registered a launcher named %q", bad)
		}
	}
}

// The bound is on machines, so a hosted baseline or reviewer counts as a competitor does, and so
// does the coordinator.
func TestHostedAgentsOfEveryRoleAndTheCoordinatorCountAgainstTheMachinesOfOneExperiment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "bounded", BudgetAcceleratorHours: 10,
		MaxAgents: 2, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now, MaxHosted: 4,
		CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Launcher: "main", Model: "model-1"},
	}
	if err := pes.CreatePlatformExperiment(ctx, pe); err != nil {
		t.Fatal(err)
	}
	add := func(signup domain.AgentSignup) error {
		id, err := domain.NewAgentID()
		if err != nil {
			t.Fatal(err)
		}
		signup.AgentID = id
		agent := &domain.Agent{ID: id, Name: id, PerformanceScore: 0.5, CreatedAt: now}
		return pes.SpawnAgent(ctx, pe.ID, agent, signup)
	}
	// max_agents is two competitors, which no baseline or reviewer counts against. Added one at
	// a time they would be without end, were machines not counted.
	for i, role := range []domain.SignupRole{domain.SignupRoleBaseline, domain.SignupRoleReviewer, domain.SignupRoleBaseline} {
		if err := add(domain.AgentSignup{Role: role, Hosted: true, Launcher: "main"}); err != nil {
			t.Fatalf("hosted %s %d: got = %v, want = nil", role, i+1, err)
		}
	}
	for _, role := range []domain.SignupRole{domain.SignupRoleBaseline, domain.SignupRoleReviewer, domain.SignupRoleCompetitor} {
		if err := add(domain.AgentSignup{Role: role, Hosted: true, Launcher: "main"}); !errors.Is(err, ErrMaxHostedReached) {
			t.Fatalf("a fifth machine, a hosted %s: got = %v, want ErrMaxHostedReached", role, err)
		}
	}
	// An agent its owner runs is no machine of the platform's.
	if err := add(domain.AgentSignup{Role: domain.SignupRoleCompetitor}); err != nil {
		t.Fatalf("a self-run agent in a full house: got = %v, want = nil", err)
	}
	if joined, err := pes.ListAgentSignups(ctx, pe.ID); err != nil || len(joined) != 4 {
		t.Fatalf("joined = %d, %v, want the three hosted and the self-run one", len(joined), err)
	}
}

func TestARunningMachinesProgressIsStampedOnThisClockAndItsSilenceEndsIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	const limit, maxRestarts = 8 * time.Minute, 3
	marker := func(m string) *string { return &m }
	for kind, thing := range hostedThings(t, pool) {
		observe := func(state string, progress *string) {
			t.Helper()
			if err := pes.ObserveInstance(ctx, thing.pe, thing.agentID, "name-1", state, progress, limit); err != nil {
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
		thing.age(thing.column("seen_at"), 10*limit)
		if got := thing.instance(); got.Shown() != "starting" || got.ProgressAt != nil || silent() {
			t.Fatalf("%s: starting: got = %+v, want no progress and no silence", kind, got)
		}

		// Running and never a marker: the limit runs from the first sight.
		observe("running", marker(""))
		if got := thing.instance(); got.Shown() != "running" || got.ProgressAt != nil {
			t.Fatalf("%s: running with no marker: got = %+v", kind, got)
		}
		thing.age(thing.column("seen_at"), limit-10*time.Second)
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
		thing.age(thing.column("progress_at"), limit/4-10*time.Second)
		aged := thing.instance().ProgressAt
		observe("running", marker("8"))
		if got := thing.instance().ProgressAt; !got.Equal(*aged) {
			t.Fatalf("%s: a marker inside a quarter of the limit was written down", kind)
		}
		thing.age(thing.column("progress_at"), limit/4+time.Second)
		observe("running", marker("8"))
		if got := thing.instance().ProgressAt; time.Since(*got) > time.Minute {
			t.Fatalf("%s: a new marker a quarter of the limit later: progress_at = %v, want now", kind, got)
		}

		// The marker stops changing: nothing inside the limit, an end past it.
		thing.age(thing.column("progress_at"), limit-10*time.Second)
		observe("running", marker("8"))
		if silent() {
			t.Fatalf("%s: ended as silent inside the limit", kind)
		}
		thing.age(thing.column("progress_at"), limit+time.Second)
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
