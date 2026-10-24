package platformexperiments

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// keepStore is what KeepAgents reads and writes: the state of one running platform
// experiment, and the agents it joins.
type keepStore struct {
	createdStore
	state  db.PoolState
	joined []domain.AgentSignup
	// joinErr refuses every join, as the store refuses one the budget holds no share for, and
	// executorErr every executor's.
	joinErr, executorErr error
	// asked counts the joins asked of each role.
	asked map[domain.SignupRole]int
	// held is every line HoldAgents wrote, and pool whether each join asked for the pool's check.
	held []string
	pool []bool
}

func (s *keepStore) HoldAgents(_ context.Context, _, line string) error {
	s.held = append(s.held, line)
	return nil
}

func (s *keepStore) AgentPoolState(context.Context, string) (db.PoolState, error) {
	return s.state, nil
}

func (s *keepStore) ListSignupsByRole(context.Context, string, domain.SignupRole) ([]string, error) {
	return nil, nil
}

func (s *keepStore) JoinRunning(_ context.Context, _ string, _ *domain.Agent, signup domain.AgentSignup, late db.LateJoin) error {
	s.pool = append(s.pool, late.Pool)
	if s.asked == nil {
		s.asked = map[domain.SignupRole]int{}
	}
	s.asked[signup.Role]++
	if s.joinErr != nil {
		return s.joinErr
	}
	if s.executorErr != nil && signup.Role == domain.SignupRoleExecutor {
		return s.executorErr
	}
	s.joined = append(s.joined, signup)
	return nil
}

func TestKeepAgentsReplacesWhoEndedAndNobodyElse(t *testing.T) {
	generalist := domain.PoolKey{AgentType: "generalist", Model: "anthropic/model-1"}
	searcher := domain.PoolKey{AgentType: "data-search", Model: "anthropic/model-1"}
	ended := func(id string, key domain.PoolKey) db.EndedAgent {
		r := db.EndedAgent{Key: key}
		r.AgentID, r.Hyperparameters, r.QuotaTier = id, map[string]any{"area": id}, domain.QuotaTierBurstOnly
		return r
	}
	pool := domain.AgentPool{{AgentType: "generalist", Model: "anthropic/model-1", Count: 2}, {AgentType: "data-search", Model: "anthropic/model-1", Count: 1}}
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		state     db.PoolState
		endsIn    time.Duration
		sandboxed bool
		joinErr   error
		replaces  []string
	}{
		"an executor whose machine stopped for good": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, sandboxed: true, replaces: []string{"ag-stopped"},
		},
		"an executor that finished": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 2}, Ended: map[domain.PoolKey][]db.EndedAgent{searcher: {ended("ag-finished", searcher)}}},
			endsIn: 10 * time.Hour, sandboxed: true, replaces: []string{"ag-finished"},
		},
		// The cut lowered the pool in its own write: the pool read wants one fewer.
		"a cut executor": {
			state:  db.PoolState{Pool: domain.AgentPool{{AgentType: "generalist", Model: "anthropic/model-1", Count: 1}, {AgentType: "data-search", Model: "anthropic/model-1", Count: 1}}, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}},
			endsIn: 10 * time.Hour, sandboxed: true,
		},
		"the last minutes of the run": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Minute, sandboxed: true,
		},
		"a budget that holds no share for a newcomer": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, sandboxed: true, joinErr: db.ErrNoShareLeft,
		},
		// The coordinator's own addition took the place up to max_agents.
		"max_agents taken by an addition of the coordinator": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1, {AgentType: "architecture-search", Model: "anthropic/model-1"}: 1}, Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour, sandboxed: true,
		},
		"executors that run themselves": {
			state:  db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1, searcher: 1}, Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-stopped", generalist)}}},
			endsIn: 10 * time.Hour,
		},
	} {
		store := &keepStore{state: c.state, joinErr: c.joinErr}
		svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
		pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorInAgentSandbox: c.sandboxed, EndsAt: now.Add(c.endsIn), JobEstimatedDurationHours: 0.5,
			Actor: &domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
		if err := svc.WithAgentLaunch(AgentLaunch{AgentSandbox: true, DefaultModel: "anthropic/model-1"}).KeepAgents(context.Background(), pe, now); err != nil {
			t.Errorf("%s: got = %v, want nil", name, err)
			continue
		}
		var replaced []string
		for _, signup := range store.joined {
			replaced = append(replaced, signup.Replaces)
			if !signup.InAgentSandbox || signup.Role != domain.SignupRoleExecutor || signup.Hyperparameters["area"] != signup.Replaces || signup.QuotaTier != domain.QuotaTierBurstOnly {
				t.Errorf("%s: added %+v, want a sandboxed executor with what the one it replaces had", name, signup)
			}
		}
		if len(replaced) != len(c.replaces) || (len(replaced) > 0 && replaced[0] != c.replaces[0]) {
			t.Errorf("%s: replaced %v, want %v", name, replaced, c.replaces)
		}
	}
}

// A pool raised while the run goes on gets new executors of its entry, in places nobody left.
func TestKeepAgentsFillsAPlaceThePoolGained(t *testing.T) {
	key := domain.PoolKey{AgentType: "generalist", Model: "anthropic/model-1"}
	store := &keepStore{state: db.PoolState{Pool: domain.AgentPool{{AgentType: "generalist", Model: "anthropic/model-1", Hyperparameters: map[string]any{"from": "entry"}, Count: 3}}, MaxAgents: 5, AtWork: map[domain.PoolKey]int{key: 1}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorInAgentSandbox: true, EndsAt: now.Add(time.Hour)}
	if err := svc.WithAgentLaunch(AgentLaunch{AgentSandbox: true, DefaultModel: "anthropic/model-1"}).KeepAgents(context.Background(), pe, now); err != nil {
		t.Fatal(err)
	}
	if len(store.joined) != 2 || store.joined[0].Replaces != "" || store.joined[0].Hyperparameters["from"] != "entry" {
		t.Errorf("added %+v, want two executors with the entry's hyperparameters, replacing nobody", store.joined)
	}
}

// An agent that ended early is not replaced, nor one beyond the limit of an entry an hour: the
// places kept empty are written as one line, once while it stays the same and cleared once none
// is, and every agent added asks the store to check its entry again under the lock.
func TestKeepAgentsHoldsThePlacesOfEarlyEndsAndAboveTheLimitAndSaysWhy(t *testing.T) {
	key := domain.PoolKey{AgentType: "generalist"}
	ended := func(id string, early bool) db.EndedAgent {
		r := db.EndedAgent{Key: key, Early: early}
		r.AgentID = id
		return r
	}
	store := &keepStore{state: db.PoolState{
		Pool: domain.AgentPool{{AgentType: "generalist", Count: 4}}, MaxAgents: 10, AtWork: map[domain.PoolKey]int{},
		Ended:  map[domain.PoolKey][]db.EndedAgent{key: {ended("ag-early", true), ended("ag-1", false), ended("ag-2", false), ended("ag-3", false)}},
		Recent: map[domain.PoolKey]int{key: domain.MaxReplacementsAnHour - 1},
	}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorInAgentSandbox: true, EndsAt: now.Add(10 * time.Hour)}
	keep := func() {
		t.Helper()
		if err := svc.WithAgentLaunch(AgentLaunch{AgentSandbox: true, DefaultModel: "anthropic/model-1"}).KeepAgents(context.Background(), pe, now); err != nil {
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
	store.state.Pool = domain.AgentPool{{AgentType: "generalist", Count: 0}}
	keep()
	if len(store.held) != 2 || store.held[1] != "" {
		t.Errorf("no place held: written %q, want the line cleared", store.held)
	}
}

// A researcher of the pool is kept at work as an executor is, signed up as a researcher, and
// takes none of the places max_agents leaves executors.
func TestKeepAgentsKeepsTheResearchersOfThePoolBesideAFullFieldOfExecutors(t *testing.T) {
	generalist := domain.PoolKey{AgentType: "generalist", Model: "anthropic/model-1"}
	pool := domain.AgentPool{{AgentType: domain.ResearcherAgentType, Count: 1}, {AgentType: "generalist", Model: "anthropic/model-1", Count: 1}}
	store := &keepStore{state: db.PoolState{Pool: pool, MaxAgents: 1, AtWork: map[domain.PoolKey]int{generalist: 1}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorInAgentSandbox: true, EndsAt: now.Add(10 * time.Hour), JobEstimatedDurationHours: 0.5,
		Actor: &domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
	if err := svc.WithAgentLaunch(AgentLaunch{AgentSandbox: true, DefaultModel: "anthropic/model-1"}).KeepAgents(context.Background(), pe, now); err != nil {
		t.Fatal(err)
	}
	if len(store.joined) != 1 || store.joined[0].Role != domain.SignupRoleResearcher || store.joined[0].AgentType != domain.ResearcherAgentType || store.joined[0].Model != "" {
		t.Errorf("joined = %+v, want one researcher of the built-in type on no model of its own", store.joined)
	}
}

// A budget that held no share for one more executor stopped the pass, and the researcher whose
// machine had stopped was never replaced. The executors' refusal ends the executors' places
// alone: the researchers' are filled.
func TestKeepAgentsFillsTheResearchersPlacesOnceNoExecutorFits(t *testing.T) {
	generalist := domain.PoolKey{AgentType: "generalist", Model: "anthropic/model-1"}
	researcher := domain.PoolKey{AgentType: domain.ResearcherAgentType}
	ended := func(id string, key domain.PoolKey) db.EndedAgent {
		r := db.EndedAgent{Key: key}
		r.AgentID = id
		return r
	}
	pool := domain.AgentPool{{AgentType: "generalist", Model: "anthropic/model-1", Count: 3}, {AgentType: domain.ResearcherAgentType, Count: 1}}
	for _, refused := range []error{db.ErrNoShareLeft, db.ErrMaxAgentsReached} {
		store := &keepStore{executorErr: refused, state: db.PoolState{Pool: pool, MaxAgents: 3, AtWork: map[domain.PoolKey]int{generalist: 1},
			Ended: map[domain.PoolKey][]db.EndedAgent{generalist: {ended("ag-1", generalist), ended("ag-2", generalist)}, researcher: {ended("ag-r", researcher)}}}}
		svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
		now := time.Now().UTC()
		pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, CoordinatorInAgentSandbox: true, EndsAt: now.Add(10 * time.Hour), JobEstimatedDurationHours: 0.5,
			Actor: &domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
		if err := svc.WithAgentLaunch(AgentLaunch{AgentSandbox: true, DefaultModel: "anthropic/model-1"}).KeepAgents(context.Background(), pe, now); err != nil {
			t.Fatal(err)
		}
		if len(store.joined) != 1 || store.joined[0].Role != domain.SignupRoleResearcher || store.joined[0].Replaces != "ag-r" {
			t.Errorf("%v: joined = %+v, want the researcher ag-r replaced", refused, store.joined)
		}
		if store.asked[domain.SignupRoleExecutor] != 1 {
			t.Errorf("%v: executor joins asked = %d, want 1: the first refusal ends the executors' places", refused, store.asked[domain.SignupRoleExecutor])
		}
	}
}
