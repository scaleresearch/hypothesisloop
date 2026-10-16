// hypothesisloop-node-agent runs as a DaemonSet on each cluster node.
// It relists active pod cgroups and POSTs fresh execution observations to the control plane.
//
// Environment variables:
//
//	NODE_NAME                  — Kubernetes node name (downward API)
//	HYPOTHESISLOOP_CONTROL_PLANE_URL  — required base URL of the control plane
//	PUSH_INTERVAL_MS           — required positive push interval in ms
//
// File layout: this file holds the stateless main loop and control-plane push logic;
// cgroup.go holds cgroup v2 cpu.stat reading; k8s.go holds pod-identity lookup.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

const (
	cgroupRoot = "/sys/fs/cgroup/kubepods.slice"
	// jobsNamespace is where the control plane creates every experiment Job — must match
	// k8sexec.HypothesisLoopNamespace (runtime/k8s/internal/k8sexec/workload_client.go). Not
	// imported directly: this binary is deliberately dependency-free from the control plane.
	jobsNamespace = "hypothesisloop-jobs"

	// experimentIDLabel is set on every experiment pod's template by the control plane
	// (see workload_client.go) — the same label this agent reads back to tag its samples.
	experimentIDLabel = "hypothesisloop.io/experiment-id"

	// pendingPushLimit bounds the retry buffer below. Every dropped payload is a hole in the
	// observed record a job is billed and evicted against, so a failed push is retried rather
	// than discarded — the control plane accepts backdated pushes for exactly this reason (see
	// services/metricsingest.maxPushBacklog). At the default 2s cadence this covers a one-hour outage.
	pendingPushLimit = 1800
)

func main() {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME is required")
	}
	cpURL := os.Getenv("HYPOTHESISLOOP_CONTROL_PLANE_URL")
	if cpURL == "" {
		log.Fatal("HYPOTHESISLOOP_CONTROL_PLANE_URL is required")
	}
	token := os.Getenv("RUNTIME_TOKEN")
	if token == "" {
		log.Fatal("RUNTIME_TOKEN is required")
	}
	controlPlane := &cpclient.Client{BaseURL: cpURL + "/internal/node-metrics", Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}

	ms := os.Getenv("PUSH_INTERVAL_MS")
	n, err := strconv.Atoi(ms)
	if err != nil || n <= 0 {
		log.Fatal("PUSH_INTERVAL_MS must be a positive integer")
	}
	interval := time.Duration(n) * time.Millisecond

	log.Printf("hypothesisloop-node-agent starting: node=%s endpoint=%s interval=%s", nodeName, controlPlane.BaseURL, interval)

	kubeClient, err := inClusterPodLister()
	if err != nil {
		log.Fatalf("pod identity client: %v", err)
	}

	ctx := context.Background()

	var pending []clusterapi.NodeMetricsPush

	for {
		time.Sleep(interval)

		// UTC explicitly: this timestamp crosses the wire as NodeMetricsPush.Timestamp and gets
		// compared against control-plane-side watermarks from other nodes/processes — pin the
		// zone here so that's never a question, even though Go's time.Time comparisons are
		// already zone-independent given a correct absolute instant.
		now := time.Now().UTC()
		current := podUIDsWithCgroups()
		samples := []clusterapi.PodSample{}

		// Re-listed every tick rather than watched: a plain List is self-healing by
		// construction (a missed update just means it self-corrects on the very next
		// iteration two seconds later), so it needs no reconnect/relist bookkeeping of its
		// own — simpler and just as reliable as a long-lived watch for this cadence.
		expByUID, err := podExperimentIDs(ctx, kubeClient, nodeName)
		if err != nil {
			log.Printf("pod identity list error: %v", err)
			continue
		}

		for uid := range current {
			if expByUID[uid] != "" {
				samples = append(samples, clusterapi.PodSample{PodUID: uid, ExperimentID: expByUID[uid]})
			}
		}

		if len(samples) > 0 {
			if len(pending) == pendingPushLimit {
				log.Printf("push buffer full; dropping the observation from %s", pending[0].Timestamp)
				pending = pending[1:]
			}
			pending = append(pending, clusterapi.NodeMetricsPush{Node: nodeName, Timestamp: now, Pods: samples})
		}
		pending = flush(controlPlane, pending)
	}
}

// flush sends the backlog oldest first and returns what is left to send. The payloads carry
// their own collection time, so a backlog delivered late still lands on the instants it was
// observed at. It stops at the first payload the control plane could not take, which is retried
// next tick; one it refused is dropped, since sending it again gets the same answer and would
// hold everything behind it.
func flush(controlPlane *cpclient.Client, pending []clusterapi.NodeMetricsPush) []clusterapi.NodeMetricsPush {
	for len(pending) > 0 {
		if push(controlPlane, pending[0]) == pushRetry {
			break
		}
		pending = pending[1:]
	}
	return pending
}

type pushOutcome int

const (
	pushDelivered pushOutcome = iota
	pushRefused
	pushRetry
)

func push(controlPlane *cpclient.Client, payload clusterapi.NodeMetricsPush) pushOutcome {
	err := controlPlane.Do(context.Background(), http.MethodPost, "", payload, nil)
	if err == nil {
		return pushDelivered
	}
	var refused *cpclient.Refusal
	if !errors.As(err, &refused) {
		log.Printf("push error: %v", err)
		return pushRetry
	}
	if refused.Status/100 == 4 {
		log.Printf("push refused with status %d, dropping the observation from %s: %s", refused.Status, payload.Timestamp, refused.Body)
		return pushRefused
	}
	log.Printf("push failed with status %d, will retry: %s", refused.Status, refused.Body)
	return pushRetry
}
