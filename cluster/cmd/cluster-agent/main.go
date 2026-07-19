// openresearch-cluster-agent is the only component with credentials to a target cluster's
// Kubernetes API. It runs inside that cluster and reconciles its local Jobs against a
// desired-state view it fetches from the control plane (GET .../desired-state) — the same
// model a kubelet uses: pull desired state, diff against actual, converge, report status
// back. The control plane never dials into this cluster; every call here is outbound.
//
// No leader election: every operation is naturally idempotent (Job creation tolerates
// AlreadyExists, deletion tolerates NotFound, status pushes are sequence-numbered) and
// desired-state is a read-only, side-effect-free GET, so any number of replicas can run this
// loop concurrently and safely. Extra replicas just mean some redundant polling — never a
// correctness problem. Run more than one only for availability if you want it; one is fine.
//
// Environment variables:
//
//	CLUSTER_NAME        — this cluster's name as registered in clusters.yaml (default: default)
//	CONTROLPLANE_URL    — base URL of scheduler-service (default: http://scheduler-service:8082)
//	OPENRESEARCH_CONFIG — path to openresearch.yaml (default: settings/openresearch.yaml) — Accelerator
//	                      type catalog and AcceleratorResourceName, the k8s extended resource requested
//	                      per accelerator (execution-engine detail; agents never see this).
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

	openresearchcfg "github.com/scaleresearch/openresearch/controlplane/shared/config"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

const (
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	clusterName := envOrDefault("CLUSTER_NAME", "default")
	controlPlaneURL := envOrDefault("CONTROLPLANE_URL", "http://scheduler-service:8082")
	// Passed to every Job this agent creates as OPENRESEARCH_REGISTRY_URL — must be reachable
	// from *inside* the training pod, not from the agent itself, so it can't just default to
	// registry-service's in-cluster DNS name the way CONTROLPLANE_URL does for this process;
	// there is no such Service unless this cluster also runs the control plane's own compose
	// stack reachable under that name. Must be set explicitly for local/dev clusters where the
	// control plane lives outside the cluster (e.g. http://host.docker.internal:8083).
	registryURL := os.Getenv("REGISTRY_URL")
	pcfg := openresearchcfg.MustLoad(envOrDefault("OPENRESEARCH_CONFIG", "settings/openresearch.yaml"))

	// JobWorkloadClient does all the actual Job/PriorityClass/Namespace work against this
	// cluster's own API server — it resolves in-cluster credentials automatically when given
	// no kubeconfig/context, which is exactly what a pod running inside the cluster has.
	jwc, err := workload.New(workload.Config{
		RegistryURL:     registryURL,
		AcceleratorResourceName: pcfg.AcceleratorResourceName,
		AcceleratorTaintKey:     pcfg.AcceleratorTaintKey,
		OpenResearchConfig: &workload.OpenResearchConfig{
			NodeLabelByType:    pcfg.NodeLabelByType,
			NodeLabelKeyByType: pcfg.NodeLabelKeyByType,
			ResourceNameByType: pcfg.ResourceNameByType,
			TaintKeyByType:     pcfg.TaintKeyByType,
			// Required for GetLiveAcceleratorCapacity's flavor lookup (nameByFlavor()) — without
			// these, OpenResearchConfig being non-nil short-circuits the built-in defaults
			// fallback, and nameByFlavor() silently returns an empty map, so the desired-state
			// poll's accelerator capacity piggyback never has anything to report.
			NameByFlavor: pcfg.NameByFlavor,
			AcceleratorsByFlavor: pcfg.AcceleratorsByFlavor,
			FlavorOrder:  pcfg.FlavorOrder(),
		},
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
		tracked:     map[string]*trackedJob{},
	}
	a.resyncTrackedFromCluster(ctx, log)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.reconcileLoop(ctx, log) }()
	go func() { defer wg.Done(); a.statusReportLoop(ctx, log) }()
	wg.Wait()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
