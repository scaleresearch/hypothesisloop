package experimentspec

// ResearcherAgentType is the built-in agent type of a researcher: an entry of a pool that names it
// is of researchers, and every other entry is of executors.
const ResearcherAgentType = "researcher"

// AgentPoolEntry is how many agents of one agent type on one model a platform experiment is to
// have at work, and what a new one of them is started with. Its agent type gives their role.
type AgentPoolEntry struct {
	AgentType       string         `json:"agent_type,omitempty" doc:"The agent type these agents are started as, by id, which gives their role. The built-in researcher makes researchers, which run no job and steer the work through the hypothesis graph. Any other makes executors, which run the jobs: an executor type of the org (GET /orgs/{id}/agent-types), the built-in generalist by default, or hyperparameter-search, architecture-search, data-search."`
	Model           string         `json:"model,omitempty" doc:"The model they run on, <provider>/<model id> as GET /models lists it. Omitted, their agent type's, read whenever each is served, else, for researchers, the coordinator's model (coordinator_model), else the platform's default."`
	Hyperparameters map[string]any `json:"hyperparameters,omitempty" doc:"A small JSON object each new agent of the entry reads from its signup, at most 4096 bytes as JSON. Omitted, their agent type's, read whenever each is served. An agent added in the place of one that ended reads the one it replaces."`
	Count           int            `json:"count" doc:"How many of them are to be at work. A stage boundary lowers it by each of them it cuts."`
}

// PoolKey is what tells the entries of a pool apart, and which entry an agent is one of: its
// agent type and its model, empty for the type's.
type PoolKey struct {
	AgentType string
	Model     string
}

// Researches says agents of the key are researchers.
func (k PoolKey) Researches() bool { return k.AgentType == ResearcherAgentType }

// Key is the entry's agent type and model.
func (e AgentPoolEntry) Key() PoolKey { return PoolKey{AgentType: e.AgentType, Model: e.Model} }
