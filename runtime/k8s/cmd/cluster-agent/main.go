// hypothesisloop-cluster-agent runs inside a target Kubernetes cluster and reconciles its local
// Jobs against desired state pulled from the control plane. The reconcile/status loop lives in
// runtime/shared/agentloop; this file builds the k8s-specific Executor.
//
// Environment variables:
//
// cluster-agent detach, run with the same environment and service account, removes this
// cluster from the control plane and stays until every job object it created here is gone. The
// chart runs it before the release is uninstalled.
//
//	CLUSTER_NAME        — this cluster's stable identity
//	API_URL             — base URL of the control-plane API
//	JOB_IMAGE           — the image a job that names none runs in; set, the cluster also takes
//	                      those jobs
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/k8s/internal/k8sexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const (
	binaryName        = "cluster-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	detach := false
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "detach" {
			fmt.Fprintln(os.Stderr, "usage: cluster-agent [detach]")
			os.Exit(2)
		}
		detach = true
	}
	clusterName := agentloop.RequiredEnv(binaryName, "CLUSTER_NAME")
	// One URL for both jobs of this process: polling the control plane, and the value passed to
	// every Job it creates as HYPOTHESISLOOP_API_URL. It must therefore be reachable from
	// *inside* a training pod as well as from the agent, so set it explicitly on local/dev
	// clusters where the control plane lives outside the cluster
	// (e.g. http://host.docker.internal:8081).
	apiURL := agentloop.RequiredEnv(binaryName, "API_URL")
	// The runtime token the cluster's owner was given when the cluster was registered.
	token := agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN")
	// Optional: how many trailing log lines to report per job per status push. Not required —
	// agentloop.DefaultLogTailLines (100) applies when unset.
	logTailLines := agentloop.OptionalIntEnv(binaryName, "LOG_TAIL_LINES")
	// Optional: whether this cluster sits behind a native autoscaler. Fail-closed default false —
	// see autoscaler.md's "Cluster capability".
	autoscalerEnabled := agentloop.OptionalBoolEnv(binaryName, "AUTOSCALER_ENABLED")

	jwc, err := k8sexec.New(k8sexec.Config{APIURL: apiURL, JobImage: os.Getenv("JOB_IMAGE")})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster-agent: workload client: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cluster-agent] "+format+"\n", args...)
	}

	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	if detach {
		agentloop.RunDetach(ctx, binaryName, log)
	} else {
		log("starting: cluster=%s control_plane=%s", clusterName, apiURL)
		setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := jwc.SetupCluster(setupCtx); err != nil {
			fmt.Fprintf(os.Stderr, "cluster-agent: SetupCluster: %v\n", err)
			os.Exit(1)
		}
		setupCancel()
	}

	a := &agentloop.Agent{
		ClusterName:       clusterName,
		APIURL:            apiURL,
		Token:             token,
		Executor:          jwc,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		LogTailLines:      logTailLines,
		AutoscalerEnabled: autoscalerEnabled,
		Log:               log,
	}
	// After a detach the control plane refuses this cluster's token, and the same loop that
	// keeps a cluster in step is what stops everything a removed one still holds.
	if err := a.Run(ctx); errors.Is(err, agentloop.ErrClusterRemoved) {
		log("cluster %s was removed from the control plane: its jobs are stopped, exiting", clusterName)
		return
	}
	if detach {
		fmt.Fprintln(os.Stderr, "cluster-agent: detach was interrupted before every job object was gone")
		os.Exit(1)
	}
}
