package apiclient

import (
	"net/url"
)

func AgentsPath(platformExperiment string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/agents"
}

// SignupPath is where an agent's own signup is read, by the agent itself too: the roster above
// is the coordinator's.
func SignupPath(platformExperiment, agent string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/signups/" + url.PathEscape(agent)
}
