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

// cpuStatEntry holds a single cpu.stat reading for a cgroup path.
type cpuStatEntry struct {
	path      string
	podUID    string
	usageUsec uint64
	readAt    time.Time
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

	for {
		time.Sleep(interval)

		// UTC explicitly: this timestamp crosses the wire into pushPayload.Timestamp and gets
		// compared against control-plane-side watermarks from other nodes/processes — pin the
		// zone here so that's never a question, even though Go's time.Time comparisons are
		// already zone-independent given a correct absolute instant.
		now := time.Now().UTC()
		current := readAllPodStats()
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

		for _, cur := range current {
			if cur.podUID != "" && expByUID[cur.podUID] != "" {
				samples = append(samples, podSample{PodUID: cur.podUID, ExperimentID: expByUID[cur.podUID]})
			}
		}

		if len(samples) > 0 {
			push(client, endpoint, pushPayload{Node: nodeName, Timestamp: now, Pods: samples})
		}
	}
}

func push(client *http.Client, endpoint string, payload pushPayload) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("push marshal error: %v", err)
		return false
	}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("push error: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("push rejected: status %d", resp.StatusCode)
		return false
	}
	return true
}
