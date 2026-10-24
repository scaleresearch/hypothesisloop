package domain

// PoolEntryAtWork is how many agents at work are of one entry of a platform experiment's pool.
type PoolEntryAtWork struct {
	AgentType string `json:"agent_type,omitempty"`
	Model     string `json:"model,omitempty"`
	AtWork    int    `json:"at_work"`
}

// AgentsAtWork counts the agents at work of a platform experiment against its pool: the executors
// by the one rule max_agents holds to, each entry of its pool in order, and the others the
// coordinator added.
type AgentsAtWork struct {
	Total       int               `json:"total" doc:"The executors at work, which max_agents holds to: the agents of role competitor that have not finished, were not cut, were not replaced and still have a machine. Researchers are not counted."`
	Pool        []PoolEntryAtWork `json:"pool" doc:"One entry for each entry of agents, in its order, with those of its agent type and model at work, researchers included."`
	OutsidePool int               `json:"outside_pool" doc:"The agents at work of an agent type and model no entry of agents names, as the coordinator adds beyond the pool."`
}

// AtWorkOf counts the agents at work by key against the pool.
func (p AgentPool) AtWorkOf(atWork map[PoolKey]int) AgentsAtWork {
	out := AgentsAtWork{Pool: make([]PoolEntryAtWork, len(p))}
	named := map[PoolKey]bool{}
	for i, e := range p {
		out.Pool[i] = PoolEntryAtWork{AgentType: e.AgentType, Model: e.Model, AtWork: atWork[e.Key()]}
		named[e.Key()] = true
	}
	for key, n := range atWork {
		if !key.Researches() {
			out.Total += n
		}
		if !named[key] {
			out.OutsidePool += n
		}
	}
	return out
}
