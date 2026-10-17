package objectstore

// The layout is <platform-experiment>/<agent>/<experiment>/ and every consumer derives its prefix
// from these three functions. The agent segment is not decoration: the per-agent byte ceiling and
// the per-agent figure on the usage endpoint are both a single prefix listing, and without it
// neither could be asked of the store at all.

// PlatformExperimentPrefix is the readable span handed to every job in a platform experiment —
// any agent can load the bytes behind any claim.
func PlatformExperimentPrefix(platformExperimentID string) string {
	return platformExperimentID + "/"
}

// AgentPrefix is everything one agent's jobs have written within a platform experiment: what the
// per-agent ceiling measures.
func AgentPrefix(platformExperimentID, agentID string) string {
	return platformExperimentID + "/" + agentID + "/"
}

// JobPrefix is the one prefix a job may write to.
func JobPrefix(platformExperimentID, agentID, experimentID string) string {
	return platformExperimentID + "/" + agentID + "/" + experimentID + "/"
}
