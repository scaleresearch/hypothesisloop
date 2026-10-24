package k8sexec

import (
	"bufio"
	"context"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/ranktail"
)

// FetchLogTail returns the last maxLines lines of experimentID's pod output, read live from the
// Kubernetes API (never cached, never stored by this executor itself).
// Returns (nil, nil) rather than an error if the pod doesn't exist yet or has no container
// started (e.g. still Pending): that's an ordinary transient state for a job that was just
// admitted, not a failure worth logging every reconcile tick.
//
// A distributed job's tail is one section per rank (see gangLogTail).
func (c *JobWorkloadClient) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	pods, err := c.experimentPods(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("k8sexec.FetchLogTail: list pods for %s: %w", experimentID, err)
	}
	if len(pods) == 0 {
		return nil, nil
	}
	if pods[0].Spec.Subdomain != "" {
		return c.gangLogTail(ctx, experimentID, pods, maxLines)
	}
	newest := &pods[0]
	for i := range pods {
		if pods[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods[i]
		}
	}
	return c.podTail(ctx, experimentID, newest.Name, maxLines)
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

// podTail reads the last maxLines lines of one pod's logs.
func (c *JobWorkloadClient) podTail(ctx context.Context, experimentID, podName string, maxLines int) ([]string, error) {
	tailLines := int64(maxLines)
	req := c.kube.CoreV1().Pods(HypothesisLoopNamespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})
	ctx, cancel := context.WithTimeout(ctx, k8sCallTimeout)
	defer cancel()
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("k8sexec.FetchLogTail: read log stream for %s: %w", experimentID, err)
	}
	return lines, nil
}

// gangLogTail is a distributed job's tail: one section per rank, sharing maxLines.
func (c *JobWorkloadClient) gangLogTail(ctx context.Context, experimentID string, pods []corev1.Pod, maxLines int) ([]string, error) {
	sections, err := c.rankSections(ctx, experimentID, pods)
	if err != nil {
		return nil, fmt.Errorf("k8sexec.FetchLogTail: %s: %w", experimentID, err)
	}
	return ranktail.Tail(sections, maxLines)
}

// rankSections is each pod of a gang as a section of its tail, read through the pod's own logs.
func (c *JobWorkloadClient) rankSections(ctx context.Context, experimentID string, pods []corev1.Pod) ([]ranktail.Section, error) {
	sections := make([]ranktail.Section, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		rank, group, err := podGangRank(p)
		if err != nil {
			return nil, err
		}
		s := ranktail.Section{Rank: rank, Group: group, Where: "pod " + p.Name, Exit: int(terminatedExitCode(p)),
			Read: func(n int) ([]string, error) { return c.podTail(ctx, experimentID, p.Name, n) }}
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; s.Exit > 0 && t != nil && int(t.ExitCode) == s.Exit {
				s.FailedAt = t.FinishedAt.Time
			}
		}
		sections = append(sections, s)
	}
	return sections, nil
}

// podGangRank is a distributed job's pod's global rank, its completion index plus the offset of
// its group, and its group's name, empty for an ungrouped job. Both are read off the pod as
// BuildJobs compiled them, which is what the workload itself was told.
func podGangRank(p *corev1.Pod) (int, string, error) {
	index, err := strconv.Atoi(p.Annotations[batchv1.JobCompletionIndexAnnotation])
	if err != nil {
		return 0, "", fmt.Errorf("pod %s has no completion index: %w", p.Name, err)
	}
	offset, group := 0, ""
	for _, container := range p.Spec.Containers {
		for _, env := range container.Env {
			switch env.Name {
			case jobenv.RankOffset:
				if offset, err = strconv.Atoi(env.Value); err != nil {
					return 0, "", fmt.Errorf("pod %s: %s %q: %w", p.Name, jobenv.RankOffset, env.Value, err)
				}
			case jobenv.Group:
				group = env.Value
			}
		}
	}
	return offset + index, group, nil
}
