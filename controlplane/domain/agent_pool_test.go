package domain

import (
	"strings"
	"testing"
	"time"
)

func TestPoolDeficitReplacesWhoEndedWithinMaxAgents(t *testing.T) {
	g := PoolKey{AgentType: "generalist", Model: "m"}
	d := PoolKey{AgentType: "data-search", Model: "m"}
	pool := AgentPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 1}}
	ended := func(ids ...string) []EndedPlace {
		out := make([]EndedPlace, len(ids))
		for i, id := range ids {
			out[i] = EndedPlace{AgentID: id}
		}
		return out
	}
	for name, c := range map[string]struct {
		maxAgents int
		atWork    map[PoolKey]int
		ended     map[PoolKey][]EndedPlace
		want      []PoolPlace
	}{
		"nobody missing":     {3, map[PoolKey]int{g: 2, d: 1}, nil, nil},
		"one ended":          {3, map[PoolKey]int{g: 1, d: 1}, map[PoolKey][]EndedPlace{g: ended("ag-1", "ag-2")}, []PoolPlace{{Entry: pool[0], Replaces: "ag-1"}}},
		"a place gained":     {3, map[PoolKey]int{g: 2}, nil, []PoolPlace{{Entry: pool[1]}}},
		"max_agents is full": {3, map[PoolKey]int{g: 1, d: 1, {AgentType: "architecture-search", Model: "m"}: 1}, map[PoolKey][]EndedPlace{g: ended("ag-1")}, nil},
		"room for one of two": {3, map[PoolKey]int{d: 1, {AgentType: "architecture-search", Model: "m"}: 1}, map[PoolKey][]EndedPlace{g: ended("ag-1", "ag-2")},
			[]PoolPlace{{Entry: pool[0], Replaces: "ag-1"}}},
	} {
		got, holds := PoolDeficit(pool, c.maxAgents, c.atWork, c.ended, nil)
		if len(got) != len(c.want) || len(holds) != 0 {
			t.Errorf("%s: got = %+v %+v, want = %+v", name, got, holds, c.want)
			continue
		}
		for i := range got {
			if got[i].Entry.Key() != c.want[i].Entry.Key() || got[i].Replaces != c.want[i].Replaces {
				t.Errorf("%s: got = %+v, want = %+v", name, got, c.want)
			}
		}
	}
}

// An agent that ended early keeps its place empty, and an entry is given at most
// MaxReplacementsAnHour replacements an hour: each place kept empty says why.
func TestPoolDeficitHoldsThePlacesOfEarlyEndsAndAboveTheReplacementLimit(t *testing.T) {
	g := PoolKey{AgentType: "generalist"}
	pool := AgentPool{{AgentType: "generalist", Count: 4}}
	places, holds := PoolDeficit(pool, 10, map[PoolKey]int{}, map[PoolKey][]EndedPlace{g: {{AgentID: "ag-early", Early: true}, {AgentID: "ag-1"}, {AgentID: "ag-2"}}}, map[PoolKey]int{g: MaxReplacementsAnHour - 1})
	if len(places) != 2 || places[0].Replaces != "ag-1" || places[1].Replaces != "" {
		t.Errorf("places = %+v, want ag-1 replaced and the place nobody left filled", places)
	}
	if len(holds) != 2 || holds[0] != (PoolHold{Key: g, Replaces: "ag-early", Reason: HoldEndedEarly}) || holds[1] != (PoolHold{Key: g, Replaces: "ag-2", Reason: HoldReplacementLimit}) {
		t.Errorf("holds = %+v, want ag-early held for ending early and ag-2 for the limit", holds)
	}
	line := HoldsLine(holds)
	if !strings.Contains(line, "ag-early (generalist) ended before submitting a job") || !strings.Contains(line, "ag-2 (generalist) wait to be replaced") {
		t.Errorf("line = %q", line)
	}
	if HoldsLine(nil) != "" {
		t.Error("no hold: want no line")
	}
	if _, holds := PoolDeficit(AgentPool{{AgentType: "generalist", Count: 1}}, 10, map[PoolKey]int{g: 1}, map[PoolKey][]EndedPlace{g: {{AgentID: "ag-early", Early: true}}}, nil); len(holds) != 0 {
		t.Errorf("an entry that needs nobody: holds = %+v, want none", holds)
	}
}

func TestACutLowersItsEntryAndNoOther(t *testing.T) {
	pool := AgentPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 1}}
	got := pool.Lowered([]PoolKey{{AgentType: "data-search", Model: "m"}, {AgentType: "data-search", Model: "m"}, {AgentType: "architecture-search", Model: "m"}})
	if got[0].Count != 2 || got[1].Count != 0 || pool[1].Count != 1 {
		t.Errorf("got = %+v from %+v, want data-search at 0, generalist kept and the pool given unchanged", got, pool)
	}
}

func TestAPoolIsCheckedAndResolved(t *testing.T) {
	for name, c := range map[string]struct {
		pool AgentPool
		ok   bool
	}{
		"none, which is allowed":   {nil, true},
		"known agent types":        {AgentPool{{Count: 2}, {AgentType: "data-search", Count: 1}}, true},
		"an unknown agent type":    {AgentPool{{AgentType: "poet", Count: 1}}, false},
		"a type of another role":   {AgentPool{{AgentType: "baseline", Count: 1}}, false},
		"a model no agent can run": {AgentPool{{Model: "two words", Count: 1}}, false},
		"a bare model":             {AgentPool{{Model: "claude-sonnet-5", Count: 1}}, false},
		"a model reference":        {AgentPool{{Model: "openrouter/moonshotai/kimi-k2", Count: 1}}, true},
	} {
		if err := c.pool.Check(testCatalog()); (err == nil) != c.ok {
			t.Errorf("%s: got = %v, want ok %v", name, err, c.ok)
		}
	}
	resolved, err := AgentPool{{Count: 1}, {AgentType: "generalist", Count: 2}, {AgentType: "generalist", Model: "anthropic/own", Count: 1}}.Resolved(testCatalog())
	if err != nil || len(resolved) != 2 || resolved[0].Key() != (PoolKey{AgentType: "generalist"}) || resolved[0].Count != 3 || resolved[1].Model != "anthropic/own" {
		t.Errorf("resolved = %+v %v, want three generalists on the type's model and one on its own", resolved, err)
	}
}

func TestACappedPoolLowersItsLastEntriesFirst(t *testing.T) {
	pool := AgentPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 2}}
	got := pool.Capped(1)
	if got.Executors() != 1 || got[0].Count != 1 || got[1].Count != 0 || pool.Executors() != 4 {
		t.Errorf("got = %+v from %+v, want one generalist and the pool given unchanged", got, pool)
	}
	if got := pool.Capped(5); got.Executors() != 4 {
		t.Errorf("a cap above the total: got = %+v, want it unchanged", got)
	}
}

func TestReplacementLeadIsOneTrialAndAtLeastTheFloor(t *testing.T) {
	if got := ReplacementLead(0); got != ReplacementLeadFloor {
		t.Errorf("no estimate: got = %v, want the floor", got)
	}
	if got := ReplacementLead(2); got != 2*time.Hour {
		t.Errorf("a two hour trial: got = %v, want two hours", got)
	}
}

// An entry of the researcher type is of researchers, which keep their places whatever max_agents
// says: it bounds the executors alone.
func TestResearchersOfAPoolAreKeptBesideTheExecutorsAndCountTowardNoMaxAgents(t *testing.T) {
	r := PoolKey{AgentType: ResearcherAgentType}
	g := PoolKey{AgentType: "generalist"}
	pool := AgentPool{{AgentType: ResearcherAgentType, Count: 1}, {AgentType: "generalist", Count: 2}}
	if pool.Executors() != 2 || PoolRole(ResearcherAgentType) != AgentRoleResearcher || PoolRole("generalist") != AgentRoleExecutor || SignupRoleOfPool(ResearcherAgentType) != SignupRoleResearcher {
		t.Fatalf("executors %d, want 2 and the researcher type of role researcher", pool.Executors())
	}
	places, _ := PoolDeficit(pool, 2, map[PoolKey]int{g: 2}, map[PoolKey][]EndedPlace{r: {{AgentID: "ag-r"}}}, nil)
	if len(places) != 1 || places[0].Entry.Key() != r || places[0].Replaces != "ag-r" {
		t.Errorf("max_agents full of executors: places = %+v, want the researcher replaced", places)
	}
	places, _ = PoolDeficit(pool, 2, map[PoolKey]int{r: 1, g: 1}, nil, nil)
	if len(places) != 1 || places[0].Entry.Key() != g {
		t.Errorf("a researcher at work: places = %+v, want the one executor max_agents leaves room for", places)
	}
	if got := pool.Capped(1); got[0].Count != 1 || got[1].Count != 1 {
		t.Errorf("capped to 1 = %+v, want the researcher kept and one executor", got)
	}
	counts := pool.AtWorkOf(map[PoolKey]int{r: 1, g: 2})
	if counts.Total != 2 || counts.Pool[0].AtWork != 1 || counts.Pool[1].AtWork != 2 || counts.OutsidePool != 0 {
		t.Errorf("at work = %+v, want 2 executors counted and the researcher on its entry", counts)
	}
	if err := pool.Check(testCatalog()); err != nil {
		t.Errorf("check: %v", err)
	}
	if resolved, err := pool.Resolved(testCatalog()); err != nil || resolved[0].Key() != r {
		t.Errorf("resolved = %+v %v, want the researcher entry as it is", resolved, err)
	}
	if err := (AgentPool{{AgentType: "at-custom", Count: 1}, {AgentType: "planner", Count: 1}}).Check(testCatalog()); err == nil {
		t.Error("a planner entry: want refused")
	}
}
