// openresearch-node-agent runs as a DaemonSet on each cluster node.
// It reads per-pod CPU utilization from cgroup v2 every 2 seconds and POSTs
// the samples to the control plane's /internal/node-metrics endpoint.
//
// Environment variables:
//   NODE_NAME                  — Kubernetes node name (downward API)
//   OPENRESEARCH_CONTROL_PLANE_URL  — base URL of the control plane (default: http://metric-controller:8084)
//   PUSH_INTERVAL_MS           — push interval in ms (default: 2000)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	cgroupRoot      = "/sys/fs/cgroup/kubepods.slice"
	defaultCPURL    = "http://metric-controller:8084"
	defaultInterval = 2 * time.Second

	// maxPending bounds the retry buffer so a prolonged control-plane outage can't grow
	// memory without limit. At the default 2s interval this covers a 15-minute outage.
	maxPending = 450

	// jobsNamespace is where the control plane creates every experiment Job — must match
	// workload.OpenResearchNamespace (controlplane/shared/workload/workload_client.go). Not
	// imported directly: this binary is deliberately dependency-free from the control plane.
	jobsNamespace = "openresearch-jobs"

	// experimentIDLabel is set on every experiment pod's template by the control plane
	// (see workload_client.go) — the same label this agent reads back to tag its samples.
	experimentIDLabel = "openresearch.io/experiment-id"

)

type podSample struct {
	PodUID       string  `json:"pod_uid"`
	ExperimentID string  `json:"experiment_id,omitempty"`
	CPUUtilPct   float64 `json:"cpu_util_pct"`
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
		nodeName, _ = os.Hostname()
	}
	cpURL := os.Getenv("OPENRESEARCH_CONTROL_PLANE_URL")
	if cpURL == "" {
		cpURL = defaultCPURL
	}
	endpoint := cpURL + "/internal/node-metrics"

	interval := defaultInterval
	if ms := os.Getenv("PUSH_INTERVAL_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil {
			interval = time.Duration(n) * time.Millisecond
		}
	}

	log.Printf("openresearch-node-agent starting: node=%s endpoint=%s interval=%s", nodeName, endpoint, interval)

	kubeClient, err := inClusterPodLister()
	if err != nil {
		// Identity tagging is best-effort: CPU samples still flow untagged (experiment_id
		// empty) rather than the agent refusing to start over an RBAC/API-server hiccup.
		log.Printf("pod identity watch disabled: %v", err)
	}

	ctx := context.Background()

	// Previous readings for delta computation.
	prev := map[string]cpuStatEntry{}
	prevTime := time.Now()

	client := &http.Client{Timeout: 5 * time.Second}

	// pending holds payloads that failed to send, oldest first, so a network blip or
	// control-plane restart never silently drops a sample — every reading either reaches
	// the control plane or is still sitting here waiting to be retried.
	var pending []pushPayload

	for {
		time.Sleep(interval)

		// UTC explicitly: this timestamp crosses the wire into pushPayload.Timestamp and gets
		// compared against control-plane-side watermarks from other nodes/processes — pin the
		// zone here so that's never a question, even though Go's time.Time comparisons are
		// already zone-independent given a correct absolute instant.
		now := time.Now().UTC()
		elapsed := now.Sub(prevTime).Seconds()
		if elapsed <= 0 {
			elapsed = interval.Seconds()
		}

		current := readAllPodStats()
		samples := []podSample{}

		// Re-listed every tick rather than watched: a plain List is self-healing by
		// construction (a missed update just means it self-corrects on the very next
		// iteration two seconds later), so it needs no reconnect/relist bookkeeping of its
		// own — simpler and just as reliable as a long-lived watch for this cadence.
		var expByUID map[string]string
		if kubeClient != nil {
			expByUID, err = podExperimentIDs(ctx, kubeClient, nodeName)
			if err != nil {
				log.Printf("pod identity list error: %v", err)
			}
		}

		for path, cur := range current {
			if p, ok := prev[path]; ok && cur.podUID != "" {
				deltaUsec := float64(cur.usageUsec) - float64(p.usageUsec)
				if deltaUsec < 0 {
					deltaUsec = 0
				}
				// CPU utilization: delta microseconds / (elapsed seconds * 1e6) * 100
				util := (deltaUsec / (elapsed * 1e6)) * 100
				if util > 100 {
					util = 100
				}
				samples = append(samples, podSample{PodUID: cur.podUID, ExperimentID: expByUID[cur.podUID], CPUUtilPct: util})
			}
		}

		prev = current
		prevTime = now

		if len(samples) > 0 {
			pending = append(pending, pushPayload{Node: nodeName, Timestamp: now, Pods: samples})
		}

		pending = flushPending(client, endpoint, pending)

		if dropped := len(pending) - maxPending; dropped > 0 {
			log.Printf("retry buffer full: dropping %d oldest payload(s) after prolonged outage", dropped)
			pending = pending[dropped:]
		}
	}
}

// flushPending sends buffered payloads to the control plane in order, oldest first, stopping at
// the first failure so delivery order (and thus timestamp ordering) is preserved. Payloads that
// still fail to send remain in the returned slice for the next tick to retry — nothing is
// dropped except under the maxPending cap in the caller.
func flushPending(client *http.Client, endpoint string, pending []pushPayload) []pushPayload {
	i := 0
	for ; i < len(pending); i++ {
		if !push(client, endpoint, pending[i]) {
			break
		}
	}
	return pending[i:]
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

// readAllPodStats walks cgroupRoot looking for cpu.stat files and returns a map
// of cgroup path → entry. Pod UID is parsed from the path.
func readAllPodStats() map[string]cpuStatEntry {
	result := map[string]cpuStatEntry{}
	_ = filepath.WalkDir(cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cpu.stat" {
			return nil
		}
		usage, ok := readUsageUsec(path)
		if !ok {
			return nil
		}
		uid := extractPodUID(path)
		result[path] = cpuStatEntry{
			path:      path,
			podUID:    uid,
			usageUsec: usage,
			readAt:    time.Now(),
		}
		return nil
	})
	return result
}

// readUsageUsec extracts the usage_usec value from a cgroup v2 cpu.stat file.
func readUsageUsec(path string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "usage_usec ") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				v, err := strconv.ParseUint(parts[1], 10, 64)
				if err == nil {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// extractPodUID parses a pod UID from a cgroup path.
// Cgroup v2 paths look like: .../pod<uid>/...
func extractPodUID(path string) string {
	parts := strings.Split(path, string(os.PathSeparator))
	for _, part := range parts {
		if strings.HasPrefix(part, "pod") {
			uid := strings.TrimPrefix(part, "pod")
			// Convert underscores to hyphens (cgroup escaping).
			uid = strings.ReplaceAll(uid, "_", "-")
			if isValidUID(uid) {
				return uid
			}
		}
	}
	return ""
}

// inClusterPodLister builds a clientset from the pod's own in-cluster service account
// credentials (the standard kubelet-mounted token/CA, automatic when running inside the
// cluster with no kubeconfig given) — this daemonset only ever needs to read pods on its own
// node, so no other credential source is supported.
func inClusterPodLister() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return cs, nil
}

// podExperimentIDs lists every pod on this node in the experiment-jobs namespace and returns
// pod UID -> openresearch.io/experiment-id label value, for pods that have it set. Identity
// comes from this label, set once by the control plane at pod creation
// (workload_client.go) — never inferred from anything the pod itself reports, so it can't be
// spoofed or drift out of sync with what actually admitted the job.
func podExperimentIDs(ctx context.Context, cs kubernetes.Interface, nodeName string) (map[string]string, error) {
	list, err := cs.CoreV1().Pods(jobsNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for _, pod := range list.Items {
		if id := pod.Labels[experimentIDLabel]; id != "" {
			out[string(pod.UID)] = id
		}
	}
	return out, nil
}

// isValidUID does a basic check for Kubernetes UID format (8-4-4-4-12).
func isValidUID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	lens := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != lens[i] {
			return false
		}
	}
	_ = fmt.Sprintf // suppress unused import warning
	return true
}
