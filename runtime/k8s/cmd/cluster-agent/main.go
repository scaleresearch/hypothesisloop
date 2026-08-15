// hypothesisloop-cluster-agent runs inside a target Kubernetes cluster and reconciles its local
// Jobs against desired state pulled from the control plane. The reconcile/status loop lives in
// runtime/shared/agentloop; this file builds the k8s-specific Executor.
//
// Environment variables:
//
//	CLUSTER_NAME        — this cluster's stable identity
//	API_URL             — base URL of the control-plane API
//	HYPOTHESISLOOP_CONFIG — path to hypothesisloop.yaml
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/runtime/k8s/internal/k8sexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const (
	binaryName        = "cluster-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	clusterName := agentloop.RequiredEnv(binaryName, "CLUSTER_NAME")
	// One URL for both jobs of this process: polling the control plane, and the value passed to
	// every Job it creates as HYPOTHESISLOOP_API_URL. It must therefore be reachable from
	// *inside* a training pod as well as from the agent, so set it explicitly on local/dev
	// clusters where the control plane lives outside the cluster
	// (e.g. http://host.docker.internal:8081).
	apiURL := agentloop.RequiredEnv(binaryName, "API_URL")
	pcfg := hypothesisloopcfg.MustLoad(agentloop.RequiredEnv(binaryName, "HYPOTHESISLOOP_CONFIG"))
	// Optional: how many trailing log lines to report per job per status push. Not required —
	// agentloop.DefaultLogTailLines (100) applies when unset.
	logTailLines, _ := strconv.Atoi(os.Getenv("LOG_TAIL_LINES"))

	jwc, err := k8sexec.New(k8sexec.Config{
		APIURL:                               apiURL,
		DefaultTerminationGracePeriodSeconds: pcfg.Scheduler.DefaultTerminationGracePeriodSeconds,
		MaxTerminationGracePeriodSeconds:     pcfg.Scheduler.MaxTerminationGracePeriodSeconds,
		PricedAcceleratorTypes:               pcfg.AcceleratorTypeNames(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster-agent: workload client: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cluster-agent] "+format+"\n", args...)
	}

	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	log("starting: cluster=%s control_plane=%s", clusterName, apiURL)

	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := jwc.SetupCluster(setupCtx); err != nil {
		log("SetupCluster: %v", err)
	}
	setupCancel()

	a := &agentloop.Agent{
		ClusterName:       clusterName,
		APIURL:            apiURL,
		Executor:          jwc,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		LogTailLines:      logTailLines,
		MaxLogLineChars:   pcfg.Scheduler.MaxLogTailLineChars,
		Log:               log,
	}
	a.Run(ctx)
}
