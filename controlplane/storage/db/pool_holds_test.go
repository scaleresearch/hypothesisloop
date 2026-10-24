package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// An executor that ended before submitting a job, or within domain.ReplacementMinRun of joining,
// is read as early; one that ran longer with a job is not, and the replacements of an entry in the
// last hour are counted.
func TestThePoolStateTellsEarlyEndsAndCountsRecentReplacements(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	pe, pes := poolRun(t, pool, 6, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 4}})
	noJob, quick, ran, replaced := spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist"), spawnExecutor(t, pes, pe, "generalist")
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
		domain.AgentSignup{Actor: testActor, AgentID: replacement, Role: domain.SignupRoleExecutor, InAgentSandbox: true, AgentType: "generalist", Model: "model-1", Replaces: replaced},
		LateJoin{Used: map[string]float64{}}); err != nil {
		t.Fatal(err)
	}
	state, err := pes.AgentPoolState(ctx, pe)
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

// With the platform experiment locked, an agent added to keep the pool is refused when its
// entry wants none beyond those at work, or when it would be the entry's replacement beyond the
// limit an hour.
func TestAnAgentAddedToKeepThePoolIsCheckedAgainstItUnderTheLock(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 10, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 1}})
	spawnExecutor(t, pes, pe, "generalist")
	ending := make([]string, domain.MaxReplacementsAnHour+1)
	for i := range ending {
		ending[i] = spawnExecutor(t, pes, pe, "generalist")
	}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	join := func(replaces string) error {
		id, _ := domain.NewID(domain.AgentIDPrefix)
		return pes.JoinRunning(ctx, pe, &domain.Agent{ID: id, CreatedAt: time.Now().UTC()},
			domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleExecutor, InAgentSandbox: true, AgentType: "generalist", Model: "model-1", Replaces: replaces},
			LateJoin{Used: map[string]float64{}, Pool: true})
	}
	if err := join(""); !errors.Is(err, ErrPoolSatisfied) {
		t.Fatalf("an entry with all at work: got = %v, want ErrPoolSatisfied", err)
	}
	exec(t, pool, `UPDATE platform_experiments SET agents = '[{"agent_type":"generalist","model":"model-1","count":10}]' WHERE id = $1`, pe)
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
// with it as the detail.
func TestHeldAgentsAreToldOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 4, nil)
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	before := cursorNow(t, pool)
	line := "agents not replaced: ag-1 (generalist) ended early"
	for range 2 {
		if err := pes.HoldAgents(ctx, pe, line); err != nil {
			t.Fatal(err)
		}
	}
	got := changesAfter(t, pes, domain.EventFilter{PlatformExperimentID: pe, Kinds: map[string]bool{platformapi.EventPlatformExperimentStatus: true}}, before)
	if len(got) != 1 || got[0].Detail != line || got[0].Value != string(domain.PlatformExpRunning) {
		t.Errorf("status changes: got = %+v, want the hold once", got)
	}
	if err := pes.HoldAgents(ctx, pe, ""); err != nil {
		t.Fatal(err)
	}
	if state, err := pes.AgentPoolState(ctx, pe); err != nil || state.Held != "" {
		t.Errorf("cleared: got = %+v, %v", state, err)
	}
}
