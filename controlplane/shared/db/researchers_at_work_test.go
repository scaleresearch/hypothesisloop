package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Every agent says whether it is at work and why not, and the platform experiment counts those at
// work by the same rule: a finished, cut, replaced or machineless researcher is no one at work, and a
// researcher of an agent type the pool names no entry of counts outside the pool.
func TestResearchersAtWorkAreCountedByTheOneRuleForEachAgentAndTheWholePool(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe, pes := poolRun(t, pool, 8, domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 4}, {AgentType: "data-search", Model: "model-1", Count: 2}})
	working, finished, stopped, cut, replaced := spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"),
		spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist"), spawnResearcher(t, pes, pe, "generalist")
	other := spawnResearcher(t, pes, pe, "data-search")
	added := spawnResearcher(t, pes, pe, "hyperparameter-search")
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if err := pes.FinishAgent(ctx, pe, finished, "nothing left"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO hosted_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, stopped)
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, pe, cut)
	exec(t, pool, `INSERT INTO hosted_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, pe, replaced)
	replacement, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: replacement, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: replacement, Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: "main", AgentType: "generalist", Model: "model-1", Replaces: replaced},
		LateJoin{Used: map[string]float64{}}); err != nil {
		t.Fatal(err)
	}

	signups, err := pes.ListAgentSignups(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	type state struct {
		atWork bool
		ended  string
	}
	got := map[string]state{}
	for _, s := range signups {
		got[s.AgentID] = state{s.AtWork, s.EndedReason}
	}
	want := map[string]state{
		working: {true, ""}, finished: {false, "finished"}, stopped: {false, "stopped"}, cut: {false, "cut"},
		replaced: {false, "replaced"}, other: {true, ""}, added: {true, ""}, replacement: {true, ""},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("agent %s: got = %+v, want = %+v", id, got[id], w)
		}
	}
	single, err := pes.GetAgentSignup(ctx, pe, cut)
	if err != nil || single.AtWork || single.EndedReason != "cut" {
		t.Errorf("one signup read: got = %+v, %v, want not at work, cut", single, err)
	}

	atWork, err := pes.ResearchersAtWork(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	counts := domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 4}, {AgentType: "data-search", Model: "model-1", Count: 2}}.AtWorkOf(atWork)
	if counts.Total != 4 || counts.OutsidePool != 1 || counts.Pool[0].AtWork != 2 || counts.Pool[1].AtWork != 1 {
		t.Errorf("counts: got = %+v, want 4 at work, generalist 2, data-search 1, 1 outside the pool", counts)
	}
	state2, err := pes.ResearcherPoolState(ctx, pe)
	if err != nil {
		t.Fatal(err)
	}
	if len(state2.AtWork) != len(atWork) {
		t.Errorf("the pool state counts %v and the read %v: want one rule", state2.AtWork, atWork)
	}
}

func TestADescriptionIsSeededForABuiltinAndWrittenOnAnOrgsType(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	builtin := agenttypes.Builtin{ID: "test-" + uuid.New().String()[:8], Name: "Test", Description: "First.", Role: domain.AgentRoleResearcher, Prompt: testPrompt, Settings: testTypeSettings()}
	if _, err := SeedBuiltinAgentTypes(ctx, pool, []agenttypes.Builtin{builtin}, testModelDefaults); err != nil {
		t.Fatal(err)
	}
	read := func() *domain.CurrentAgentType {
		t.Helper()
		got, err := store.Get(ctx, "", builtin.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got.Description != "First." {
		t.Fatalf("seeded: got description %q", got.Description)
	}
	builtin.Description = "Second."
	seeded, err := SeedBuiltinAgentTypes(ctx, pool, []agenttypes.Builtin{builtin}, testModelDefaults)
	if err != nil || len(seeded) != 0 {
		t.Fatalf("seed with a new description: %+v, %v, want no new version", seeded, err)
	}
	if got := read(); got.Description != "Second." || got.CurrentVersion != 1 {
		t.Errorf("reseeded: got description %q at version %d, want the new one at version 1", got.Description, got.CurrentVersion)
	}

	orgID, _, custom := orgWithExperiment(t, pool)
	if custom.Description != "Tunes the knobs." {
		t.Errorf("created: got description %q", custom.Description)
	}
	edited, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Description: "Changed.", Prompt: testPrompt, Settings: testTypeSettings()}, testModelDefaults)
	if err != nil || edited.Description != "Changed." || edited.CurrentVersion != 1 {
		t.Errorf("edited: got = %+v, %v, want the description changed at version 1", edited, err)
	}
	listed, err := store.List(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range listed {
		if ty.ID == custom.ID && ty.Description != "Changed." {
			t.Errorf("listed: got description %q", ty.Description)
		}
	}
}
