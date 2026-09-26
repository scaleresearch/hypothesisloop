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
//	SCRATCH_DIR           — filesystem Storage quotas are measured/enforced against (default /var/lib/hypothesisloop/scratch)
//	NODE_NAME             — override this node's reported identity (default: OS hostname)
//
// bare-agent detach, run with the same CLUSTER_NAME, API_URL and RUNTIME_TOKEN, removes this
// node's cluster from the control plane. The running agent then stops its jobs and exits, as it
// does when the cluster's owner removes it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/podexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const (
	binaryName        = "bare-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "detach" {
			fmt.Fprintln(os.Stderr, "usage: bare-agent [detach]")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		agentloop.RunDetach(ctx, binaryName, func(format string, args ...any) {
			fmt.Printf("[bare-agent] "+format+"\n", args...)
		})
		return
	}
	clusterName := agentloop.RequiredEnv(binaryName, "CLUSTER_NAME")
	// One URL for both polling the control plane and for the value handed to every container
	// this agent starts as HYPOTHESISLOOP_API_URL, so it must be reachable from inside a
	// workload container as well as from the agent process.
	apiURL := agentloop.RequiredEnv(binaryName, "API_URL")
	// The runtime token the cluster's owner was given when the cluster was registered.
	token := agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN")
	// Optional: how many trailing log lines to report per job per status push. Not required —
	// agentloop.DefaultLogTailLines (100) applies when unset.
	logTailLines := agentloop.OptionalIntEnv(binaryName, "LOG_TAIL_LINES")

	scratchDir := os.Getenv("SCRATCH_DIR")
	if scratchDir == "" {
		scratchDir = "/var/lib/hypothesisloop/scratch"
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: create scratch dir %s: %v\n", scratchDir, err)
		os.Exit(1)
	}

	exec, err := podexec.New(podexec.Config{
		APIURL:     apiURL,
		ScratchDir: scratchDir,
		NodeLabels: parseNodeLabels(os.Getenv("NODE_LABELS")),
		NodeName:   os.Getenv("NODE_NAME"),
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
		Token:             token,
		Executor:          exec,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		LogTailLines:      logTailLines,
		Log:               log,
	}
	err = a.Run(ctx)
	exec.Wait()
	if errors.Is(err, agentloop.ErrClusterRemoved) {
		log("cluster %s was removed from the control plane: its jobs are stopped, exiting", clusterName)
	}
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
