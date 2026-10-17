package experimentspec

// ResearcherPoolEntry is how many researchers of one agent type on one model a platform
// experiment is to have at work, and what a new one of them is started with.
type ResearcherPoolEntry struct {
	AgentType       string         `json:"agent_type,omitempty" doc:"The agent type these researchers are started as, by id: a researcher type of the org (GET /orgs/{id}/agent-types), the built-in generalist by default, or hyperparameter-search, architecture-search, data-search."`
	Model           string         `json:"model,omitempty" doc:"The model they run on. Omitted, their agent type's, read whenever each is served, or the platform's default when the type names none."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object each new researcher of the entry reads from its signup, at most 4096 bytes as JSON. Omitted, their agent type's, read whenever each is served. A researcher added in the place of one that ended reads the one it replaces."`
	Count           int            `json:"count" doc:"How many of them are to be at work. A stage boundary lowers it by each of them it cuts."`
}

// PoolKey is what tells the entries of a pool apart, and which entry a researcher is one of: its
// agent type and its model, empty for the type's.
type PoolKey struct {
	AgentType string
	Model     string
}

// Key is the entry's agent type and model.
func (e ResearcherPoolEntry) Key() PoolKey { return PoolKey{AgentType: e.AgentType, Model: e.Model} }
