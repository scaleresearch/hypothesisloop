package db

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func spawnTestAgents(t *testing.T, pes *PlatformExperimentsStore, pe string, signups ...domain.AgentSignup) []string {
	t.Helper()
	agents := make([]*domain.Agent, len(signups))
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
		agents[i] = &domain.Agent{ID: id, Name: "display-" + id, Kind: domain.AgentKindAgent, PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
	}
	if err := pes.SpawnAgents(context.Background(), pe, agents, signups); err != nil {
		t.Fatalf("spawn agents: got = %v, want = nil", err)
	}
	return ids
}

func setPEStatus(t *testing.T, pool *Pool, pe string, status domain.PlatformExperimentStatus, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := NewPlatformExperimentsStore(pool).UpdatePlatformExperimentStatus(ctx, pe, status); err != nil {
		t.Fatalf("set status %s: got = %v, want = nil", status, err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status_changed_at = now() - make_interval(secs => $2) WHERE id = $1`, pe, ago.Seconds()); err != nil {
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
	spawn := func(pe string, n int) error {
		agents := make([]*domain.Agent, n)
		signups := make([]domain.AgentSignup, n)
		for i := range agents {
			id, _ := domain.NewAgentID()
			agents[i] = &domain.Agent{ID: id, Name: "spawn-rollback-" + id, Kind: domain.AgentKindAgent, CreatedAt: time.Now().UTC()}
			signups[i] = domain.AgentSignup{AgentID: id, Role: domain.SignupRoleCompetitor}
		}
		return pes.SpawnAgents(ctx, pe, agents, signups)
	}

	full := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, full, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = 2 WHERE id = $1`, full); err != nil {
		t.Fatal(err)
	}
	if err := spawn(full, 3); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("three competitors into a field of two: got = %v, want = ErrMaxAgentsReached", err)
	}
	if err := spawn(full, 2); err != nil {
		t.Fatalf("two competitors into a field of two: got = %v, want = nil", err)
	}
	if err := spawn(full, 1); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("a third competitor: got = %v, want = ErrMaxAgentsReached", err)
	}

	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		pe := "pe-" + uuid.New().String()[:8]
		createTestPE(t, pool, pe, 10)
		setPEStatus(t, pool, pe, status, 0)
		if err := spawn(pe, 1); !errors.Is(err, ErrSignupClosed) {
			t.Fatalf("spawn into a %s experiment: got = %v, want = ErrSignupClosed", status, err)
		}
	}
	if err := spawn("pe-that-does-not-exist", 1); !errors.Is(err, ErrSignupClosed) {
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

	if err := pes.RemoveAgent(ctx, pe, ids[0]); err != nil {
		t.Fatalf("remove from an open experiment: got = %v, want = nil", err)
	}
	if in, _ := pes.IsSignedUp(ctx, pe, ids[0]); in {
		t.Error("the removed agent is still signed up")
	}
	if err := pes.RemoveAgent(ctx, pe, ids[0]); !errors.Is(err, ErrNotSignedUp) {
		t.Errorf("remove twice: got = %v, want = ErrNotSignedUp", err)
	}
	if err := pes.RemoveAgent(ctx, "pe-that-does-not-exist", ids[1]); !errors.Is(err, ErrSignupClosed) {
		t.Errorf("remove from no experiment: got = %v, want = ErrSignupClosed", err)
	}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		setPEStatus(t, pool, pe, status, 0)
		if err := pes.RemoveAgent(ctx, pe, ids[1]); !errors.Is(err, ErrSignupClosed) {
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

	open := newPE()
	expect(spawnTestAgents(t, pes, open, hosted)[0], false)
	spawnTestAgents(t, pes, open, domain.AgentSignup{Flavor: "generalist"})
	spawnTestAgents(t, pes, open, domain.AgentSignup{Hosted: true, Launcher: launcher + "-other"})

	running := newPE()
	runningAgents := spawnTestAgents(t, pes, running, hosted, hosted, hosted)
	setPEStatus(t, pool, running, domain.PlatformExpRunning, time.Hour)
	expect(runningAgents[0], false)
	cutTestAgent(t, pool, running, runningAgents[1], 30*time.Second)
	expect(runningAgents[1], true)
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
	expect(spawnTestAgents(t, pes, foreign, hosted)[0], false)

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
		got[a.AgentID] = a.Ending
		order = append(order, a.PlatformExperimentID+"/"+a.AgentID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launcher list (agent -> ending): got = %v, want = %v", got, want)
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("launcher list order: got = %v, want sorted", order)
	}

	for name, wantHas := range map[string]bool{launcher: true, launcher + "-other": true, launcher + "-nobody": false} {
		if has, err := pes.LauncherHasAgents(ctx, name); err != nil || has != wantHas {
			t.Errorf("LauncherHasAgents(%s): got = %v, %v, want = %v", name, has, err, wantHas)
		}
	}
}

// A launcher that is named only by signups that are not hosted runs nothing.
func TestALauncherNamedOnASelfRunSignupHasNoAgents(t *testing.T) {
	pool := eventsTestDB(t)
	pes := NewPlatformExperimentsStore(pool)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	launcher := "launcher-" + uuid.New().String()[:8]
	spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: false, Launcher: launcher})
	if has, err := pes.LauncherHasAgents(context.Background(), launcher); err != nil || has {
		t.Errorf("got = %v, %v, want = false, nil", has, err)
	}
}
