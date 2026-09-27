package db

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func spawnTestAgents(t *testing.T, pes *PlatformExperimentsStore, pe string, signups ...domain.AgentSignup) []string {
	t.Helper()
	ids := make([]string, len(signups))
	for i := range signups {
		id, err := domain.NewAgentID()
		if err != nil {
			t.Fatal(err)
		}
		ids[i], signups[i].AgentID = id, id
		if signups[i].Role == "" {
			signups[i].Role = domain.SignupRoleCompetitor
		}
		agent := &domain.Agent{ID: id, Name: "display-" + id, PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
		if err := pes.SpawnAgent(context.Background(), pe, agent, signups[i]); err != nil {
			t.Fatalf("spawn agent: got = %v, want = nil", err)
		}
	}
	return ids
}

func setPEStatus(t *testing.T, pool *Pool, pe string, status domain.PlatformExperimentStatus, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	var err error
	if status == domain.PlatformExpClosed {
		err = NewPlatformExperimentsStore(pool).ClosePlatformExperiment(ctx, pe, nil)
	} else {
		_, err = pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = $2::platform_experiment_status WHERE id = $1`, pe, string(status))
	}
	if err != nil {
		t.Fatalf("set status %s: got = %v, want = nil", status, err)
	}
	if _, err := pool.pool.Exec(ctx, `
UPDATE platform_experiments
SET status_changed_at = now() - make_interval(secs => $2),
    closed_at = CASE WHEN closed_at IS NULL THEN NULL ELSE now() - make_interval(secs => $2) END
WHERE id = $1`, pe, ago.Seconds()); err != nil {
		t.Fatalf("age status change: got = %v, want = nil", err)
	}
}

func cutTestAgent(t *testing.T, pool *Pool, pe, agent string, ago time.Duration) {
	t.Helper()
	if _, err := pool.pool.Exec(context.Background(), `
INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index, cut_at)
VALUES ($1, $2, 1, now() - make_interval(secs => $3))`, pe, agent, ago.Seconds()); err != nil {
		t.Fatalf("cut agent: got = %v, want = nil", err)
	}
}

func TestSpawnedAgentsAreCreatedInTheExperimentsOrgWithWhatTheyWereLaunchedWith(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	accounts := NewAccountsStore(pool)
	_, orgID := newTestUserHome(t, accounts)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, pe, defaultProjectOf(t, accounts, orgID).ID)
	pes := NewPlatformExperimentsStore(pool)

	ids := spawnTestAgents(t, pes, pe,
		domain.AgentSignup{Hosted: true, Launcher: "main", Flavor: "data-search", Model: "model-x", Hyperparameters: map[string]any{"batch_size": float64(64)}},
		domain.AgentSignup{Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierBurstOnly},
	)

	signups, err := pes.ListAgentSignups(ctx, pe)
	if err != nil || len(signups) != 2 {
		t.Fatalf("list signups: got = %+v, %v, want two", signups, err)
	}
	byID := map[string]domain.AgentSignup{}
	for _, s := range signups {
		byID[s.AgentID] = s
	}
	hosted, own := byID[ids[0]], byID[ids[1]]
	if !hosted.Hosted || hosted.Launcher != "main" || hosted.Flavor != "data-search" || hosted.Model != "model-x" ||
		!reflect.DeepEqual(hosted.Hyperparameters, map[string]any{"batch_size": float64(64)}) ||
		hosted.Role != domain.SignupRoleCompetitor || hosted.Name != "display-"+ids[0] || hosted.PlatformExperimentID != pe {
		t.Errorf("hosted signup: got = %+v", hosted)
	}
	if own.Hosted || own.Launcher != "" || own.Role != domain.SignupRoleBaseline || own.QuotaTier != domain.QuotaTierBurstOnly ||
		own.Hyperparameters == nil || len(own.Hyperparameters) != 0 {
		t.Errorf("self-run signup: got = %+v", own)
	}
	one, err := pes.GetAgentSignup(ctx, pe, ids[0])
	if err != nil || one == nil || !reflect.DeepEqual(*one, hosted) {
		t.Errorf("get signup: got = %+v, %v, want = %+v", one, err, hosted)
	}
	if missing, err := pes.GetAgentSignup(ctx, pe, "nobody"); err != nil || missing != nil {
		t.Errorf("get signup of a stranger: got = %+v, %v, want = nil, nil", missing, err)
	}
	for _, id := range ids {
		if got, found, err := accounts.AgentOrg(ctx, id); err != nil || !found || got != orgID {
			t.Errorf("org of spawned agent %s: got = %q, %v, %v, want = %q", id, got, found, err, orgID)
		}
	}
}

func TestASpawnThatDoesNotFitOrComesTooLateLeavesNothingBehind(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	agentCount := func() int {
		var n int
		if err := pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE name LIKE 'spawn-rollback-%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := agentCount()
	spawn := func(pe string) error {
		id, _ := domain.NewAgentID()
		agent := &domain.Agent{ID: id, Name: "spawn-rollback-" + id, CreatedAt: time.Now().UTC()}
		return pes.SpawnAgent(ctx, pe, agent, domain.AgentSignup{AgentID: id, Role: domain.SignupRoleCompetitor})
	}

	full := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, full, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = 2 WHERE id = $1`, full); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := spawn(full); err != nil {
			t.Fatalf("a competitor into a field of two: got = %v, want = nil", err)
		}
	}
	if err := spawn(full); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("a third competitor: got = %v, want = ErrMaxAgentsReached", err)
	}

	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		pe := "pe-" + uuid.New().String()[:8]
		createTestPE(t, pool, pe, 10)
		setPEStatus(t, pool, pe, status, 0)
		if err := spawn(pe); !errors.Is(err, ErrSignupClosed) {
			t.Fatalf("spawn into a %s experiment: got = %v, want = ErrSignupClosed", status, err)
		}
	}
	if err := spawn("pe-that-does-not-exist"); !errors.Is(err, ErrSignupClosed) {
		t.Fatalf("spawn into no experiment: got = %v, want = ErrSignupClosed", err)
	}
	if got := agentCount() - before; got != 2 {
		t.Errorf("agents left behind by refused spawns: got = %v created, want = 2", got)
	}
	if signups, _ := pes.ListAgentSignups(ctx, full); len(signups) != 2 {
		t.Errorf("signups in the full experiment: got = %v, want = 2", len(signups))
	}
}

func TestAnAgentIsRemovedOnlyBeforeTheExperimentStarts(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	ids := spawnTestAgents(t, pes, pe, domain.AgentSignup{}, domain.AgentSignup{})

	if err := pes.RemoveAgent(ctx, pe, ids[0], nil); err != nil {
		t.Fatalf("remove from an open experiment: got = %v, want = nil", err)
	}
	if in, _ := pes.IsSignedUp(ctx, pe, ids[0]); in {
		t.Error("the removed agent is still signed up")
	}
	if err := pes.RemoveAgent(ctx, pe, ids[0], nil); !errors.Is(err, ErrNotSignedUp) {
		t.Errorf("remove twice: got = %v, want = ErrNotSignedUp", err)
	}
	if err := pes.RemoveAgent(ctx, "pe-that-does-not-exist", ids[1], nil); !errors.Is(err, ErrSignupClosed) {
		t.Errorf("remove from no experiment: got = %v, want = ErrSignupClosed", err)
	}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		setPEStatus(t, pool, pe, status, 0)
		if err := pes.RemoveAgent(ctx, pe, ids[1], nil); !errors.Is(err, ErrSignupClosed) {
			t.Errorf("remove from a %s experiment: got = %v, want = ErrSignupClosed", status, err)
		}
	}
	if in, _ := pes.IsSignedUp(ctx, pe, ids[1]); !in {
		t.Error("an agent was removed from an experiment that had started")
	}
}

func TestALaunchersListFollowsEachAgentThroughItsLife(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	accounts := NewAccountsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	const windDown = 120 * time.Second
	hosted := domain.AgentSignup{Hosted: true, Launcher: launcher, Flavor: "generalist"}

	newPE := func() string {
		pe := "pe-" + uuid.New().String()[:8]
		createTestPE(t, pool, pe, 10)
		return pe
	}
	want := map[string]bool{}
	expect := func(agent string, ending bool) { want[agent] = ending }

	// No researcher's machine is rented before there is a baseline to start from: while a
	// platform experiment is open its launcher runs the agents that measure the baseline and
	// nobody else. One of them is done when it finishes, there and then.
	open := newPE()
	measuring := hosted
	measuring.Role = domain.SignupRoleBaseline
	spawnTestAgents(t, pes, open, hosted)
	baselines := spawnTestAgents(t, pes, open, measuring, measuring)
	expect(baselines[0], false)
	if err := pes.FinishAgent(ctx, open, baselines[1], "the baseline is measured"); err != nil {
		t.Fatalf("a baseline agent finishing in an open experiment: got = %v, want = nil", err)
	}
	expect(baselines[1], true)
	spawnTestAgents(t, pes, open, domain.AgentSignup{Flavor: "generalist"})
	spawnTestAgents(t, pes, open, domain.AgentSignup{Hosted: true, Launcher: launcher + "-other", Role: domain.SignupRoleBaseline})

	running := newPE()
	runningAgents := spawnTestAgents(t, pes, running, hosted, hosted, hosted, hosted, hosted)
	if err := pes.FinishAgent(ctx, running, runningAgents[3], "too early"); !errors.Is(err, ErrSignupClosed) {
		t.Errorf("finish before the experiment runs: got = %v, want = ErrSignupClosed", err)
	}
	setPEStatus(t, pool, running, domain.PlatformExpRunning, time.Hour)
	expect(runningAgents[0], false)
	// An agent with nothing left to do is let wind down like a cut one, and one with a job in
	// flight is not finished yet.
	seedExperimentForCapTest(t, pool, "job-"+uuid.New().String()[:8], runningAgents[0], running, 1, domain.StatusRunning)
	if err := pes.FinishAgent(ctx, running, runningAgents[0], "done"); !errors.Is(err, ErrWorkUnfinished) {
		t.Errorf("finish with a running job: got = %v, want = ErrWorkUnfinished", err)
	}
	for _, agent := range runningAgents[3:] {
		if err := pes.FinishAgent(ctx, running, agent, "quota spent"); err != nil {
			t.Fatalf("finish: got = %v, want = nil", err)
		}
	}
	expect(runningAgents[3], true)
	// A submission and a finish are decided under one lock. The finished agent's submission is
	// refused inside the admission itself, and a finish that meets a submission in flight waits
	// for it and then counts its job.
	admission := NewPlatformExperimentsFullStore(NewStore(pool, "http://metrics.invalid", 1))
	late := &domain.Experiment{ID: "job-" + uuid.New().String()[:8], AgentID: runningAgents[3], PlatformExperimentID: running,
		Actor: domain.Actor{Kind: domain.ActorAgent, ID: runningAgents[3]}, Status: domain.StatusQueued}
	if decision, reason, err := admission.AdmitExperimentTx(ctx, late, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err != nil || decision != AdmitRejected || !strings.HasPrefix(reason, RejectionAgentFinished) {
		t.Errorf("admitting a finished agent's job: got = (%v, %q, %v), want = rejected as finished", decision, reason, err)
	}
	submitting, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer submitting.Rollback(ctx) //nolint:errcheck
	inFlight := "job-" + uuid.New().String()[:8]
	if _, err := submitting.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, admissionLockKey(runningAgents[1], running)); err != nil {
		t.Fatal(err)
	}
	if _, err := submitting.Exec(ctx, `INSERT INTO experiments SELECT (jsonb_populate_record(e, jsonb_build_object('id', $1::text, 'agent_id', $2::text))).* FROM experiments e WHERE e.agent_id = $3 AND e.platform_experiment_id = $4`,
		inFlight, runningAgents[1], runningAgents[0], running); err != nil {
		t.Fatalf("insert a job in flight: %v", err)
	}
	meanwhile := make(chan error, 1)
	go func() { meanwhile <- pes.FinishAgent(ctx, running, runningAgents[1], "raced") }()
	select {
	case err := <-meanwhile:
		t.Fatalf("finish beside a submission in flight: got = %v at once, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := submitting.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-meanwhile; !errors.Is(err, ErrWorkUnfinished) {
		t.Errorf("finish after the submission committed: got = %v, want = ErrWorkUnfinished", err)
	}
	for agent, want := range map[string]bool{runningAgents[0]: false, runningAgents[3]: true} {
		if finished, err := pes.IsAgentFinished(ctx, running, agent); err != nil || finished != want {
			t.Errorf("finished %s: got = (%v, %v), want = %v", agent, finished, err, want)
		}
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE experiment_signups SET finished_at = now() - make_interval(secs => $3) WHERE platform_experiment_id = $1 AND agent_id = $2`,
		running, runningAgents[4], (windDown + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	if signup, err := pes.GetAgentSignup(ctx, running, runningAgents[4]); err != nil || signup.FinishedAt == nil || signup.FinishedReason != "quota spent" || !signup.Instance.Released || signup.Ended != domain.RunEndFinished {
		t.Errorf("finished signup: got = (%+v, %v), want = its moment, its reason and its machine released", signup, err)
	}
	cutTestAgent(t, pool, running, runningAgents[1], 30*time.Second)
	expect(runningAgents[1], true)
	if signup, err := pes.GetAgentSignup(ctx, running, runningAgents[1]); err != nil || signup.Ended != domain.RunEndCut {
		t.Errorf("cut signup: got = (%+v, %v), want = ended as cut", signup, err)
	}
	if signup, err := pes.GetAgentSignup(ctx, running, runningAgents[0]); err != nil || signup.Ended != "" {
		t.Errorf("signup of an agent at work: got = (%+v, %v), want = not ended", signup, err)
	}
	cutTestAgent(t, pool, running, runningAgents[2], windDown+time.Second)

	justClosed := newPE()
	justClosedAgents := spawnTestAgents(t, pes, justClosed, hosted, hosted)
	setPEStatus(t, pool, justClosed, domain.PlatformExpRunning, time.Hour)
	cutTestAgent(t, pool, justClosed, justClosedAgents[1], time.Hour)
	setPEStatus(t, pool, justClosed, domain.PlatformExpClosed, windDown-10*time.Second)
	expect(justClosedAgents[0], true)

	longClosed := newPE()
	spawnTestAgents(t, pes, longClosed, hosted)
	setPEStatus(t, pool, longClosed, domain.PlatformExpClosed, windDown+time.Second)

	draft := newPE()
	spawnTestAgents(t, pes, draft, hosted)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'draft' WHERE id = $1`, draft); err != nil {
		t.Fatal(err)
	}

	_, otherOrg := newTestUserHome(t, accounts)
	foreign := "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, foreign, defaultProjectOf(t, accounts, otherOrg).ID)
	foreignAgent := spawnTestAgents(t, pes, foreign, hosted)[0]
	setPEStatus(t, pool, foreign, domain.PlatformExpRunning, time.Hour)
	expect(foreignAgent, false)

	listed, err := pes.ListLauncherAgents(ctx, launcher, windDown)
	if err != nil {
		t.Fatalf("list launcher agents: got = %v, want = nil", err)
	}
	got := map[string]bool{}
	var order []string
	for _, a := range listed {
		if !a.Hosted || a.Launcher != launcher {
			t.Errorf("listed an agent that is not this launcher's: %+v", a)
		}
		got[a.AgentID] = a.Ended != ""
		order = append(order, a.PlatformExperimentID+"/"+a.AgentID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launcher list (agent -> ending): got = %v, want = %v", got, want)
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("launcher list order: got = %v, want sorted", order)
	}

}

// The wind-down is counted from closed_at, the moment the authenticator counts an agent token's
// last minutes from, and never from a later status change.
func TestTheWindDownIsCountedFromTheClose(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	const windDown = 120 * time.Second
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: true, Launcher: launcher})
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, time.Hour)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status_changed_at = now() WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if listed, err := pes.ListLauncherAgents(ctx, launcher, windDown); err != nil || len(listed) != 0 {
		t.Errorf("closed an hour ago with a fresh status change: listed = %+v, %v, want none", listed, err)
	}
}

// After a real run closed and its machines were destroyed, every instance still read "running":
// the launcher is no longer asked about a machine whose run is over, so what it last reported of
// it stays in the row. A machine is shown as stopped once the wind-down after its run has
// passed, for an agent whose experiment closed, one that was cut, and a coordinator.
func TestAMachineOfARunThatEndedIsShownAsStoppedOnceItsWindDownPassed(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	agents := spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: true, Launcher: launcher}, domain.AgentSignup{Hosted: true, Launcher: launcher})
	if _, err := pool.pool.Exec(ctx, `UPDATE experiment_signups SET instance_name = 'm', instance_state = 'running' WHERE platform_experiment_id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET coordinator_hosted = true, coordinator_launcher = $2, coordinator_instance_name = 'm', coordinator_instance_state = 'running' WHERE id = $1`, pe, launcher); err != nil {
		t.Fatal(err)
	}
	shown := func() (first, second, coordinator string) {
		t.Helper()
		signups, err := pes.ListAgentSignups(ctx, pe)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]string{}
		for _, signup := range signups {
			byID[signup.AgentID] = signup.Instance.Shown()
		}
		got, err := pes.GetPlatformExperiment(ctx, pe)
		if err != nil {
			t.Fatal(err)
		}
		return byID[agents[0]], byID[agents[1]], got.Coordinator.Instance.Shown()
	}
	if a, b, c := shown(); a != "running" || b != "running" || c != "running" {
		t.Fatalf("while the run goes on: %s, %s, %s, want running", a, b, c)
	}
	cutTestAgent(t, pool, pe, agents[0], domain.AgentWindDown+time.Minute)
	if a, b, c := shown(); a != "stopped" || b != "running" || c != "running" {
		t.Fatalf("after one agent's cut and its wind-down: %s, %s, %s, want only the cut agent stopped", a, b, c)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, domain.AgentWindDown/2)
	if _, b, c := shown(); b != "running" || c != "running" {
		t.Fatalf("inside the wind-down after the close: %s, %s, want running, their last turn is under way", b, c)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, domain.AgentWindDown+time.Minute)
	if a, b, c := shown(); a != "stopped" || b != "stopped" || c != "stopped" {
		t.Fatalf("after the close and its wind-down: %s, %s, %s, want stopped", a, b, c)
	}
}

func TestARegisteredAgentJoinsOnlyAnExperimentOfItsOwnOrg(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	accounts := NewAccountsStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)
	peA, peB := "pe-"+uuid.New().String()[:8], "pe-"+uuid.New().String()[:8]
	createTestPEIn(t, pool, peA, defaultProjectOf(t, accounts, orgA).ID)
	createTestPEIn(t, pool, peB, defaultProjectOf(t, accounts, orgB).ID)
	agentOfA := spawnTestAgents(t, pes, peA, domain.AgentSignup{})[0]

	signup := domain.AgentSignup{AgentID: agentOfA, Role: domain.SignupRoleCompetitor}
	if inserted, err := pes.Signup(ctx, peB, signup); !errors.Is(err, ErrAgentOutsideOrg) || inserted {
		t.Fatalf("an agent of org A into an experiment of org B: got = %v, %v, want = ErrAgentOutsideOrg", inserted, err)
	}
	if in, _ := pes.IsSignedUp(ctx, peB, agentOfA); in {
		t.Fatal("the agent is signed up in the other org's experiment")
	}
	signup.AgentID = "agent-nobody-" + uuid.New().String()[:8]
	if _, err := pes.Signup(ctx, peA, signup); !errors.Is(err, ErrAgentOutsideOrg) {
		t.Fatalf("an agent that does not exist: got = %v, want = ErrAgentOutsideOrg", err)
	}
}

// Removal takes the agent's place and leaves the agent: it is added again by its id.
func TestARemovedAgentStaysInItsOrgAndJoinsAgainByItsID(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	accounts := NewAccountsStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	_, orgID := newTestUserHome(t, accounts)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, pe, defaultProjectOf(t, accounts, orgID).ID)
	agent := spawnTestAgents(t, pes, pe, domain.AgentSignup{})[0]
	if err := pes.RemoveAgent(ctx, pe, agent, nil); err != nil {
		t.Fatal(err)
	}
	if got, found, err := accounts.AgentOrg(ctx, agent); err != nil || !found || got != orgID {
		t.Fatalf("org of the removed agent: got = %q, %v, %v, want = %q", got, found, err, orgID)
	}
	if inserted, err := pes.Signup(ctx, pe, domain.AgentSignup{AgentID: agent, Role: domain.SignupRoleCompetitor}); err != nil || !inserted {
		t.Fatalf("join again: got = %v, %v, want = true, nil", inserted, err)
	}
}
