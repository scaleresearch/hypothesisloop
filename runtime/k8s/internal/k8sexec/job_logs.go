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

// FetchLogTail returns the last maxLines lines of experimentID's pod output, read live from the
// Kubernetes API (never cached, never stored by this executor itself — see agentloop.LogTailer).
// Returns (nil, nil) rather than an error if the pod doesn't exist yet or has no container
// started (e.g. still Pending): that's an ordinary transient state for a job that was just
// admitted, not a failure worth logging every reconcile tick.
//
// When an attempt has already failed, its output is included ahead of the current pod's. A retry
// starts a brand-new pod, so reporting only the newest one replaced the failed attempt's
// traceback with the replacement's startup banner — with max_retries=N that happened N times and
// the job reached FAILED with every error discarded, leaving the reason it failed recorded
// nowhere at all once the pods were deleted. Both are returned so a retry cannot erase the one
// thing worth reading.
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
	newest, failed := selectLogPods(pods.Items)

	var lines []string
	// Failed attempt first: it is the older output, and it is the part that explains the failure.
	if failed != nil {
		failedLines, err := c.podTail(ctx, experimentID, failed.Name, maxLines, false)
		if err != nil {
			return nil, err
		}
		if len(failedLines) > 0 {
			lines = append(lines, fmt.Sprintf("--- failed attempt (pod %s, exit %d) ---",
				failed.Name, terminatedExitCode(failed)))
			lines = append(lines, failedLines...)
		}
	}
	// A container that crashes under RestartPolicy=OnFailure restarts *in place*: same pod, same
	// name, and GetLogs then serves the replacement instance's startup banner. The crashed
	// instance's output — the traceback — is only reachable with Previous, so without this a
	// restart silently replaces the reason the job failed with the reason it is still alive.
	if newest != nil && podHasRestarted(newest) {
		prevLines, err := c.podTail(ctx, experimentID, newest.Name, maxLines, true)
		if err != nil {
			return nil, err
		}
		if len(prevLines) > 0 {
			lines = append(lines, fmt.Sprintf("--- crashed instance before restart (pod %s, exit %d) ---",
				newest.Name, lastTerminationExitCode(newest)))
			lines = append(lines, prevLines...)
		}
	}
	if newest != nil && (failed == nil || newest.Name != failed.Name) {
		currentLines, err := c.podTail(ctx, experimentID, newest.Name, maxLines, false)
		if err != nil {
			return nil, err
		}
		if len(currentLines) > 0 {
			if len(lines) > 0 {
				lines = append(lines, fmt.Sprintf("--- current attempt (pod %s) ---", newest.Name))
			}
			lines = append(lines, currentLines...)
		}
	}
	return lines, nil
}

// selectLogPods picks the pod producing output now, and the most recent attempt that already
// terminated non-zero, if there is one. They are usually different pods: a retry replaces the
// failed pod rather than reusing it.
func selectLogPods(items []corev1.Pod) (newest, failed *corev1.Pod) {
	for i := range items {
		p := &items[i]
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
		if terminatedExitCode(p) > 0 {
			if failed == nil || p.CreationTimestamp.After(failed.CreationTimestamp.Time) {
				failed = p
			}
		}
	}
	return newest, failed
}

// terminatedExitCode returns the highest non-zero exit code among p's terminated containers, or
// 0 when none has terminated in failure.
func terminatedExitCode(p *corev1.Pod) int32 {
	var code int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode > code {
			code = cs.State.Terminated.ExitCode
		}
	}
	return code
}

// podTail reads the last maxLines lines of one pod's logs. previous reads the instance before
// the most recent restart — the equivalent of kubectl logs --previous.
func (c *JobWorkloadClient) podTail(ctx context.Context, experimentID, podName string, maxLines int, previous bool) ([]string, error) {
	tailLines := int64(maxLines)
	req := c.kube.CoreV1().Pods(HypothesisLoopNamespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
		Previous:  previous,
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
		return nil, fmt.Errorf("k8sexec.FetchLogTail: open log stream for %s (pod %s): %w", experimentID, podName, err)
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

// podHasRestarted reports whether any container in p has been restarted at least once, meaning
// an earlier instance's output exists only behind PodLogOptions.Previous.
func podHasRestarted(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 || cs.LastTerminationState.Terminated != nil {
			return true
		}
	}
	return false
}

// lastTerminationExitCode returns the exit code of the instance that ran before the most recent
// restart, or 0 when nothing has terminated.
func lastTerminationExitCode(p *corev1.Pod) int32 {
	var code int32
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode > code {
			code = t.ExitCode
		}
	}
	return code
}
