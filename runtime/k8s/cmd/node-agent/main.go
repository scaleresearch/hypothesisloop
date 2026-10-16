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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
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

type podSample struct {
	PodUID       string `json:"pod_uid"`
	ExperimentID string `json:"experiment_id,omitempty"`
}

type pushPayload struct {
	Node      string      `json:"node"`
	Timestamp time.Time   `json:"timestamp"`
	Pods      []podSample `json:"pods"`
}

func main() {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME is required")
	}
	cpURL := os.Getenv("HYPOTHESISLOOP_CONTROL_PLANE_URL")
	if cpURL == "" {
		log.Fatal("HYPOTHESISLOOP_CONTROL_PLANE_URL is required")
	}
	endpoint := cpURL + "/internal/node-metrics"
	token := os.Getenv("RUNTIME_TOKEN")
	if token == "" {
		log.Fatal("RUNTIME_TOKEN is required")
	}

	ms := os.Getenv("PUSH_INTERVAL_MS")
	n, err := strconv.Atoi(ms)
	if err != nil || n <= 0 {
		log.Fatal("PUSH_INTERVAL_MS must be a positive integer")
	}
	interval := time.Duration(n) * time.Millisecond

	log.Printf("hypothesisloop-node-agent starting: node=%s endpoint=%s interval=%s", nodeName, endpoint, interval)

	kubeClient, err := inClusterPodLister()
	if err != nil {
		log.Fatalf("pod identity client: %v", err)
	}

	ctx := context.Background()

	client := &http.Client{Timeout: 5 * time.Second}
	var pending []pushPayload

	for {
		time.Sleep(interval)

		// UTC explicitly: this timestamp crosses the wire into pushPayload.Timestamp and gets
		// compared against control-plane-side watermarks from other nodes/processes — pin the
		// zone here so that's never a question, even though Go's time.Time comparisons are
		// already zone-independent given a correct absolute instant.
		now := time.Now().UTC()
		current := podUIDsWithCgroups()
		samples := []podSample{}

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
				samples = append(samples, podSample{PodUID: uid, ExperimentID: expByUID[uid]})
			}
		}

		if len(samples) > 0 {
			if len(pending) == pendingPushLimit {
				log.Printf("push buffer full; dropping the observation from %s", pending[0].Timestamp)
				pending = pending[1:]
			}
			pending = append(pending, pushPayload{Node: nodeName, Timestamp: now, Pods: samples})
		}
		pending = flush(client, endpoint, token, pending)
	}
}

// flush sends the backlog oldest first and returns what is left to send. The payloads carry
// their own collection time, so a backlog delivered late still lands on the instants it was
// observed at. It stops at the first payload the control plane could not take, which is retried
// next tick; one it refused is dropped, since sending it again gets the same answer and would
// hold everything behind it.
func flush(client *http.Client, endpoint, token string, pending []pushPayload) []pushPayload {
	for len(pending) > 0 {
		if push(client, endpoint, token, pending[0]) == pushRetry {
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

// refusalBodyLimit bounds how much of a refusal's body is logged.
const refusalBodyLimit = 512

func push(client *http.Client, endpoint, token string, payload pushPayload) pushOutcome {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("push marshal error, dropping the observation from %s: %v", payload.Timestamp, err)
		return pushRefused
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Printf("push request error: %v", err)
		return pushRetry
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("push error: %v", err)
		return pushRetry
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return pushDelivered
	}
	reason, _ := io.ReadAll(io.LimitReader(resp.Body, refusalBodyLimit))
	if resp.StatusCode/100 == 4 {
		log.Printf("push refused with status %d, dropping the observation from %s: %s", resp.StatusCode, payload.Timestamp, bytes.TrimSpace(reason))
		return pushRefused
	}
	log.Printf("push failed with status %d, will retry: %s", resp.StatusCode, bytes.TrimSpace(reason))
	return pushRetry
}
