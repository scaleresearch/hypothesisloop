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

	"k8s.io/client-go/kubernetes"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

const cgroupRoot = "/sys/fs/cgroup/kubepods.slice"

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
	controlPlane := &cpclient.Client{BaseURL: cpURL + "/internal/node-metrics", Token: token, HTTP: &http.Client{}}

	n, err := strconv.Atoi(os.Getenv("PUSH_INTERVAL_MS"))
	if err != nil || n <= 0 {
		log.Fatal("PUSH_INTERVAL_MS must be a positive integer")
	}
	interval := time.Duration(n) * time.Millisecond

	log.Printf("hypothesisloop-node-agent starting: node=%s endpoint=%s interval=%s", nodeName, controlPlane.BaseURL, interval)

	kubeClient, err := inClusterPodLister()
	if err != nil {
		log.Fatalf("pod identity client: %v", err)
	}

	ctx, cancel := agentloop.SignalContext(log.Printf)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		tickCtx, tickCancel := context.WithTimeout(ctx, interval)
		tick(tickCtx, kubeClient, controlPlane, nodeName)
		tickCancel()
	}
}

// tick pushes what runs on this node now. A push that does not land is not sent again: the next
// tick's is current.
func tick(ctx context.Context, kube kubernetes.Interface, controlPlane *cpclient.Client, nodeName string) {
	// UTC: the timestamp is compared with watermarks other nodes and processes wrote.
	now := time.Now().UTC()
	current := podUIDsWithCgroups()
	expByUID, err := podExperimentIDs(ctx, kube, nodeName)
	if err != nil {
		log.Printf("pod identity list error: %v", err)
		return
	}
	samples := []clusterapi.PodSample{}
	for uid := range current {
		if expByUID[uid] != "" {
			samples = append(samples, clusterapi.PodSample{PodUID: uid, ExperimentID: expByUID[uid]})
		}
	}
	if len(samples) == 0 {
		return
	}
	push(ctx, controlPlane, clusterapi.NodeMetricsPush{Node: nodeName, Timestamp: now, Pods: samples})
}

func push(ctx context.Context, controlPlane *cpclient.Client, payload clusterapi.NodeMetricsPush) {
	err := controlPlane.Do(ctx, http.MethodPost, "", payload, nil)
	var refused *cpclient.Refusal
	switch {
	case err == nil:
	case errors.As(err, &refused):
		log.Printf("push of the observation from %s refused with status %d: %s", payload.Timestamp, refused.Status, refused.Body)
	default:
		log.Printf("push of the observation from %s: %v", payload.Timestamp, err)
	}
}
