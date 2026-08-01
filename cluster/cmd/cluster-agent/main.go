// hypothesisloop-cluster-agent is the only component with credentials to a target cluster's
// Kubernetes API. It runs inside that cluster and reconciles its local Jobs against a
// desired-state view it receives from the control plane (POST .../reconcile) — the same
// model a kubelet uses: pull desired state, diff against actual, converge, report status
// back. The control plane never dials into this cluster; every call here is outbound.
//
// No leader election: every operation is naturally idempotent (Job creation tolerates
// AlreadyExists, deletion tolerates NotFound, status pushes are current snapshots) and
// reconciliation exchange is idempotent, so any number of replicas can run this
// loop concurrently and safely. Extra replicas just mean some redundant polling — never a
// correctness problem. Run more than one only for availability if you want it; one is fine.
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
	"os/signal"
	"sync"
	"syscall"
	"time"

	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

const (
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	clusterName := requiredEnv("CLUSTER_NAME")
	controlPlaneURL := requiredEnv("CONTROLPLANE_URL")
	// Passed to every Job this agent creates as HYPOTHESISLOOP_REGISTRY_URL — must be reachable
	// from *inside* the training pod, not from the agent itself, so it can't just default to
	// registry-service's in-cluster DNS name the way CONTROLPLANE_URL does for this process;
	// there is no such Service unless this cluster also runs the control plane's own compose
	// stack reachable under that name. Must be set explicitly for local/dev clusters where the
	// control plane lives outside the cluster (e.g. http://host.docker.internal:8083).
	registryURL := requiredEnv("REGISTRY_URL")
	pcfg := hypothesisloopcfg.MustLoad(requiredEnv("HYPOTHESISLOOP_CONFIG"))

	// JobWorkloadClient does all the actual Job/PriorityClass/Namespace work against this
	// cluster's own API server — it resolves in-cluster credentials automatically when given
	// no kubeconfig/context, which is exactly what a pod running inside the cluster has.
	jwc, err := workload.New(workload.Config{
		RegistryURL:                          registryURL,
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

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log("shutting down")
		cancel()
	}()

	log("starting: cluster=%s control_plane=%s", clusterName, controlPlaneURL)

	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := jwc.SetupCluster(setupCtx); err != nil {
		log("SetupCluster: %v", err)
	}
	setupCancel()

	a := &agent{
		clusterName: clusterName,
		cpURL:       controlPlaneURL,
		jwc:         jwc,
		httpClient:  &http.Client{Timeout: 35 * time.Second},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.reconcileLoop(ctx, log) }()
	go func() { defer wg.Done(); a.statusReportLoop(ctx, log) }()
	wg.Wait()
}

func requiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		fmt.Fprintf(os.Stderr, "cluster-agent: %s environment variable is required\n", key)
		os.Exit(1)
	}
	return value
}
