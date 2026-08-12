package k8sexec

import (
	"bufio"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// FetchLogTail returns the last maxLines lines of experimentID's own pod's current stdout,
// read live from the Kubernetes API (never cached, never stored by this executor itself — see
// agentloop.LogTailer). Returns (nil, nil) rather than an error if the pod doesn't exist yet or
// has no container started (e.g. still Pending): that's an ordinary transient state for a job
// that was just admitted, not a failure worth logging every reconcile tick.
func (c *JobWorkloadClient) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	pods, err := c.kube.CoreV1().Pods(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return nil, fmt.Errorf("k8sexec.FetchLogTail: list pods for %s: %w", experimentID, err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	// A Job normally has exactly one live pod for its current attempt; if a retry left more than
	// one listable, the most recently created is the one actually producing output right now.
	pod := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = p
		}
	}

	tailLines := int64(maxLines)
	req := c.kube.CoreV1().Pods(HypothesisLoopNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		// A 400 here means the container hasn't started producing logs yet (e.g. still pulling
		// the image) -- an ordinary transient state, not a failure worth reporting every tick.
		// Anything else (403 in particular -- confirmed live: a missing pods/log RBAC rule
		// fails exactly this way, and silently swallowing it here made a real permissions bug
		// indistinguishable from "pod not ready yet" until the ClusterRole was fixed) must
		// propagate so agentloop's Log surfaces it.
		if apierrors.IsBadRequest(err) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("k8sexec.FetchLogTail: open log stream for %s (pod %s): %w", experimentID, pod.Name, err)
	}
	defer stream.Close()

	var lines []string
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("k8sexec.FetchLogTail: read log stream for %s: %w", experimentID, err)
	}
	return lines, nil
}
