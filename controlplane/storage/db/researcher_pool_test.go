package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// poolRun is a platform experiment the platform runs the researchers of, with its pool written.
func poolRun(t *testing.T, pool *Pool, maxAgents int, researchers domain.ResearcherPool) (string, *Store) {
	t.Helper()
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	encoded, err := poolJSON(researchers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = $2, researchers = $3, coordinator_hosted = true WHERE id = $1`, pe, maxAgents, encoded); err != nil {
		t.Fatal(err)
	}
	return pe, pes
}

func spawnResearcher(t *testing.T, pes *Store, pe, flavor string) string {
	t.Helper()
	id, _ := domain.NewID(domain.AgentIDPrefix)
	agent := &domain.Agent{ID: id, Name: "r-" + id, CreatedAt: time.Now().UTC()}
	if err := pes.SpawnAgent(context.Background(), pe, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleCompetitor, Hosted: true, AgentType: flavor, Model: "model-1"}); err != nil {
		t.Fatalf("spawn: got = %v, want = nil", err)
	}
	qid, _ := domain.NewID(domain.AgentQuotaIDPrefix)
	if _, err := pes.pool.pool.Exec(context.Background(), upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: qid, AgentID: id, PlatformExperimentID: pe, GuaranteedAcceleratorHours: 2, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		t.Fatal(err)
	}
	return id
}

// The pool is read with who is at work, who ended for good and not yet replaced, and nobody
// cut; a replacement takes its predecessor out of both, and of its launcher's desired state, and
// is told as agent.replaced, live and replayed.
func TestThePoolStateNamesWhoEndedAndAReplacementTakesItsPlaceOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	pe, pes := poolRun(t, pool, 4, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 4}})
	atWork, finished, stopped, cut := spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if err := pes.FinishAgent(ctx, pe, finished, "nothing left"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO hosted_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, stopped)
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, pe, cut)

	state, err := pes.ResearcherPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	var endedIDs []string
	for _, r := range state.Ended[generalist] {
		endedIDs = append(endedIDs, r.AgentID)
	}
	if state.MaxAgents != 4 || state.Pool.Total() != 4 || state.AtWork[generalist] != 1 || len(endedIDs) != 2 || endedIDs[0] != finished || endedIDs[1] != stopped {
		t.Fatalf("state = %+v, ended %v, want max_agents 4, one at work (%s) and the finished and stopped ones in that order", state, endedIDs, atWork)
	}

	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()
	replacement, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: replacement, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: replacement, Role: domain.SignupRoleCompetitor, Hosted: true, AgentType: "generalist", Model: "model-1", Replaces: stopped},
		LateJoin{Used: map[string]float64{}}); err != nil {
		t.Fatalf("a replacement: got = %v, want = nil", err)
	}
	live := awaitEvent(t, events, domain.EventAgentReplaced, replacement)
	if live.Value != stopped || live.Detail != "its machine stopped with no restart left" {
		t.Errorf("live: got = %+v, want the stopped one and why its place was free", live)
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: pe}, before)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range replayed {
		if e.Kind == domain.EventAgentReplaced {
			found = e.Identity() == live.Identity()
		}
	}
	if !found {
		t.Errorf("replayed %+v, want the live agent.replaced again", replayed)
	}

	state, err = pes.ResearcherPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if state.AtWork[generalist] != 2 || len(state.Ended[generalist]) != 1 || state.Ended[generalist][0].AgentID != finished {
		t.Errorf("after the replacement: %+v, want two at work and the finished one alone still free", state)
	}
	if signup, err := pes.GetAgentSignup(ctx, pe, stopped); err != nil || signup.ReplacedBy != replacement {
		t.Errorf("the replaced one: got = %+v %v, want replaced_by %s", signup, err, replacement)
	}
	// A launch input changing gives a stopped machine a fresh budget: the replaced one is no longer
	// wanted at all.
	agents, err := pes.ListLauncherAgents(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.AgentID == stopped {
			t.Errorf("the launcher still wants the replaced researcher %s", stopped)
		}
	}
	// A researcher is replaced once.
	twice, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: twice, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: twice, Role: domain.SignupRoleCompetitor, Hosted: true, AgentType: "generalist", Model: "model-1", Replaces: stopped},
		LateJoin{Used: map[string]float64{}}); err == nil {
		t.Error("a second replacement of one researcher was written")
	}
}

// A boundary lowers the pool by the researchers it cuts, in the write that cuts: nobody is added in
// their places. A researcher of no entry lowers nothing.
func TestACutLowersThePoolInItsOwnWrite(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 6, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 3}, {AgentType: "data-search", Model: "model-1", Count: 2}})
	a, b := spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "data-search")
	extra := spawnResearcher(t, pes, pe, "architecture-search")
	survivor := spawnResearcher(t, pes, pe, "generalist")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if _, err := pes.AdvanceStage(ctx, pe, 1, []string{a, b, extra}, []string{survivor}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if got.Researchers[0].Count != 2 || got.Researchers[1].Count != 1 || got.MaxAgents != 6 {
		t.Errorf("researchers %+v, max_agents %d, want 2 and 1 after the cut and the cap as it was", got.Researchers, got.MaxAgents)
	}
}

// max_agents and the pool are held to the researchers at work, the count a join is held to: one
// that finished or was cut takes no place, and the refusal says how many are at work.
func TestMaxAgentsIsHeldToTheResearchersAtWork(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 4, nil)
	spawnResearcher(t, pes, pe, "generalist")
	spawnResearcher(t, pes, pe, "generalist")
	finished, cut := spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist")

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	got.MaxAgents = 3
	var below *PoolBelowAtWorkError
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpOpen, nil); !errors.As(err, &below) || below.AtWork != 4 || below.Count != 3 {
		t.Fatalf("max_agents below the four signed up before the start: got = %v, want PoolBelowAtWorkError of 4", err)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if err := pes.FinishAgent(ctx, pe, finished, "done"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, pe, cut)
	if got, err = pes.GetPlatformExperiment(ctx, pe); err != nil {
		t.Fatal(err)
	}
	got.MaxAgents = 2
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpRunning, nil); err != nil {
		t.Fatalf("max_agents of the two at work: got = %v, want = nil", err)
	}
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpRunning, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 1}}); !errors.As(err, &below) || below.Key.AgentType != "generalist" {
		t.Fatalf("an entry below its researchers at work: got = %v, want PoolBelowAtWorkError of the entry", err)
	}
	if stored, _ := pes.GetPlatformExperiment(ctx, pe); stored.MaxAgents != 2 || len(stored.Researchers) != 0 {
		t.Errorf("stored %d and %+v, want the refused write to leave max_agents 2 and no researchers", stored.MaxAgents, stored.Researchers)
	}
}

// A max_agents lowered below the researchers at work of a running platform experiment cuts the
// newest of them, as the platform's decision written as their end: each is told agent.cut with
// why, the ladder does not move, and nobody takes their places.
func TestALoweredMaxAgentsCutsTheNewestResearchersAndNobodyReplacesThem(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	dataSearch := domain.PoolKey{AgentType: "data-search", Model: "model-1"}
	pe, pes := poolRun(t, pool, 4, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 3}, {AgentType: "data-search", Model: "model-1", Count: 1}})
	first := spawnResearcher(t, pes, pe, "generalist")
	second := spawnResearcher(t, pes, pe, "generalist")
	third := spawnResearcher(t, pes, pe, "generalist")
	newest := spawnResearcher(t, pes, pe, "data-search")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	before := cursorNow(t, pool)
	live, stop := listenForEvents(t, pool)
	defer stop()

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	got.MaxAgents = 2
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpRunning, nil); err != nil {
		t.Fatalf("lower max_agents to 2 of 4 at work: got = %v, want = nil", err)
	}
	for _, c := range []struct {
		agent string
		ended domain.RunEnd
	}{{first, ""}, {second, ""}, {third, domain.RunEndCut}, {newest, domain.RunEndCut}} {
		if signup := signupOf(t, pes, pe, c.agent); signup.Ended != c.ended || (c.ended == domain.RunEndCut) != (signup.CutReason == CutMaxAgentsLowered) {
			t.Errorf("%s: ended %q cut for %q, want %q", c.agent, signup.Ended, signup.CutReason, c.ended)
		}
	}
	state, err := pes.ResearcherPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if state.MaxAgents != 2 || state.AtWork[generalist] != 2 || state.AtWork[dataSearch] != 0 || len(state.Ended[generalist])+len(state.Ended[dataSearch]) != 0 {
		t.Fatalf("state = %+v, want two at work and nobody whose place is free", state)
	}
	if state.Pool.Total() != 2 || len(poolPlaces(state)) != 0 {
		t.Errorf("researchers %+v, want them lowered by the two cut, so that nobody is added", state.Pool)
	}

	told := map[string]string{}
	deadline := time.After(5 * time.Second)
	for len(told) < 2 {
		select {
		case e := <-live:
			if e.PlatformExperimentID != pe {
				continue
			}
			if e.Kind == domain.EventStageBoundary {
				t.Errorf("a cut for max_agents moved the ladder: %+v", e)
			}
			if e.Kind == domain.EventAgentCut {
				told[e.AgentID] = e.Detail
			}
		case <-deadline:
			t.Fatalf("told %v, want the two cut", told)
		}
	}
	if told[third] != CutMaxAgentsLowered || told[newest] != CutMaxAgentsLowered {
		t.Errorf("told %v, want %s to %s and %s", told, CutMaxAgentsLowered, third, newest)
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: pe}, before)
	if err != nil {
		t.Fatal(err)
	}
	cuts := 0
	for _, e := range replayed {
		if e.Kind == domain.EventStageBoundary {
			t.Errorf("replayed a ladder move: %+v", e)
		}
		if e.Kind == domain.EventAgentCut && e.Detail == CutMaxAgentsLowered {
			cuts++
		}
	}
	if cuts != 2 {
		t.Errorf("replayed %d cuts for max_agents, want 2", cuts)
	}
}

// A platform experiment and the researchers it is created with are written together or not at
// all: one machine too many leaves nothing.
func TestAPlatformExperimentIsCreatedWithItsResearchersOrNotAtAll(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	create := func(maxHosted, researchers int) (string, error) {
		now := time.Now().UTC()
		pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "pool", BudgetAcceleratorHours: 10,
			MaxAgents: researchers, Researchers: domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: researchers}},
			StartsAt: now, DurationHours: 24, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, ReportIntervalSeconds: 30,
			Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now, MaxHosted: maxHosted,
			CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Model: "model-1"},
		}
		var agents []*domain.Agent
		var signups []domain.AgentSignup
		for i := 0; i <= researchers; i++ {
			id, _ := domain.NewID(domain.AgentIDPrefix)
			role := domain.SignupRoleCompetitor
			if i == researchers {
				role = domain.SignupRoleBaseline
			}
			agents = append(agents, &domain.Agent{ID: id, CreatedAt: now})
			signups = append(signups, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role, Hosted: true, Model: "model-1"})
		}
		return pe.ID, pes.CreatePlatformExperiment(ctx, pe, agents, signups)
	}
	id, err := create(5, 3)
	if err != nil {
		t.Fatalf("three researchers, a baseline agent and a coordinator on five machines: got = %v, want = nil", err)
	}
	if signups, err := pes.ListAgentSignups(ctx, id); err != nil || len(signups) != 4 {
		t.Errorf("signups %d %v, want all four", len(signups), err)
	}
	id, err = create(4, 3)
	if !errors.Is(err, ErrMaxHostedReached) {
		t.Fatalf("one machine too many: got = %v, want ErrMaxHostedReached", err)
	}
	if got, err := pes.GetPlatformExperiment(ctx, id); err != nil || got != nil {
		t.Errorf("after the refusal: got = %+v %v, want no platform experiment", got, err)
	}
}

func poolPlaces(state PoolState) []domain.PoolPlace {
	places, _ := domain.PoolDeficit(state.Pool, state.MaxAgents, state.AtWork, nil, state.Recent)
	return places
}
