// hypothesisloop-cluster-agent runs inside a target Kubernetes cluster and reconciles its local
// Jobs against desired state pulled from the control plane. The reconcile/status loop lives in
// runtime/shared/agentloop; this file builds the k8s-specific Executor.
//
// Environment variables:
//
//	CLUSTER_NAME        — this cluster's stable identity
//	CONTROLPLANE_URL    — base URL of scheduler-service
//	HYPOTHESISLOOP_CONFIG — path to hypothesisloop.yaml
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
	controlPlaneURL := agentloop.RequiredEnv(binaryName, "CONTROLPLANE_URL")
	// Passed to every Job this agent creates as HYPOTHESISLOOP_REGISTRY_URL — must be reachable
	// from *inside* the training pod, not from the agent itself, so it can't just default to
	// registry-service's in-cluster DNS name the way CONTROLPLANE_URL does for this process;
	// there is no such Service unless this cluster also runs the control plane's own compose
	// stack reachable under that name. Must be set explicitly for local/dev clusters where the
	// control plane lives outside the cluster (e.g. http://host.docker.internal:8083).
	registryURL := agentloop.RequiredEnv(binaryName, "REGISTRY_URL")
	pcfg := hypothesisloopcfg.MustLoad(agentloop.RequiredEnv(binaryName, "HYPOTHESISLOOP_CONFIG"))

	jwc, err := k8sexec.New(k8sexec.Config{
		RegistryURL:                          registryURL,
		JobDeadlineMultiplier:                pcfg.Scheduler.JobDeadlineMultiplier,
		MinJobDeadlineSeconds:                pcfg.Scheduler.MinJobDeadlineSeconds,
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

	log("starting: cluster=%s control_plane=%s", clusterName, controlPlaneURL)

	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := jwc.SetupCluster(setupCtx); err != nil {
		log("SetupCluster: %v", err)
	}
	setupCancel()

	a := &agentloop.Agent{
		ClusterName:       clusterName,
		ControlPlaneURL:   controlPlaneURL,
		Executor:          jwc,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		Log:               log,
	}
	a.Run(ctx)
}
