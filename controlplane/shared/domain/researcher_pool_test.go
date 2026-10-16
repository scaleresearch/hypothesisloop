package domain

import (
	"testing"
	"time"
)

func TestPoolDeficitReplacesWhoEndedWithinMaxAgents(t *testing.T) {
	g := PoolKey{AgentType: "generalist", Model: "m"}
	d := PoolKey{AgentType: "data-search", Model: "m"}
	pool := ResearcherPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 1}}
	for name, c := range map[string]struct {
		maxAgents int
		atWork    map[PoolKey]int
		ended     map[PoolKey][]string
		want      []PoolPlace
	}{
		"nobody missing":     {3, map[PoolKey]int{g: 2, d: 1}, nil, nil},
		"one ended":          {3, map[PoolKey]int{g: 1, d: 1}, map[PoolKey][]string{g: {"ag-1", "ag-2"}}, []PoolPlace{{Entry: pool[0], Replaces: "ag-1"}}},
		"a place gained":     {3, map[PoolKey]int{g: 2}, nil, []PoolPlace{{Entry: pool[1]}}},
		"max_agents is full": {3, map[PoolKey]int{g: 1, d: 1, {AgentType: "architecture-search", Model: "m"}: 1}, map[PoolKey][]string{g: {"ag-1"}}, nil},
		"room for one of two": {3, map[PoolKey]int{d: 1, {AgentType: "architecture-search", Model: "m"}: 1}, map[PoolKey][]string{g: {"ag-1", "ag-2"}},
			[]PoolPlace{{Entry: pool[0], Replaces: "ag-1"}}},
	} {
		got := PoolDeficit(pool, c.maxAgents, c.atWork, c.ended)
		if len(got) != len(c.want) {
			t.Errorf("%s: got = %+v, want = %+v", name, got, c.want)
			continue
		}
		for i := range got {
			if got[i].Entry.Key() != c.want[i].Entry.Key() || got[i].Replaces != c.want[i].Replaces {
				t.Errorf("%s: got = %+v, want = %+v", name, got, c.want)
			}
		}
	}
}

func TestACutLowersItsEntryAndNoOther(t *testing.T) {
	pool := ResearcherPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 1}}
	got := pool.Lowered([]PoolKey{{AgentType: "data-search", Model: "m"}, {AgentType: "data-search", Model: "m"}, {AgentType: "architecture-search", Model: "m"}})
	if got[0].Count != 2 || got[1].Count != 0 || pool[1].Count != 1 {
		t.Errorf("got = %+v from %+v, want data-search at 0, generalist kept and the pool given unchanged", got, pool)
	}
}

func TestAPoolIsCheckedAndResolved(t *testing.T) {
	for name, c := range map[string]struct {
		pool ResearcherPool
		ok   bool
	}{
		"none, which is allowed":   {nil, true},
		"within max_agents":        {ResearcherPool{{Count: 2}, {AgentType: "data-search", Count: 1}}, true},
		"above max_agents":         {ResearcherPool{{Count: 4}}, false},
		"an unknown agent type":    {ResearcherPool{{AgentType: "poet", Count: 1}}, false},
		"a type of another role":   {ResearcherPool{{AgentType: "baseline", Count: 1}}, false},
		"a count of none":          {ResearcherPool{{Count: 0}}, false},
		"one agent type twice":     {ResearcherPool{{Count: 1}, {AgentType: "generalist", Count: 1}}, false},
		"a model no agent can run": {ResearcherPool{{Model: "two words", Count: 1}}, false},
	} {
		if err := c.pool.Check(3, testCatalog()); (err == nil) != c.ok {
			t.Errorf("%s: got = %v, want ok %v", name, err, c.ok)
		}
	}
	resolved, err := ResearcherPool{{Count: 1}, {AgentType: "generalist", Model: "default", Count: 2}}.Resolved(testCatalog(), "default")
	if err != nil || len(resolved) != 1 || resolved[0].Key() != (PoolKey{AgentType: "generalist", Model: "default"}) || resolved[0].Count != 3 {
		t.Errorf("resolved = %+v %v, want one entry of three generalists on the default", resolved, err)
	}
}

func TestACappedPoolLowersItsLastEntriesFirst(t *testing.T) {
	pool := ResearcherPool{{AgentType: "generalist", Model: "m", Count: 2}, {AgentType: "data-search", Model: "m", Count: 2}}
	got := pool.Capped(1)
	if got.Total() != 1 || got[0].Count != 1 || got[1].Count != 0 || pool.Total() != 4 {
		t.Errorf("got = %+v from %+v, want one generalist and the pool given unchanged", got, pool)
	}
	if got := pool.Capped(5); got.Total() != 4 {
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
