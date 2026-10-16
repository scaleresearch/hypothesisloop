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
//	RUNTIME_TOKEN         — the cluster's runtime token
//	SCRATCH_DIR           — a directory measured as this node's storage in place of the engine's store,
//	                        for an engine whose store is not on this host (default: the engine's store)
//	NODE_NAME             — override this node's reported identity (default: OS hostname)
//	NODE_LABELS           — this node's own labels, as k1=v1,k2=v2 (default: none)
//
// bare-agent install, detach and uninstall (node.go) put the agent on a node as a systemd unit
// and take it off again.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/hostinstall"
	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/podexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

// version is the image tag this binary was built from, set at link time (-X main.version=TAG).
// Built without it, the agent names no version.
var version string

const (
	binaryName        = "bare-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 {
		command, known := commands[os.Args[1]]
		if !known {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if err := command(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "bare-agent %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	clusterName := agentloop.RequiredEnv(binaryName, "CLUSTER_NAME")
	// One URL for both polling the control plane and for the value handed to every container
	// this agent starts as HYPOTHESISLOOP_API_URL, so it must be reachable from inside a
	// workload container as well as from the agent process.
	apiURL := agentloop.RequiredEnv(binaryName, "API_URL")
	// The runtime token the cluster's owner was given when the cluster was registered.
	token := agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN")

	ctx, cancel := agentloop.SignalContext(say)
	defer cancel()

	exec, err := newExecutor(ctx, clusterName, apiURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: %v\n", err)
		os.Exit(1)
	}

	say("starting: cluster=%s node=%s engine=%s control_plane=%s storage_quota_enforced=%v", clusterName, exec.NodeName(), exec.Engine(), apiURL, exec.StorageQuotaEnforced())
	preflightCtx, preflightCancel := context.WithTimeout(ctx, 30*time.Second)
	facts, err := exec.Preflight(preflightCtx, &http.Client{Timeout: 10 * time.Second})
	preflightCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: %v\n", err)
		os.Exit(1)
	}
	for _, fact := range facts {
		say("%s", fact)
	}

	remote := &agentloop.Remote{ClusterName: clusterName, APIURL: apiURL, Token: token, Version: version, HTTP: &http.Client{Timeout: 35 * time.Second}}
	a := &agentloop.Agent{
		ClusterName:       clusterName,
		ControlPlane:      remote,
		Builds:            remote,
		Executor:          exec,
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		Log:               say,
	}
	err = a.Run(ctx)
	exec.Wait()
	if errors.Is(err, agentloop.ErrClusterRemoved) {
		say("cluster %s was removed from the control plane: its jobs are stopped, exiting", clusterName)
		// systemd sets INVOCATION_ID on what it starts. Run as the installed unit, the agent
		// keeps that unit from starting again at boot for a cluster that is gone.
		if os.Getenv("INVOCATION_ID") != "" {
			if err := hostinstall.StayStopped(hostinstall.Node{}); err != nil {
				say("%v", err)
			}
		}
	}
}

func newExecutor(ctx context.Context, clusterName, apiURL string) (*podexec.Executor, error) {
	nodeLabels, err := parseNodeLabels(os.Getenv("NODE_LABELS"))
	if err != nil {
		return nil, err
	}
	return podexec.New(ctx, podexec.Config{
		ClusterName: clusterName,
		APIURL:      apiURL,
		StorageDir:  os.Getenv("SCRATCH_DIR"),
		NodeLabels:  nodeLabels,
		NodeName:    os.Getenv("NODE_NAME"),
	})
}

// parseNodeLabels parses "k1=v1,k2=v2" into a map — the bare-node equivalent of one fleet.yaml
// node entry's static labels. A pair that is not key=value is refused: left out, the node would
// run without a label its jobs are selected by, with nothing said.
func parseNodeLabels(raw string) (map[string]string, error) {
	labels := map[string]string{}
	if raw == "" {
		return labels, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, fmt.Errorf("NODE_LABELS=%q: %q is not key=value", raw, pair)
		}
		labels[k] = strings.TrimSpace(v)
	}
	return labels, nil
}
