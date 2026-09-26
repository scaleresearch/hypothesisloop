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
//	JOB_IMAGE             — the image a job that names none runs in; set, the node also takes
//	                        those jobs
//	CPU_CLASS             — optional: the name of the CPU class this node offers its cores
//	                        as, one unit a core, beside its accelerators
//	SCRATCH_DIR           — a directory measured as this node's storage in place of the engine's store,
//	                        for an engine whose store is not on this host (default: the engine's store)
//	NODE_NAME             — override this node's reported identity (default: OS hostname)
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
	// Optional: how many trailing log lines to report per job per status push. Not required —
	// agentloop.DefaultLogTailLines (100) applies when unset.
	logTailLines := agentloop.OptionalIntEnv(binaryName, "LOG_TAIL_LINES")

	exec, err := newExecutor(clusterName, apiURL, os.Getenv("JOB_IMAGE"), os.Getenv("CPU_CLASS"))
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
	preflightCtx, preflightCancel := context.WithTimeout(ctx, 30*time.Second)
	facts, err := exec.Preflight(preflightCtx, &http.Client{Timeout: 10 * time.Second})
	preflightCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bare-agent: %v\n", err)
		os.Exit(1)
	}
	for _, fact := range facts {
		log("%s", fact)
	}

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
		// systemd sets INVOCATION_ID on what it starts. Run as the installed unit, the agent
		// keeps that unit from starting again at boot for a cluster that is gone.
		if os.Getenv("INVOCATION_ID") != "" {
			if err := hostinstall.StayStopped(hostinstall.Node{}); err != nil {
				log("%v", err)
			}
		}
	}
}

func newExecutor(clusterName, apiURL, jobImage, cpuClass string) (*podexec.Executor, error) {
	return podexec.New(podexec.Config{
		ClusterName: clusterName,
		JobImage:    jobImage,
		CPUClass:    cpuClass,
		APIURL:      apiURL,
		StorageDir:  os.Getenv("SCRATCH_DIR"),
		NodeLabels:  parseNodeLabels(os.Getenv("NODE_LABELS")),
		NodeName:    os.Getenv("NODE_NAME"),
	})
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
