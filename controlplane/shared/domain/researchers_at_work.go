package domain

// PoolEntryAtWork is how many researchers at work are of one entry of a platform experiment's pool.
type PoolEntryAtWork struct {
	AgentType string `json:"agent_type,omitempty"`
	Model     string `json:"model,omitempty"`
	AtWork    int    `json:"at_work"`
}

// ResearchersAtWork counts the researchers at work of a platform experiment by the one rule
// max_agents holds to: each entry of its pool in order, and the others the coordinator added.
type ResearchersAtWork struct {
	Total       int               `json:"total" doc:"The researchers at work: the agents of role competitor that have not finished, were not cut, were not replaced and still have a machine."`
	Pool        []PoolEntryAtWork `json:"pool" doc:"One entry for each entry of researchers, in its order, with those of its agent type and model at work."`
	OutsidePool int               `json:"outside_pool" doc:"The researchers at work of an agent type and model no entry of researchers names, as the coordinator adds beyond the pool."`
}

// AtWorkOf counts the researchers at work by key against the pool.
func (p ResearcherPool) AtWorkOf(atWork map[PoolKey]int) ResearchersAtWork {
	out := ResearchersAtWork{Pool: make([]PoolEntryAtWork, len(p))}
	named := map[PoolKey]bool{}
	for i, e := range p {
		out.Pool[i] = PoolEntryAtWork{AgentType: e.AgentType, Model: e.Model, AtWork: atWork[e.Key()]}
		named[e.Key()] = true
	}
	for key, n := range atWork {
		out.Total += n
		if !named[key] {
			out.OutsidePool += n
		}
	}
	return out
}
