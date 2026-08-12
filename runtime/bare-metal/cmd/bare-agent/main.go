// hypothesisloop-bare-agent runs directly on a single bare node — no Kubernetes, just a
// container engine already on the node (podman or docker). The reconcile/status loop lives in
// runtime/shared/agentloop; this file builds the bare-node-specific Executor.
//
// Each bare-agent registers as its own CLUSTER_NAME: the control plane's desired-state and
// capacity reporting are unpartitioned per cluster name, so two bare-agent processes sharing
// one name would both try to run the same experiments and clobber each other's capacity report.
//
// Environment variables:
//
//	CLUSTER_NAME          — this node's stable identity, registered as its own cluster
//	API_URL               — base URL of the control-plane API
//	                        (must also be reachable from *inside* a workload container)
//	HYPOTHESISLOOP_CONFIG — path to hypothesisloop.yaml
//	SCRATCH_DIR           — filesystem Storage quotas are measured/enforced against (default /var/lib/hypothesisloop/scratch)
//	NODE_NAME             — override this node's reported identity (default: OS hostname)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/podexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const (
	binaryName        = "bare-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	clusterName := agentloop.RequiredEnv(binaryName, "CLUSTER_NAME")
	// One URL for both polling the control plane and for the value handed to every container
	// this agent starts as HYPOTHESISLOOP_API_URL, so it must be reachable from inside a
	// workload container as well as from the agent process.
	apiURL := agentloop.RequiredEnv(binaryName, "API_URL")
	pcfg := hypothesisloopcfg.MustLoad(agentloop.RequiredEnv(binaryName, "HYPOTHESISLOOP_CONFIG"))
	// Optional: how many trailing log lines to report per job per status push. Not required —
	// agentloop.DefaultLogTailLines (100) applies when unset.
	logTailLines, _ := strconv.Atoi(os.Getenv("LOG_TAIL_LINES"))

	scratchDir := os.Getenv("SCRATCH_DIR")
	if scratchDir == "" {
		scratchDir = "/var/lib/hypothesisloop/scratch"
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: create scratch dir %s: %v\n", scratchDir, err)
		os.Exit(1)
	}

	exec, err := podexec.New(podexec.Config{
		APIURL:                               apiURL,
		DefaultTerminationGracePeriodSeconds: pcfg.Scheduler.DefaultTerminationGracePeriodSeconds,
		MaxTerminationGracePeriodSeconds:     pcfg.Scheduler.MaxTerminationGracePeriodSeconds,
		PricedAcceleratorTypes:               pcfg.AcceleratorTypeNames(),
		ScratchDir:                           scratchDir,
		NodeLabels:                           parseNodeLabels(os.Getenv("NODE_LABELS")),
		NodeName:                             os.Getenv("NODE_NAME"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[bare-agent] "+format+"\n", args...)
	}

	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	log("starting: cluster=%s node=%s engine=%s control_plane=%s storage_quota_enforced=%v", clusterName, exec.NodeName(), exec.Engine(), apiURL, exec.StorageQuotaEnforced())

	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := exec.SetupCluster(setupCtx); err != nil {
		log("SetupCluster: %v", err)
	}
	setupCancel()

	a := &agentloop.Agent{
		ClusterName:       clusterName,
		APIURL:            apiURL,
		Executor:          exec,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		LogTailLines:      logTailLines,
		MaxLogLineChars:   pcfg.Scheduler.MaxLogTailLineChars,
		Log:               log,
	}
	a.Run(ctx)
}

// parseNodeLabels parses "k1=v1,k2=v2" into a map — the bare-node equivalent of one fleet.yaml
// node entry's static labels.
func parseNodeLabels(raw string) map[string]string {
	labels := map[string]string{}
	if raw == "" {
		return labels
	}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return labels
}
