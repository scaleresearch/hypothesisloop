package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

// AgentPoolEntry is how many agents of one agent type on one model a platform experiment is to
// have at work (experimentspec.AgentPoolEntry).
type AgentPoolEntry = experimentspec.AgentPoolEntry

type PoolKey = experimentspec.PoolKey

// AgentPool is the team a platform experiment is to have at work, executors and researchers: the
// desired state the platform keeps where it runs them (platformexperiments.Service.KeepAgents).
// It is optional, and its executors never exceed the platform experiment's max_agents, the cap on
// the executors at work.
type AgentPool []AgentPoolEntry

// PoolRole is the role of the agents of an entry of agentType: researcher for
// ResearcherAgentType, executor for any other.
func PoolRole(agentType string) AgentRole {
	if agentType == ResearcherAgentType {
		return AgentRoleResearcher
	}
	return AgentRoleExecutor
}

// SignupRoleOfPool is the role an agent of an entry of agentType is signed up with.
func SignupRoleOfPool(agentType string) SignupRole {
	if agentType == ResearcherAgentType {
		return SignupRoleResearcher
	}
	return SignupRoleCompetitor
}

// Executors is how many executors the pool wants at work.
func (p AgentPool) Executors() int {
	total := 0
	for _, e := range p {
		if !e.Key().Researches() {
			total += e.Count
		}
	}
	return total
}

// Check refuses a pool by what the platform's catalog says of it: an agent type catalog holds no
// type of the entry's role of (LookupAgentType), and a model that is no reference. Every other
// rule of a pool is the spec's (experimentspec.Spec.Validate).
func (p AgentPool) Check(catalog AgentTypeCatalog) error {
	for i, e := range p {
		agentType := e.AgentType
		if agentType == "" {
			agentType = DefaultAgentType
		}
		if _, err := LookupAgentType(catalog, agentType, PoolRole(agentType)); err != nil {
			return fmt.Errorf("agents[%d]: %w", i, err)
		}
		if e.Model != "" {
			if _, err := ParseModelRef(e.Model); err != nil {
				return fmt.Errorf("agents[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// Resolved is the pool with each entry's agent type filled as a joining agent's is
// (ResolveAgentLaunch), entries that come to name one agent type and model merged, and its order
// kept. A model and hyperparameters left out stay out: they are the type's.
func (p AgentPool) Resolved(catalog AgentTypeCatalog) (AgentPool, error) {
	out := make(AgentPool, 0, len(p))
	at := map[PoolKey]int{}
	for i, e := range p {
		choice, err := ResolveAgentLaunch(catalog, PoolRole(e.AgentType), e.AgentType, e.Model, e.Hyperparameters)
		if err != nil {
			return nil, fmt.Errorf("agents[%d]: %w", i, err)
		}
		key := PoolKey{AgentType: choice.AgentType, Model: choice.Model}
		if j, ok := at[key]; ok {
			out[j].Count += e.Count
			continue
		}
		at[key] = len(out)
		out = append(out, AgentPoolEntry{AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: e.Hyperparameters, Count: e.Count})
	}
	return out, nil
}

// PoolOf is the pool of agents launched with the agent types and models given, one each, in
// the order each key first appears.
func PoolOf(keys []PoolKey) AgentPool {
	var pool AgentPool
	at := map[PoolKey]int{}
	for _, key := range keys {
		if j, ok := at[key]; ok {
			pool[j].Count++
			continue
		}
		at[key] = len(pool)
		pool = append(pool, AgentPoolEntry{AgentType: key.AgentType, Model: key.Model, Count: 1})
	}
	return pool
}

// Lowered is the pool once agents of the keys given leave it for good, as a cut makes them: each
// lowers the entry of its agent type and model. One of no entry, an agent the coordinator added
// beyond the pool, lowers nothing. No entry goes below zero.
func (p AgentPool) Lowered(keys []PoolKey) AgentPool {
	out := slices.Clone(p)
	for _, key := range keys {
		if i := slices.IndexFunc(out, func(e AgentPoolEntry) bool { return e.Key() == key && e.Count > 0 }); i >= 0 {
			out[i].Count--
		}
	}
	return out
}

// Capped is the pool with its last entries of executors lowered until it wants at most maxAgents
// executors in all. No entry goes below zero, and researchers are left as they are.
func (p AgentPool) Capped(maxAgents int) AgentPool {
	out := slices.Clone(p)
	over := out.Executors() - maxAgents
	for i := len(out) - 1; i >= 0 && over > 0; i-- {
		if out[i].Key().Researches() {
			continue
		}
		cut := min(out[i].Count, over)
		out[i].Count -= cut
		over -= cut
	}
	return out
}

// ReplacementLeadFloor is the least time left in a run for the platform to add an agent in
// the place of one that ended: a machine boots, clones and reads the run in about that time.
const ReplacementLeadFloor = 15 * time.Minute

// ReplacementLead is how much of a run must be left for the platform to replace an agent: the
// estimate of one trial of its baseline job, and never less than ReplacementLeadFloor. One added
// later could not complete a trial before the end.
func ReplacementLead(jobEstimatedDurationHours float64) time.Duration {
	return max(ReplacementLeadFloor, time.Duration(jobEstimatedDurationHours*float64(time.Hour)))
}

// PoolPlace is one agent the platform is to add to keep a pool: the entry it fills, and the
// agent that ended whose place it takes, empty for a place the pool gained.
type PoolPlace struct {
	Entry    AgentPoolEntry
	Replaces string
}

// ReplacementMinRun is how long an agent must have run, an executor having submitted a job, for the
// platform to replace it: one that ended sooner is likely to end so again, so its place is held.
const ReplacementMinRun = 10 * time.Minute

// MaxReplacementsAnHour is how many agents the platform adds to one entry of a pool in the
// places of others in an hour.
const MaxReplacementsAnHour = 3

// EndedPlace is an agent whose place is free for good. Early is one that ended within
// ReplacementMinRun of joining or, an executor, before submitting any job, which the platform does
// not replace.
type EndedPlace struct {
	AgentID string
	Early   bool
}

// The reasons a place of a pool is held empty.
const (
	HoldEndedEarly       = "ended_early"
	HoldReplacementLimit = "replacement_limit"
)

// PoolHold is one place of a pool the platform keeps empty, and why: the agent that ended in
// it ended early (HoldEndedEarly), or its entry had MaxReplacementsAnHour replacements in the last
// hour (HoldReplacementLimit).
type PoolHold struct {
	Key      PoolKey
	Replaces string
	Reason   string
}

// PoolDeficit is what keeping pool needs: for each entry the agents it wants beyond those at work
// of its agent type and model, and for its executors never more in all than maxAgents leaves
// beside every executor at work, its own or not. A place an agent of the entry in ended left is
// held empty when that one ended early, then the others take the places of the ones that ended
// first, at most MaxReplacementsAnHour less recent of the entry's in the last hour, the rest held.
// A place no agent left is one the pool gained, and is filled. So an agent is replaced once, and
// one that ends early is never.
func PoolDeficit(pool AgentPool, maxAgents int, atWork map[PoolKey]int, ended map[PoolKey][]EndedPlace, recent map[PoolKey]int) ([]PoolPlace, []PoolHold) {
	working := 0
	for key, n := range atWork {
		if !key.Researches() {
			working += n
		}
	}
	room := maxAgents - working
	var places []PoolPlace
	var holds []PoolHold
	for _, e := range pool {
		need := e.Count - atWork[e.Key()]
		var predecessors []string
		for _, r := range ended[e.Key()] {
			if !r.Early {
				predecessors = append(predecessors, r.AgentID)
				continue
			}
			if need > 0 {
				holds = append(holds, PoolHold{Key: e.Key(), Replaces: r.AgentID, Reason: HoldEndedEarly})
				need--
			}
		}
		allowed := max(0, MaxReplacementsAnHour-recent[e.Key()])
		fits := need
		if !e.Key().Researches() {
			fits = min(need, room)
			room -= fits
		}
		for n := 0; n < fits; n++ {
			if n >= len(predecessors) {
				places = append(places, PoolPlace{Entry: e})
				continue
			}
			if allowed == 0 {
				holds = append(holds, PoolHold{Key: e.Key(), Replaces: predecessors[n], Reason: HoldReplacementLimit})
				continue
			}
			allowed--
			places = append(places, PoolPlace{Entry: e, Replaces: predecessors[n]})
		}
	}
	return places, holds
}

// HoldsLine is what the platform tells of the places it holds empty, one line, "" for none.
func HoldsLine(holds []PoolHold) string {
	if len(holds) == 0 {
		return ""
	}
	var early, limited []string
	for _, h := range holds {
		who := fmt.Sprintf("%s (%s)", h.Replaces, keyName(h.Key))
		if h.Reason == HoldEndedEarly {
			early = append(early, who)
		} else {
			limited = append(limited, who)
		}
	}
	var parts []string
	if len(early) > 0 {
		parts = append(parts, fmt.Sprintf("%s ended before submitting a job or within %d minutes of joining, so the platform does not replace them: add agents or raise the pool once the cause is fixed",
			strings.Join(early, ", "), int(ReplacementMinRun.Minutes())))
	}
	if len(limited) > 0 {
		parts = append(parts, fmt.Sprintf("%s wait to be replaced: their pool entry had %d replacements in the last hour",
			strings.Join(limited, ", "), MaxReplacementsAnHour))
	}
	return "agents not replaced: " + strings.Join(parts, "; ")
}

func keyName(k PoolKey) string {
	if k.Model == "" {
		return k.AgentType
	}
	return k.AgentType + " on " + k.Model
}
