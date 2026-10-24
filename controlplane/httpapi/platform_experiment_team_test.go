package httpapi

import (
	"net/http"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A create with a sandboxed coordinator states who starts in agents, and the platform creates
// them in the same write as the experiment and its baseline agent, each with its entry's agent
// type, model and hyperparameters: nothing is left for the browser to add one call at a time.
func TestCreateWithASandboxedCoordinatorCreatesItsAgentsInTheSameWrite(t *testing.T) {
	store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "max_agents": 4, "agents": []map[string]any{
		{"count": 2},
		{"agent_type": "data-search", "model": "anthropic/model-2", "hyperparameters": map[string]any{"lr": 0.1}, "count": 1},
	}}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	pe := store.created[0]
	want := domain.AgentPool{
		{AgentType: "generalist", Count: 2},
		{AgentType: "data-search", Model: "anthropic/model-2", Hyperparameters: map[string]any{"lr": 0.1}, Count: 1},
	}
	if len(pe.Agents) != 2 || pe.Agents[0].Key() != want[0].Key() || pe.Agents[1].Key() != want[1].Key() || pe.Agents.Executors() != 3 || pe.MaxAgents != 4 {
		t.Fatalf("stored agents %+v and max_agents %d, want %+v resolved and the cap of 4 kept apart", pe.Agents, pe.MaxAgents, want)
	}
	if len(store.agentIDs) != 3 || len(store.baselines) != 1 || out["signup_count"] != float64(4) {
		t.Errorf("created %d agents and %d baseline agents (answer %v), want 3 and 1 in the one create", len(store.agentIDs), len(store.baselines), out["signup_count"])
	}
}

// The team is one list: an entry of the built-in researcher type is created as researchers beside
// the executors, and max_agents bounds the executors alone.
func TestCreateMakesTheResearchersOfItsTeamBesideItsExecutors(t *testing.T) {
	store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "max_agents": 1, "agents": []map[string]any{
		{"agent_type": domain.ResearcherAgentType, "count": 2},
		{"count": 1},
	}}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	roles := map[domain.SignupRole]int{}
	for _, signup := range store.team {
		roles[signup.Role]++
		if (signup.Role == domain.SignupRoleResearcher) != (signup.AgentType == domain.ResearcherAgentType) {
			t.Errorf("signup %+v: want the researcher type for a researcher alone", signup)
		}
	}
	if roles[domain.SignupRoleResearcher] != 2 || roles[domain.SignupRoleExecutor] != 1 || len(store.team) != 3 {
		t.Errorf("team = %v, want two researchers and one executor", roles)
	}
	if code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "max_agents": 1, "agents": []map[string]any{{"count": 2}}})); code != http.StatusUnprocessableEntity || out["reason"] != "invalid_agents" {
		t.Errorf("two executors under max_agents 1: got = %d %v, want 422 invalid_agents", code, out)
	}
}

// Every answer about a platform experiment counts the agents at work, as the store counts them,
// against each entry of the pool in its order and outside it.
func TestTheAnswerCountsTheAgentsAtWorkPerPoolEntryAndOutsideIt(t *testing.T) {
	store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	store.atWork = map[domain.PoolKey]int{
		{AgentType: "generalist"}:            2,
		{AgentType: "hyperparameter-search"}: 3,
	}
	code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "max_agents": 6, "agents": []map[string]any{
		{"count": 2},
		{"agent_type": "data-search", "count": 1},
	}}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	counts, _ := out["agents_at_work"].(map[string]any)
	pool, _ := counts["pool"].([]any)
	if counts["total"] != float64(5) || counts["outside_team"] != float64(3) || len(pool) != 2 ||
		pool[0].(map[string]any)["at_work"] != float64(2) || pool[1].(map[string]any)["at_work"] != float64(0) {
		t.Errorf("agents_at_work = %v, want 5 in all, 2 and 0 in the pool entries, 3 outside the pool", counts)
	}
}

// What the pool may say is checked before anything is created.
func TestTheTeamIsCheckedBeforeAnythingIsCreated(t *testing.T) {
	for name, agents := range map[string][]map[string]any{
		"more than max_agents":  {{"count": 4}},
		"no agent type":         {{"agent_type": "poet", "count": 1}},
		"a count of none":       {{"count": 0}},
		"a model no agent runs": {{"model": "a model with spaces", "count": 1}},
	} {
		store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
		code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "agents": agents}))
		if code != http.StatusUnprocessableEntity || out["reason"] != "invalid_agents" || len(store.created) != 0 {
			t.Errorf("%s: got = %d %v, created %d, want 422 invalid_agents and nothing", name, code, out, len(store.created))
		}
	}
	for name, model := range map[string]string{"a model not listed": "anthropic/claude-fable-5-1", "a provider not served": "openrouter/moonshotai/kimi-k2"} {
		store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
		code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true, "agents": []map[string]any{{"count": 1}, {"model": model, "count": 1}}}))
		problems, _ := out["problems"].([]any)
		if code != http.StatusUnprocessableEntity || out["reason"] != "model_not_served" || len(problems) != 1 ||
			problems[0].(map[string]any)["field"] != "agents[1].model" || len(store.created) != 0 {
			t.Errorf("%s: got = %d %v, created %d, want 422 model_not_served on agents[1].model and nothing", name, code, out, len(store.created))
		}
	}
	// The team is optional: the cap alone creates no agent.
	store, create := agentSandboxAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	if code, out := create(run(map[string]any{"coordinator_in_agentsandbox": true})); code != http.StatusCreated || len(store.agentIDs) != 0 {
		t.Errorf("without a team: got = %d %v and %d agents, want 201 and none", code, out, len(store.agentIDs))
	}
}
