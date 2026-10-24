package db

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// poolRun is a platform experiment the platform runs the agents of, with its pool written.
func poolRun(t *testing.T, pool *Pool, maxAgents int, agents domain.AgentPool) (string, *Store) {
	t.Helper()
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	encoded, err := poolJSON(agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = $2, agents = $3, coordinator_in_agentsandbox = true WHERE id = $1`, pe, maxAgents, encoded); err != nil {
		t.Fatal(err)
	}
	return pe, pes
}

func spawnExecutor(t *testing.T, pes *Store, pe, agentType string) string {
	t.Helper()
	id, _ := domain.NewID(domain.AgentIDPrefix)
	agent := &domain.Agent{ID: id, Name: "r-" + id, CreatedAt: time.Now().UTC()}
	if err := pes.SpawnAgent(context.Background(), pe, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleExecutor, InAgentSandbox: true, AgentType: agentType, Model: "model-1"}); err != nil {
		t.Fatalf("spawn: got = %v, want = nil", err)
	}
	qid, _ := domain.NewID(domain.AgentQuotaIDPrefix)
	if err := pes.pool.write(context.Background(), func(w *Write) error {
		return upsertAgentQuota(context.Background(), w, &domain.AgentQuota{
			ID: qid, AgentID: id, PlatformExperimentID: pe, GuaranteedAcceleratorHours: 2, CreatedAt: time.Now().UTC(),
		})
	}); err != nil {
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
	pe, pes := poolRun(t, pool, 4, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 4}})
	atWork, finished, stopped, cut := spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if err := pes.FinishAgent(ctx, pe, finished, "nothing left"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO agentsandbox_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, stopped)
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, pe, cut)

	state, err := pes.AgentPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	var endedIDs []string
	for _, r := range state.Ended[generalist] {
		endedIDs = append(endedIDs, r.AgentID)
	}
	if state.MaxAgents != 4 || state.Pool.Executors() != 4 || state.AtWork[generalist] != 1 || len(endedIDs) != 2 || endedIDs[0] != finished || endedIDs[1] != stopped {
		t.Fatalf("state = %+v, ended %v, want max_agents 4, one at work (%s) and the finished and stopped ones in that order", state, endedIDs, atWork)
	}

	before := cursorNow(t, pool)
	replacement, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: replacement, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: replacement, Role: domain.SignupRoleExecutor, InAgentSandbox: true, AgentType: "generalist", Model: "model-1", Replaces: stopped},
		LateJoin{Used: map[string]float64{}}); err != nil {
		t.Fatalf("a replacement: got = %v, want = nil", err)
	}
	replaced := changesAfter(t, pes, domain.EventFilter{PlatformExperimentID: pe, Kinds: map[string]bool{platformapi.EventAgentReplaced: true}}, before)
	if len(replaced) != 1 || replaced[0].Subject != replacement || replaced[0].Value != stopped || replaced[0].Detail != "its machine stopped with no restart left" {
		t.Errorf("agent.replaced: got = %+v, want the stopped one and why its place was free, once", replaced)
	}

	state, err = pes.AgentPoolState(ctx, pe)
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
			t.Errorf("the launcher still wants the replaced executor %s", stopped)
		}
	}
	// An executor is replaced once.
	twice, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: twice, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: twice, Role: domain.SignupRoleExecutor, InAgentSandbox: true, AgentType: "generalist", Model: "model-1", Replaces: stopped},
		LateJoin{Used: map[string]float64{}}); err == nil {
		t.Error("a second replacement of one executor was written")
	}
}

// A boundary lowers the pool by the executors it cuts, in the write that cuts: nobody is added in
// their places. An executor of no entry lowers nothing.
func TestACutLowersThePoolInItsOwnWrite(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 6, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 3}, {AgentType: "data-search", Model: "model-1", Count: 2}})
	a, b := spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "data-search")
	extra := spawnExecutor(t, pes, pe, "architecture-search")
	survivor := spawnExecutor(t, pes, pe, "generalist")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if _, err := pes.AdvanceStage(ctx, pe, 1, []string{a, b, extra}, []string{survivor}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents[0].Count != 2 || got.Agents[1].Count != 1 || got.MaxAgents != 6 {
		t.Errorf("agents %+v, max_agents %d, want 2 and 1 after the cut and the cap as it was", got.Agents, got.MaxAgents)
	}
}

// max_agents and the pool are held to the executors at work, the count a join is held to: one
// that finished or was cut takes no place, and the refusal says how many are at work.
func TestMaxAgentsIsHeldToTheExecutorsAtWork(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 4, nil)
	spawnExecutor(t, pes, pe, "generalist")
	spawnExecutor(t, pes, pe, "generalist")
	finished, cut := spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist")

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
	if err := pes.UpdatePlatformExperiment(ctx, got, domain.PlatformExpRunning, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 1}}); !errors.As(err, &below) || below.Key.AgentType != "generalist" {
		t.Fatalf("an entry below its executors at work: got = %v, want PoolBelowAtWorkError of the entry", err)
	}
	if stored, _ := pes.GetPlatformExperiment(ctx, pe); stored.MaxAgents != 2 || len(stored.Agents) != 0 {
		t.Errorf("stored %d and %+v, want the refused write to leave max_agents 2 and no executors", stored.MaxAgents, stored.Agents)
	}
}

// A max_agents lowered below the executors at work of a running platform experiment cuts the
// newest of them, as the platform's decision written as their end: each is told agent.cut with
// why, the ladder does not move, and nobody takes their places.
func TestALoweredMaxAgentsCutsTheNewestExecutorsAndNobodyReplacesThem(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	dataSearch := domain.PoolKey{AgentType: "data-search", Model: "model-1"}
	pe, pes := poolRun(t, pool, 4, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 3}, {AgentType: "data-search", Model: "model-1", Count: 1}})
	first := spawnExecutor(t, pes, pe, "generalist")
	second := spawnExecutor(t, pes, pe, "generalist")
	third := spawnExecutor(t, pes, pe, "generalist")
	newest := spawnExecutor(t, pes, pe, "data-search")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	before := cursorNow(t, pool)

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
	state, err := pes.AgentPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if state.MaxAgents != 2 || state.AtWork[generalist] != 2 || state.AtWork[dataSearch] != 0 || len(state.Ended[generalist])+len(state.Ended[dataSearch]) != 0 {
		t.Fatalf("state = %+v, want two at work and nobody whose place is free", state)
	}
	if state.Pool.Executors() != 2 || len(poolPlaces(state)) != 0 {
		t.Errorf("pool %+v, want them lowered by the two cut, so that nobody is added", state.Pool)
	}

	told := map[string]string{}
	for _, e := range changesAfter(t, pes, domain.EventFilter{PlatformExperimentID: pe}, before) {
		if e.Kind == platformapi.EventStageBoundary {
			t.Errorf("a cut for max_agents moved the ladder: %+v", e)
		}
		if e.Kind == platformapi.EventAgentCut {
			if _, again := told[e.AgentID]; again {
				t.Errorf("%s told of its cut twice", e.AgentID)
			}
			told[e.AgentID] = e.Detail
		}
	}
	if len(told) != 2 || told[third] != CutMaxAgentsLowered || told[newest] != CutMaxAgentsLowered {
		t.Errorf("told %v, want %s to %s and %s", told, CutMaxAgentsLowered, third, newest)
	}
}

// A platform experiment and the executors it is created with are written together or not at
// all: one machine too many leaves nothing.
func TestAPlatformExperimentIsCreatedWithItsExecutorsOrNotAtAll(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	create := func(maxAgentSandboxes, executors int) (string, error) {
		now := time.Now().UTC()
		pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "pool", BudgetAcceleratorHours: 10,
			MaxAgents: executors, Agents: domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: executors}},
			StartsAt: now, DurationHours: 24, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, ReportIntervalSeconds: 30,
			Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now, MaxAgentSandboxes: maxAgentSandboxes,
			CoordinatorInAgentSandbox: true, Coordinator: &domain.AgentSandboxCoordinator{Model: "model-1"},
		}
		var agents []*domain.Agent
		var signups []domain.AgentSignup
		for i := 0; i <= executors; i++ {
			id, _ := domain.NewID(domain.AgentIDPrefix)
			role := domain.SignupRoleExecutor
			if i == executors {
				role = domain.SignupRoleBaseline
			}
			agents = append(agents, &domain.Agent{ID: id, CreatedAt: now})
			signups = append(signups, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role, InAgentSandbox: true, Model: "model-1"})
		}
		return pe.ID, pes.CreatePlatformExperiment(ctx, pe, agents, signups)
	}
	id, err := create(5, 3)
	if err != nil {
		t.Fatalf("three executors, a baseline agent and a coordinator on five machines: got = %v, want = nil", err)
	}
	if signups, err := pes.ListAgentSignups(ctx, id); err != nil || len(signups) != 4 {
		t.Errorf("signups %d %v, want all four", len(signups), err)
	}
	id, err = create(4, 3)
	if !errors.Is(err, ErrMaxAgentSandboxesReached) {
		t.Fatalf("one machine too many: got = %v, want ErrMaxAgentSandboxesReached", err)
	}
	if got, err := pes.GetPlatformExperiment(ctx, id); err != nil || got != nil {
		t.Errorf("after the refusal: got = %+v %v, want no platform experiment", got, err)
	}
}

func poolPlaces(state PoolState) []domain.PoolPlace {
	places, _ := domain.PoolDeficit(state.Pool, state.MaxAgents, state.AtWork, nil, state.Recent)
	return places
}

func spawnResearcher(t *testing.T, pes *Store, pe string) string {
	t.Helper()
	id, _ := domain.NewID(domain.AgentIDPrefix)
	agent := &domain.Agent{ID: id, Name: "r-" + id, CreatedAt: time.Now().UTC()}
	if err := pes.SpawnAgent(context.Background(), pe, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleResearcher, InAgentSandbox: true, AgentType: domain.ResearcherAgentType}); err != nil {
		t.Fatalf("spawn a researcher: got = %v, want = nil", err)
	}
	return id
}

func poolOf(t *testing.T, pes *Store, pe string) domain.AgentPool {
	t.Helper()
	got, err := pes.GetPlatformExperiment(context.Background(), pe)
	if err != nil {
		t.Fatal(err)
	}
	return got.Agents
}

// A researcher removed before the start left its entry as it was, and the start added another in
// its place. Its removal lowers its entry as an executor's does.
func TestARemovedResearcherLowersItsEntryOfThePool(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 4, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 1}, {AgentType: domain.ResearcherAgentType, Count: 1}})
	spawnExecutor(t, pes, pe, "generalist")
	researcher := spawnResearcher(t, pes, pe)
	if err := pes.RemoveAgent(ctx, pe, researcher); err != nil {
		t.Fatal(err)
	}
	if agents := poolOf(t, pes, pe); agents[0].Count != 1 || agents[1].Count != 0 {
		t.Errorf("agents = %+v, want the executor's entry as it was and the researcher's lowered to 0", agents)
	}
}

// A researcher's place is kept as an executor's: one that finished and one whose machine stopped
// both leave the researchers' entry as it was and wait to be replaced.
func TestAResearcherThatFinishedIsReplacedAsOneWhoseMachineStopped(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	researcher := domain.PoolKey{AgentType: domain.ResearcherAgentType}
	pe, pes := poolRun(t, pool, 4, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 1}, {AgentType: domain.ResearcherAgentType, Count: 2}})
	spawnExecutor(t, pes, pe, "generalist")
	finished, stopped := spawnResearcher(t, pes, pe), spawnResearcher(t, pes, pe)
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if err := pes.FinishAgent(ctx, pe, finished, "my context is spent"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO agentsandbox_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, finished)
	exec(t, pool, `INSERT INTO agentsandbox_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, stopped)
	state, err := pes.AgentPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	var ended []string
	for _, e := range state.Ended[researcher] {
		ended = append(ended, e.AgentID)
	}
	slices.Sort(ended)
	want := []string{finished, stopped}
	slices.Sort(want)
	if state.Pool[1].Count != 2 || !slices.Equal(ended, want) {
		t.Errorf("state = %+v, want the researchers' entry kept at 2 and %v waiting to be replaced", state, want)
	}
	if places := poolPlaces(state); len(places) != 2 {
		t.Errorf("places = %+v, want two: both researchers'", places)
	}
}

// A merge checked that no job was on its hypothesis and committed while a submit on it, which
// had read it open, was writing its job. The submit's last check runs under the hypothesis's
// lock, which the curation holds from its check of the jobs to its commit: of the two, the later
// sees the earlier.
func TestACurationAndASubmitOnOneHypothesisSeeEachOther(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	exp.HypothesisID = "h-" + exp.ID
	ctx := context.Background()
	held, release := make(chan struct{}), make(chan struct{})
	curated := make(chan error, 1)
	var committed atomic.Bool
	go func() {
		curated <- store.AroundHypothesis(ctx, exp.PlatformExperimentID, exp.HypothesisID, func(domain.Announcer) error {
			close(held)
			<-release
			committed.Store(true)
			return nil
		})
	}()
	<-held
	admitted := make(chan error, 1)
	var sawCuration bool
	go func() {
		_, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, func(context.Context) error {
			sawCuration = committed.Load()
			return nil
		})
		admitted <- err
	}()
	select {
	case err := <-admitted:
		t.Fatalf("the submit ended while the curation held the hypothesis: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-curated; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; err != nil || !sawCuration {
		t.Fatalf("the submit: got = %v, saw the curation %v, want its last check after the curation committed", err, sawCuration)
	}

	// The other way round: a curation that starts while the job is written finds it.
	second := *exp
	second.ID, second.DataKey = exp.ID+"-2", domain.DataKey{AccessKeyID: "AK2-" + exp.ID, SecretAccessKey: "SK2-" + exp.ID}
	held, release = make(chan struct{}), make(chan struct{})
	go func() {
		_, err := store.AdmitExperimentTx(ctx, &second, noUsage, SubmissionRateLimit{}, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
		admitted <- err
	}()
	<-held
	var found []*domain.Experiment
	go func() {
		curated <- store.AroundHypothesis(ctx, exp.PlatformExperimentID, exp.HypothesisID, func(domain.Announcer) error {
			var err error
			found, err = store.ListExperimentsTesting(ctx, []string{exp.HypothesisID})
			return err
		})
	}()
	select {
	case err := <-curated:
		t.Fatalf("the curation ended while the submit held the hypothesis: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if err := <-curated; err != nil || len(found) != 2 {
		t.Fatalf("the curation: got = %v with %d jobs found, want both jobs on the hypothesis", err, len(found))
	}
}
