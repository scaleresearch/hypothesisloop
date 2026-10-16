// The agent types the agent harness ships (agents/experimentator/src/hypothesisloop_agent/prompts/flavors/),
// by key. The platform checks a key with its own lookup; this list only offers them.
export const AGENT_TYPES = ['generalist', 'hyperparameter-search', 'architecture-search', 'data-search']

export const DEFAULT_AGENT_TYPE = 'generalist'
