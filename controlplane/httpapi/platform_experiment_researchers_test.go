package httpapi

import (
	"net/http"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A create with a hosted coordinator states who starts in researchers, and the platform creates
// them in the same write as the experiment and its baseline agent, each with its entry's agent
// type, model and hyperparameters: nothing is left for the browser to add one call at a time.
func TestCreateWithAHostedCoordinatorCreatesItsResearchersInTheSameWrite(t *testing.T) {
	store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	code, out := create(run(map[string]any{"coordinator_hosted": true, "max_agents": 4, "researchers": []map[string]any{
		{"count": 2},
		{"agent_type": "data-search", "model": "model-2", "hyperparameters": map[string]any{"lr": 0.1}, "count": 1},
	}}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	pe := store.created[0]
	want := domain.ResearcherPool{
		{AgentType: "generalist", Count: 2},
		{AgentType: "data-search", Model: "model-2", Hyperparameters: map[string]any{"lr": 0.1}, Count: 1},
	}
	if len(pe.Researchers) != 2 || pe.Researchers[0].Key() != want[0].Key() || pe.Researchers[1].Key() != want[1].Key() || pe.Researchers.Total() != 3 || pe.MaxAgents != 4 {
		t.Fatalf("stored researchers %+v and max_agents %d, want %+v resolved and the cap of 4 kept apart", pe.Researchers, pe.MaxAgents, want)
	}
	if len(store.researchers) != 3 || len(store.baselines) != 1 || out["signup_count"] != float64(4) {
		t.Errorf("created %d researchers and %d baseline agents (answer %v), want 3 and 1 in the one create", len(store.researchers), len(store.baselines), out["signup_count"])
	}
}

// Every answer about a platform experiment counts the researchers at work, as the store counts them,
// against each entry of the pool in its order and outside it.
func TestTheAnswerCountsTheResearchersAtWorkPerPoolEntryAndOutsideIt(t *testing.T) {
	store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	store.atWork = map[domain.PoolKey]int{
		{AgentType: "generalist"}:            2,
		{AgentType: "hyperparameter-search"}: 3,
	}
	code, out := create(run(map[string]any{"coordinator_hosted": true, "max_agents": 6, "researchers": []map[string]any{
		{"count": 2},
		{"agent_type": "data-search", "count": 1},
	}}))
	if code != http.StatusCreated {
		t.Fatalf("got = %d %v, want 201", code, out)
	}
	counts, _ := out["researchers_at_work"].(map[string]any)
	pool, _ := counts["pool"].([]any)
	if counts["total"] != float64(5) || counts["outside_pool"] != float64(3) || len(pool) != 2 ||
		pool[0].(map[string]any)["at_work"] != float64(2) || pool[1].(map[string]any)["at_work"] != float64(0) {
		t.Errorf("researchers_at_work = %v, want 5 in all, 2 and 0 in the pool entries, 3 outside the pool", counts)
	}
}

// What the pool may say is checked before anything is created.
func TestResearchersAreCheckedBeforeAnythingIsCreated(t *testing.T) {
	for name, researchers := range map[string][]map[string]any{
		"more than max_agents":  {{"count": 4}},
		"no agent type":         {{"agent_type": "poet", "count": 1}},
		"a count of none":       {{"count": 0}},
		"a model no agent runs": {{"model": "a model with spaces", "count": 1}},
	} {
		store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
		code, out := create(run(map[string]any{"coordinator_hosted": true, "researchers": researchers}))
		if code != http.StatusUnprocessableEntity || out["reason"] != "invalid_agents" || len(store.created) != 0 {
			t.Errorf("%s: got = %d %v, created %d, want 422 invalid_agents and nothing", name, code, out, len(store.created))
		}
	}
	// Researchers are optional: the cap alone creates no researcher.
	store, create := hostingAPI(t, &fakeRepositories{files: map[string]string{"prj-a": experimentBase}})
	if code, out := create(run(map[string]any{"coordinator_hosted": true})); code != http.StatusCreated || len(store.researchers) != 0 {
		t.Errorf("without researchers: got = %d %v and %d researchers, want 201 and none", code, out, len(store.researchers))
	}
}
