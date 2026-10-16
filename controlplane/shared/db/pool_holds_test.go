package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A researcher that ended before submitting a job, or within domain.ReplacementMinRun of joining,
// is read as early; one that ran longer with a job is not, and the replacements of an entry in the
// last hour are counted.
func TestThePoolStateTellsEarlyEndsAndCountsRecentReplacements(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	pe, pes := poolRun(t, pool, 6, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 4}})
	noJob, quick, ran, replaced := spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	seedPricedExperiment(t, pool, quick, pe, quotaL40, 1, 1, domain.StatusCompleted)
	seedPricedExperiment(t, pool, ran, pe, quotaL40, 1, 1, domain.StatusCompleted)
	exec(t, pool, `UPDATE experiment_signups SET signed_up_at = now() - interval '1 hour' WHERE platform_experiment_id = $1 AND agent_id IN ($2, $3)`, pe, noJob, ran)
	for _, id := range []string{noJob, quick, ran, replaced} {
		if err := pes.FinishAgent(ctx, pe, id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	replacement, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: replacement, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: replacement, Role: domain.SignupRoleCompetitor, Hosted: true, AgentType: "generalist", Model: "model-1", Replaces: replaced},
		LateJoin{Used: map[string]float64{}}); err != nil {
		t.Fatal(err)
	}
	state, err := pes.ResearcherPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	early := map[string]bool{}
	for _, r := range state.Ended[generalist] {
		early[r.AgentID] = r.Early
	}
	if len(early) != 3 || !early[noJob] || !early[quick] || early[ran] {
		t.Errorf("early: got = %v, want %s (no job) and %s (quick) early and %s not", early, noJob, quick, ran)
	}
	if state.Recent[generalist] != 1 {
		t.Errorf("recent: got = %v, want the one replacement", state.Recent)
	}
}

// With the platform experiment locked, a researcher added to keep the pool is refused when its
// entry wants none beyond those at work, or when it would be the entry's replacement beyond the
// limit an hour.
func TestAResearcherAddedToKeepThePoolIsCheckedAgainstItUnderTheLock(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 10, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 1}})
	spawnResearcher(t, pes, pe, "generalist")
	ending := make([]string, domain.MaxReplacementsAnHour+1)
	for i := range ending {
		ending[i] = spawnResearcher(t, pes, pe, "generalist")
	}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	join := func(replaces string) error {
		id, _ := domain.NewID(domain.AgentIDPrefix)
		return pes.JoinRunning(ctx, pe, &domain.Agent{ID: id, CreatedAt: time.Now().UTC()},
			domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleCompetitor, Hosted: true, AgentType: "generalist", Model: "model-1", Replaces: replaces},
			LateJoin{Used: map[string]float64{}, Pool: true})
	}
	if err := join(""); !errors.Is(err, ErrPoolSatisfied) {
		t.Fatalf("an entry with all at work: got = %v, want ErrPoolSatisfied", err)
	}
	exec(t, pool, `UPDATE platform_experiments SET researchers = '[{"agent_type":"generalist","model":"model-1","count":10}]' WHERE id = $1`, pe)
	for _, ended := range ending {
		if err := pes.FinishAgent(ctx, pe, ended, "done"); err != nil {
			t.Fatal(err)
		}
	}
	for n, ended := range ending[:domain.MaxReplacementsAnHour] {
		if err := join(ended); err != nil {
			t.Fatalf("replacement %d: got = %v, want = nil", n+1, err)
		}
	}
	if err := join(ending[domain.MaxReplacementsAnHour]); !errors.Is(err, ErrPoolSatisfied) {
		t.Errorf("replacement %d in an hour: got = %v, want ErrPoolSatisfied", domain.MaxReplacementsAnHour+1, err)
	}
	if err := join(""); err != nil {
		t.Errorf("a place the pool gained: got = %v, want it added", err)
	}
}

// What the platform says of the places it keeps empty is told once as platform_experiment.status
// with it as the detail, live and replayed.
func TestHeldResearchersAreToldOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 4, nil)
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	before := cursorNow(t, pool)
	live, stop := listenForEvents(t, pool)
	defer stop()
	line := "researchers not replaced: ag-1 (generalist) ended early"
	for range 2 {
		if err := pes.HoldResearchers(ctx, pe, line); err != nil {
			t.Fatal(err)
		}
	}
	got := awaitEvent(t, live, domain.EventPlatformExperimentStatus, pe)
	if got.Detail != line || got.Value != string(domain.PlatformExpRunning) {
		t.Errorf("live: got = %+v", got)
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: pe}, before)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range replayed {
		if e.Kind == domain.EventPlatformExperimentStatus && e.Detail == line {
			n++
		}
	}
	if n != 1 {
		t.Errorf("replayed %d, want the hold once: %+v", n, replayed)
	}
	if err := pes.HoldResearchers(ctx, pe, ""); err != nil {
		t.Fatal(err)
	}
	if state, err := pes.ResearcherPoolState(ctx, pe); err != nil || state.Held != "" {
		t.Errorf("cleared: got = %+v, %v", state, err)
	}
}
