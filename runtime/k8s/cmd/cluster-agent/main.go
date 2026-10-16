// hypothesisloop-cluster-agent runs inside a target Kubernetes cluster and reconciles its local
// Jobs against desired state pulled from the control plane. The reconcile/status loop lives in
// runtime/shared/agentloop; this file builds the k8s-specific Executor.
//
// Environment variables:
//
// cluster-agent detach, run with the same environment and service account, deletes every job
// object this agent created here and says whether the cluster is still registered. The chart
// runs it before the release is uninstalled.
//
// cluster-agent check, run with CLUSTER_NAME, API_URL, METRICS_URL and RUNTIME_TOKEN, asks the
// API's and the metrics service's /healthz and whether the API takes the token, changes nothing
// and exits 1 when any fails. install.sh --dry-run runs it in a pod of the cluster.
//
//	CLUSTER_NAME          — this cluster's stable identity
//	API_URL               — base URL of the control-plane API
//	HYPOTHESISLOOP_AGENT_VERSION: the image tag this agent runs, sent on every call. The chart
//	                        sets it from images.tag.
//	HYPOTHESISLOOP_BUILDS — the BuildKit daemon this cluster builds environments on, a unix
//	                        socket in the pod. Unset, the cluster builds none.
//	HYPOTHESISLOOP_NODE_IMAGES — the containerd socket of this pod's node. Set, each environment
//	                        built is also loaded there, so a job on this node does not pull it.
//	HYPOTHESISLOOP_NODE_SNAPSHOTTER — the snapshotter the node's kubelet uses, when not
//	                        containerd's default.
//	HYPOTHESISLOOP_NODE_NAME — the name of this pod's node. Set beside HYPOTHESISLOOP_NODE_IMAGES,
//	                        each build loaded there is reported as it is loaded, so a job that
//	                        waits for it is placed on this node before the push ends.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/k8s/internal/k8sexec"
	"github.com/scaleresearch/hypothesisloop/runtime/k8s/internal/nodeimages"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/envbuild"
)

const (
	binaryName        = "cluster-agent"
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	detach := false
	if len(os.Args) > 1 {
		switch {
		case len(os.Args) == 2 && os.Args[1] == "detach":
			detach = true
		case len(os.Args) == 2 && os.Args[1] == "check":
			os.Exit(check())
		default:
			fmt.Fprintln(os.Stderr, "usage: cluster-agent [detach | check]")
			os.Exit(2)
		}
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
	version := os.Getenv("HYPOTHESISLOOP_AGENT_VERSION")
	// Optional: whether this cluster sits behind a native autoscaler. Fail-closed default false —
	// see autoscaler.md's "Cluster capability".
	autoscalerEnabled := agentloop.OptionalBoolEnv(binaryName, "AUTOSCALER_ENABLED")

	jwc, err := k8sexec.New(k8sexec.Config{APIURL: apiURL})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster-agent: workload client: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cluster-agent] "+format+"\n", args...)
	}

	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	a := &agentloop.Agent{
		ClusterName:       clusterName,
		APIURL:            apiURL,
		Token:             token,
		Version:           version,
		Executor:          jwc,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		AutoscalerEnabled: autoscalerEnabled,
		Log:               log,
	}
	if detach {
		if err := a.Detach(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "cluster-agent: detach was interrupted before every job object was gone")
			os.Exit(1)
		}
		log("every job object this agent created here is gone")
		a.SayIfStillRegistered(ctx, log)
		return
	}

	if address := os.Getenv("HYPOTHESISLOOP_BUILDS"); address != "" {
		daemon, err := envbuild.Dial(context.Background(), address)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cluster-agent: %v\n", err)
			os.Exit(1)
		}
		if socket := os.Getenv("HYPOTHESISLOOP_NODE_IMAGES"); socket != "" {
			dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
			node, err := nodeimages.Dial(dialCtx, socket, os.Getenv("HYPOTHESISLOOP_NODE_SNAPSHOTTER"))
			dialCancel()
			if err != nil {
				// Loading only spares a pull, so the cluster still builds without it.
				fmt.Fprintf(os.Stderr, "cluster-agent: builds are not loaded into this node: %v\n", err)
			} else {
				daemon.LoadInto(node, os.Getenv("HYPOTHESISLOOP_NODE_NAME"))
			}
		}
		a.Builder = daemon
	}

	log("starting: cluster=%s control_plane=%s version=%s", clusterName, apiURL, cmp.Or(version, "unknown"))
	log("checking the jobs namespace and priority classes")
	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := jwc.SetupCluster(setupCtx); err != nil {
		fmt.Fprintf(os.Stderr, "cluster-agent: SetupCluster: %v\n", err)
		os.Exit(1)
	}
	setupCancel()
	log("first contact with the control plane at %s", apiURL)
	// Once its owner removed the cluster the control plane refuses this cluster's token, and the
	// same loop that keeps a cluster in step is what stops everything a removed one still holds.
	if err := a.Run(ctx); errors.Is(err, agentloop.ErrClusterRemoved) {
		log("cluster %s was removed from the control plane: its jobs are stopped, exiting", clusterName)
	}
}

// check is the cluster-agent check subcommand: each line says ok or FAIL, and the exit status is
// 1 when any failed.
func check() int {
	agent := &agentloop.Agent{
		ClusterName: agentloop.RequiredEnv(binaryName, "CLUSTER_NAME"),
		APIURL:      agentloop.RequiredEnv(binaryName, "API_URL"),
		Token:       agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN"),
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
	}
	metricsURL := agentloop.RequiredEnv(binaryName, "METRICS_URL")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	failed := 0
	say := func(what string, err error) {
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", what, err)
			return
		}
		fmt.Printf("ok   %s\n", what)
	}
	say("api: "+agent.APIURL+" answers from this pod", agentloop.CheckReachable(ctx, agent.HTTPClient, agent.APIURL))
	say("metrics: "+metricsURL+" answers from this pod", agentloop.CheckReachable(ctx, agent.HTTPClient, metricsURL))
	tokenErr := agent.CheckToken(ctx)
	if errors.Is(tokenErr, agentloop.ErrClusterRemoved) {
		tokenErr = fmt.Errorf("no cluster named %s is registered there (hl cluster add %s)", agent.ClusterName, agent.ClusterName)
	}
	say("token: the API takes the runtime token of cluster "+agent.ClusterName, tokenErr)
	if failed > 0 {
		return 1
	}
	return 0
}
