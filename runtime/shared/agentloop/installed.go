package agentloop

import (
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// NewRemote is the control plane at apiURL as an agent installed on a cluster or node reaches it.
func NewRemote(clusterName, apiURL, token, version string) *Remote {
	return &Remote{ClusterName: clusterName, APIURL: apiURL, Token: token, Version: version, HTTP: &http.Client{Timeout: 35 * time.Second}}
}

// Installed is the agent of a cluster or node that runs its own runtime against remote.
func Installed(remote *Remote, exec agentexec.Executor, log func(format string, args ...any)) *Agent {
	return &Agent{
		ClusterName:       remote.ClusterName,
		ControlPlane:      remote,
		Builds:            remote,
		Executor:          exec,
		ReconcileInterval: 2 * time.Second,
		StatusInterval:    3 * time.Second,
		Log:               log,
	}
}
