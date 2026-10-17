package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ResearcherPoolEntry is how many researchers of one agent type on one model a platform
// experiment is to have at work, and what a new one of them is started with.
type ResearcherPoolEntry struct {
	AgentType       string         `json:"agent_type,omitempty" doc:"The agent type these researchers are started as, by id: a researcher type of the org (GET /orgs/{id}/agent-types), the built-in generalist by default, or hyperparameter-search, architecture-search, data-search."`
	Model           string         `json:"model,omitempty" doc:"The model they run on. Omitted, their agent type's, read whenever each is served, or the platform's default when the type names none."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object each new researcher of the entry reads from its signup, at most 4096 bytes as JSON. Omitted, their agent type's, read whenever each is served. A researcher added in the place of one that ended reads the one it replaces."`
	Count           int            `json:"count" doc:"How many of them are to be at work. A stage boundary lowers it by each of them it cuts."`
}

// PoolKey is what tells the entries of a pool apart, and which entry a researcher is one of: its
// agent type, which a signup holds as its flavor, and its model, empty for the type's.
type PoolKey struct {
	AgentType string
	Model     string
}

// Key is the entry's agent type and model.
func (e ResearcherPoolEntry) Key() PoolKey { return PoolKey{AgentType: e.AgentType, Model: e.Model} }

// ResearcherPool is the researchers a platform experiment is to have at work: the desired state the
// platform keeps where it runs them (quota.KeepResearchers). It is optional, and its total never
// exceeds the platform experiment's max_agents, the cap on the researchers at work.
type ResearcherPool []ResearcherPoolEntry

// Total is how many researchers the pool wants at work.
func (p ResearcherPool) Total() int {
	total := 0
	for _, e := range p {
		total += e.Count
	}
	return total
}

// Check refuses a pool a person or a planner states: an entry for no researcher, an agent type
// catalog holds no researcher type of (LookupAgentType), a model no agent can be started on,
// hyperparameters too large, two entries of one agent type and model, and more researchers in all
// than maxAgents.
func (p ResearcherPool) Check(maxAgents int, catalog AgentTypeCatalog) error {
	seen := map[PoolKey]bool{}
	for i, e := range p {
		if e.Count <= 0 {
			return fmt.Errorf("researchers[%d]: count must be positive, got %d", i, e.Count)
		}
		agentType := e.AgentType
		if agentType == "" {
			agentType = DefaultAgentFlavor
		}
		if _, err := LookupAgentType(catalog, agentType, AgentRoleResearcher); err != nil {
			return fmt.Errorf("researchers[%d]: %w", i, err)
		}
		if e.Model != "" {
			if err := ValidateModel(e.Model); err != nil {
				return fmt.Errorf("researchers[%d]: %w", i, err)
			}
		}
		if encoded, err := json.Marshal(e.Hyperparameters); err != nil || len(encoded) > MaxHyperparametersBytes {
			return fmt.Errorf("researchers[%d]: hyperparameters must be a JSON object of at most %d bytes", i, MaxHyperparametersBytes)
		}
		key := PoolKey{AgentType: agentType, Model: e.Model}
		if seen[key] {
			return fmt.Errorf("researchers[%d]: a second entry of agent_type %s on model %q: state one entry with their count", i, agentType, e.Model)
		}
		seen[key] = true
	}
	if total := p.Total(); total > maxAgents {
		return fmt.Errorf("researchers total %d, more than max_agents %d, the researchers that may be at work at once", total, maxAgents)
	}
	return nil
}

// Resolved is the pool with each entry's agent type filled as a joining agent's is
// (ResolveAgentLaunch), entries that come to name one agent type and model merged, and its order
// kept. A model and hyperparameters left out stay out: they are the type's.
func (p ResearcherPool) Resolved(catalog AgentTypeCatalog) (ResearcherPool, error) {
	out := make(ResearcherPool, 0, len(p))
	at := map[PoolKey]int{}
	for i, e := range p {
		choice, err := ResolveAgentLaunch(catalog, AgentRoleResearcher, e.AgentType, e.Model, e.Hyperparameters)
		if err != nil {
			return nil, fmt.Errorf("researchers[%d]: %w", i, err)
		}
		key := PoolKey{AgentType: choice.AgentType, Model: choice.Model}
		if j, ok := at[key]; ok {
			out[j].Count += e.Count
			continue
		}
		at[key] = len(out)
		out = append(out, ResearcherPoolEntry{AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: e.Hyperparameters, Count: e.Count})
	}
	return out, nil
}

// PoolOf is the pool of researchers launched with the agent types and models given, one each, in
// the order each key first appears.
func PoolOf(keys []PoolKey) ResearcherPool {
	var pool ResearcherPool
	at := map[PoolKey]int{}
	for _, key := range keys {
		if j, ok := at[key]; ok {
			pool[j].Count++
			continue
		}
		at[key] = len(pool)
		pool = append(pool, ResearcherPoolEntry{AgentType: key.AgentType, Model: key.Model, Count: 1})
	}
	return pool
}

// Lowered is the pool once researchers of the keys given leave it for good, as a cut makes them:
// each lowers the entry of its agent type and model. One of no entry, a researcher the
// coordinator added beyond the pool, lowers nothing. No entry goes below zero.
func (p ResearcherPool) Lowered(keys []PoolKey) ResearcherPool {
	out := slices.Clone(p)
	for _, key := range keys {
		if i := slices.IndexFunc(out, func(e ResearcherPoolEntry) bool { return e.Key() == key && e.Count > 0 }); i >= 0 {
			out[i].Count--
		}
	}
	return out
}

// Capped is the pool with its last entries lowered until it wants at most maxAgents researchers in
// all. No entry goes below zero.
func (p ResearcherPool) Capped(maxAgents int) ResearcherPool {
	out := slices.Clone(p)
	over := out.Total() - maxAgents
	for i := len(out) - 1; i >= 0 && over > 0; i-- {
		cut := min(out[i].Count, over)
		out[i].Count -= cut
		over -= cut
	}
	return out
}

// ReplacementLeadFloor is the least time left in a run for the platform to add a researcher in
// the place of one that ended: a machine boots, clones and reads the run in about that time.
const ReplacementLeadFloor = 15 * time.Minute

// ReplacementLead is how much of a run must be left for the platform to replace a researcher: the
// estimate of one trial of its baseline job, and never less than ReplacementLeadFloor. One added
// later could not complete a trial before the end.
func ReplacementLead(jobEstimatedDurationHours float64) time.Duration {
	return max(ReplacementLeadFloor, time.Duration(jobEstimatedDurationHours*float64(time.Hour)))
}

// PoolPlace is one researcher the platform is to add to keep a pool: the entry it fills, and the
// researcher that ended whose place it takes, empty for a place the pool gained.
type PoolPlace struct {
	Entry    ResearcherPoolEntry
	Replaces string
}

// ReplacementMinRun is how long a researcher must have run, having submitted a job, for the
// platform to replace it: one that ended sooner is likely to end so again, so its place is held.
const ReplacementMinRun = 10 * time.Minute

// MaxReplacementsAnHour is how many researchers the platform adds to one entry of a pool in the
// places of others in an hour.
const MaxReplacementsAnHour = 3

// EndedPlace is a researcher whose place is free for good. Early is one that ended before
// submitting any job or within ReplacementMinRun of joining, which the platform does not replace.
type EndedPlace struct {
	AgentID string
	Early   bool
}

// The reasons a place of a pool is held empty.
const (
	HoldEndedEarly       = "ended_early"
	HoldReplacementLimit = "replacement_limit"
)

// PoolHold is one place of a pool the platform keeps empty, and why: the researcher that ended in
// it ended early (HoldEndedEarly), or its entry had MaxReplacementsAnHour replacements in the last
// hour (HoldReplacementLimit).
type PoolHold struct {
	Key      PoolKey
	Replaces string
	Reason   string
}

// PoolDeficit is what keeping pool needs: for each entry the researchers it wants beyond those at
// work of its agent type and model, never more in all than maxAgents leaves beside every
// researcher at work, its own or not. A place a researcher of the entry in ended left is held
// empty when that one ended early, then the others take the places of the ones that ended first,
// at most MaxReplacementsAnHour less recent of the entry's in the last hour, the rest held. A
// place no researcher left is one the pool gained, and is filled. So a researcher is replaced
// once, and one that ends early is never.
func PoolDeficit(pool ResearcherPool, maxAgents int, atWork map[PoolKey]int, ended map[PoolKey][]EndedPlace, recent map[PoolKey]int) ([]PoolPlace, []PoolHold) {
	working := 0
	for _, n := range atWork {
		working += n
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
		for n := 0; n < min(need, room); n++ {
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
		room -= min(need, room)
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
		parts = append(parts, fmt.Sprintf("%s ended before submitting a job or within %d minutes of joining, so the platform does not replace them: add researchers or raise the pool once the cause is fixed",
			strings.Join(early, ", "), int(ReplacementMinRun.Minutes())))
	}
	if len(limited) > 0 {
		parts = append(parts, fmt.Sprintf("%s wait to be replaced: their pool entry had %d replacements in the last hour",
			strings.Join(limited, ", "), MaxReplacementsAnHour))
	}
	return "researchers not replaced: " + strings.Join(parts, "; ")
}

func keyName(k PoolKey) string {
	if k.Model == "" {
		return k.AgentType
	}
	return k.AgentType + " on " + k.Model
}
