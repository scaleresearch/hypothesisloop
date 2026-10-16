package quota

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

// keepStore is what KeepResearchers reads and writes: the state of one running platform
// experiment, and the researchers it joins.
type keepStore struct {
	createdStore
	state  db.PoolState
	joined []domain.AgentSignup
	// joinErr refuses every join, as the store refuses one the budget holds no share for.
	joinErr error
	// held is every line HoldResearchers wrote, and pool whether each join asked for the pool's check.
	held []string
	pool []bool
}

func (s *keepStore) HoldResearchers(_ context.Context, _, line string) error {
	s.held = append(s.held, line)
	return nil
}

func (s *keepStore) ResearcherPoolState(context.Context, string) (db.PoolState, error) {
	return s.state, nil
}

func (s *keepStore) LauncherKnown(context.Context, string) (bool, error) { return true, nil }

func (s *keepStore) ListSignupsByRole(context.Context, string, domain.SignupRole) ([]string, error) {
	return nil, nil
}

func (s *keepStore) JoinRunning(_ context.Context, _ string, _ *domain.Agent, signup domain.AgentSignup, late db.LateJoin) error {
	s.pool = append(s.pool, late.Pool)
	if s.joinErr != nil {
		return s.joinErr
	}
	s.joined = append(s.joined, signup)
	return nil
}

func TestKeepResearchersReplacesWhoEndedAndNobodyElse(t *testing.T) {
	generalist := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	searcher := domain.PoolKey{AgentType: "data-search", Model: "model-1"}
	ended := func(id string, key domain.PoolKey) db.EndedResearcher {
		r := db.EndedResearcher{Key: key}
		r.AgentID, r.Hyperparameters, r.QuotaTier = id, map[string]any{"area": id}, domain.QuotaTierBurstOnly
		return r
	}
	pool := domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Count: 2}, {AgentType: "data-search", Model: "model-1", Count: 1}}
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		state    db.PoolState
		endsIn   time.Duration
		hosted   bool
		joinErr  error
		replaces []string
	}{
		"a researcher whose machine stopped for good": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedResearcher{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, hosted: true, replaces: []string{"ag-stopped"},
		},
		"a researcher that finished": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 2}, Ended: map[domain.PoolKey][]db.EndedResearcher{searcher: {ended("ag-finished", searcher)}}},
			endsIn: 10 * time.Hour, hosted: true, replaces: []string{"ag-finished"},
		},
		// The cut lowered the pool in its own write: the pool read wants one fewer.
		"a cut researcher": {
			state:  db.PoolState{Pool: pool.Lowered([]domain.PoolKey{generalist}), MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}},
			endsIn: 10 * time.Hour, hosted: true,
		},
		"the last minutes of the run": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedResearcher{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Minute, hosted: true,
		},
		"a budget that holds no share for a newcomer": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedResearcher{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, hosted: true, joinErr: db.ErrNoShareLeft,
		},
		// The coordinator's own addition took the place up to max_agents.
		"max_agents taken by an addition of the coordinator": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1, {AgentType: "architecture-search", Model: "model-1"}: 1}, Ended: map[domain.PoolKey][]db.EndedResearcher{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, hosted: true,
		},
		"researchers that run themselves": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedResearcher{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour,
		},
	} {
		store := &keepStore{state: c.state, joinErr: c.joinErr}
		svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), noUsage(t), time.Minute)
		pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorHosted: c.hosted, EndsAt: now.Add(c.endsIn), JobEstimatedDurationHours: 0.5,
			Actor: &domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
		if err := svc.KeepResearchers(context.Background(), pe, AgentLaunch{Launcher: "main", DefaultModel: "model-1"}, now); err != nil {
			t.Errorf("%s: got = %v, want nil", name, err)
			continue
		}
		var replaced []string
		for _, signup := range store.joined {
			replaced = append(replaced, signup.Replaces)
			if !signup.Hosted || signup.Role != domain.SignupRoleCompetitor || signup.Launcher != "main" || signup.Hyperparameters["area"] != signup.Replaces || signup.QuotaTier != domain.QuotaTierBurstOnly {
				t.Errorf("%s: added %+v, want a hosted researcher with what the one it replaces had", name, signup)
			}
		}
		if len(replaced) != len(c.replaces) || (len(replaced) > 0 && replaced[0] != c.replaces[0]) {
			t.Errorf("%s: replaced %v, want %v", name, replaced, c.replaces)
		}
	}
}

// A pool raised while the run goes on gets new researchers of its entry, in places nobody left.
func TestKeepResearchersFillsAPlaceThePoolGained(t *testing.T) {
	key := domain.PoolKey{AgentType: "generalist", Model: "model-1"}
	store := &keepStore{state: db.PoolState{Pool: domain.ResearcherPool{{AgentType: "generalist", Model: "model-1", Hyperparameters: map[string]any{"from": "entry"}, Count: 3}}, MaxAgents: 5, AtWork: map[domain.PoolKey]int{key: 1}}}
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), noUsage(t), time.Minute)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorHosted: true, EndsAt: now.Add(time.Hour)}
	if err := svc.KeepResearchers(context.Background(), pe, AgentLaunch{Launcher: "main", DefaultModel: "model-1"}, now); err != nil {
		t.Fatal(err)
	}
	if len(store.joined) != 2 || store.joined[0].Replaces != "" || store.joined[0].Hyperparameters["from"] != "entry" {
		t.Errorf("added %+v, want two researchers with the entry's hyperparameters, replacing nobody", store.joined)
	}
}

// A researcher that ended early is not replaced, nor one beyond the limit of an entry an hour: the
// places kept empty are written as one line, once while it stays the same and cleared once none
// is, and every researcher added asks the store to check its entry again under the lock.
func TestKeepResearchersHoldsThePlacesOfEarlyEndsAndAboveTheLimitAndSaysWhy(t *testing.T) {
	key := domain.PoolKey{AgentType: "generalist"}
	ended := func(id string, early bool) db.EndedResearcher {
		r := db.EndedResearcher{Key: key, Early: early}
		r.AgentID = id
		return r
	}
	store := &keepStore{state: db.PoolState{
		Pool: domain.ResearcherPool{{AgentType: "generalist", Count: 4}}, MaxAgents: 10, AtWork: map[domain.PoolKey]int{},
		Ended:  map[domain.PoolKey][]db.EndedResearcher{key: {ended("ag-early", true), ended("ag-1", false), ended("ag-2", false), ended("ag-3", false)}},
		Recent: map[domain.PoolKey]int{key: domain.MaxReplacementsAnHour - 1},
	}}
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), noUsage(t), time.Minute)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorHosted: true, EndsAt: now.Add(10 * time.Hour)}
	keep := func() {
		t.Helper()
		if err := svc.KeepResearchers(context.Background(), pe, AgentLaunch{Launcher: "main", DefaultModel: "model-1"}, now); err != nil {
			t.Fatal(err)
		}
	}
	keep()
	if len(store.joined) != 1 || store.joined[0].Replaces != "ag-1" || len(store.pool) != 1 || !store.pool[0] {
		t.Errorf("joined %+v (pool checks %v), want ag-1 replaced under the pool's check", store.joined, store.pool)
	}
	if len(store.held) != 1 || !strings.Contains(store.held[0], "ag-early (generalist) ended before submitting a job") || !strings.Contains(store.held[0], "ag-2 (generalist), ag-3 (generalist) wait to be replaced") {
		t.Fatalf("held %q", store.held)
	}
	store.state.Held = store.held[0]
	keep()
	if len(store.held) != 1 {
		t.Errorf("the same holds again: written %q, want nothing more", store.held)
	}
	store.state.Pool = domain.ResearcherPool{{AgentType: "generalist", Count: 0}}
	keep()
	if len(store.held) != 2 || store.held[1] != "" {
		t.Errorf("no place held: written %q, want the line cleared", store.held)
	}
}
