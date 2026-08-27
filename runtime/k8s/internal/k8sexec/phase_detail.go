package k8sexec

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// k8sPhaseReasons translates the small set of Kubernetes container-status waiting reasons that
// never self-heal into the control plane's generic domain.PhaseReason* vocabulary — the
// control plane must never learn Kubernetes' own reason strings (important.md #7). Reasons not
// in this map (CrashLoopBackOff, ContainerCreating, PodInitializing, ...) are expected to
// resolve on their own and are reported with an empty Reason, message only.
var k8sPhaseReasons = map[string]string{
	"ErrImagePull":               domain.PhaseReasonImagePullFailed,
	"ImagePullBackOff":           domain.PhaseReasonImagePullFailed,
	"InvalidImageName":           domain.PhaseReasonInvalidImage,
	"CreateContainerConfigError": domain.PhaseReasonConfigError,
}

// PollPhaseDetail inspects experimentID's currently-scheduled pod(s) for a waiting or
// terminated container state and reports it, plus the gang-readiness facts the control plane's
// scale-up-timeout watcher needs: scheduledNodes (pods with condition PodScheduled=True) and
// schedulingReason (the PodScheduled=False condition's message on a still-Pending pod — this is
// where CA's "0/1 nodes are available" / Karpenter's "incompatible with nodepool" messages
// surface; a Pending pod already carries this on its own status, so no separate Events read is
// needed). Read live on every call — no caching (important.md #4). Returns empty reason/message
// and nil error when nothing notable is happening (pod not scheduled yet, or every container
// running cleanly).
//
// attempt scopes the pod list to the current generation: a pod from a superseded attempt
// (recreated after a failover, or an Indexed Job's replaced index) still lists under
// experimentID by label alone, and would otherwise inflate scheduledNodes with a pod this
// attempt never created — reading, for example, a genuinely 0-of-2-bound gang as fully bound
// because attempt N-1's terminating pods are still around. workload.AttemptUnknown (the executor
// could not resolve one) falls back to counting every pod under the experiment, today's
// behaviour, rather than filtering on a label value that doesn't exist.
func (c *JobWorkloadClient) PollPhaseDetail(ctx context.Context, experimentID string, attempt int) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, err error) {
	selector := fmt.Sprintf("%s=%s", workloadkeys.ExperimentID, experimentID)
	if attempt != workload.AttemptUnknown {
		selector = fmt.Sprintf("%s,%s=%d", selector, workloadkeys.Attempt, attempt)
	}
	pods, err := c.kube.CoreV1().Pods(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", "", 0, 0, "", fmt.Errorf("workload: list pods for %s: %w", experimentID, err)
	}

	var maxRestarts int32
	var firstWaiting *corev1.ContainerStateWaiting
	var firstTerminated *corev1.ContainerStateTerminated
	for i := range pods.Items {
		p := &pods.Items[i]
		if scheduled, unscheduledReason := podScheduledCondition(p); scheduled {
			scheduledNodes++
		} else if unscheduledReason != "" && schedulingReason == "" {
			schedulingReason = unscheduledReason
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > maxRestarts {
				maxRestarts = cs.RestartCount
			}
			if cs.State.Waiting != nil && firstWaiting == nil {
				firstWaiting = cs.State.Waiting
			}
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 && firstTerminated == nil {
				firstTerminated = cs.State.Terminated
			}
		}
	}

	switch {
	case firstWaiting != nil:
		return k8sPhaseReasons[firstWaiting.Reason], firstWaiting.Message, maxRestarts, scheduledNodes, schedulingReason, nil
	case firstTerminated != nil:
		r, m, rc, tErr := terminatedPhaseDetail(firstTerminated, maxRestarts)
		return r, m, rc, scheduledNodes, schedulingReason, tErr
	default:
		return "", "", maxRestarts, scheduledNodes, schedulingReason, nil
	}
}

// podScheduledCondition reports whether p's PodScheduled condition is True, and when it is
// explicitly False, the condition's message — the autoscaler's own refusal text when one exists
// (e.g. "0/3 nodes are available: 3 Insufficient nvidia.com/gpu"), empty otherwise (condition not
// yet reported, or scheduling simply hasn't been attempted).
func podScheduledCondition(p *corev1.Pod) (scheduled bool, unscheduledMessage string) {
	for _, cond := range p.Status.Conditions {
		if cond.Type != corev1.PodScheduled {
			continue
		}
		if cond.Status == corev1.ConditionTrue {
			return true, ""
		}
		return false, cond.Message
	}
	return false, ""
}

// terminatedPhaseDetail describes a container that exited non-zero. The exit code and
// Kubernetes' own termination Reason used to be dropped on the floor here, and
// Terminated.Message is empty unless the workload writes to terminationMessagePath — which a
// crashing Python process does not. The result was a FAILED experiment carrying no failure
// information whatsoever: the record showed a status and nothing else, leaving the job's log
// tail as the single diagnostic channel. Reported through phase_detail rather than a new column
// because it reaches the experiment record on the existing path, with no schema change.
func terminatedPhaseDetail(t *corev1.ContainerStateTerminated, restarts int32) (string, string, int32, error) {
	reason := domain.PhaseReasonContainerFailed
	if t.Reason == "OOMKilled" {
		reason = domain.PhaseReasonOOMKilled
	}
	message := fmt.Sprintf("container exited with code %d", t.ExitCode)
	if t.Reason != "" {
		message += " (" + t.Reason + ")"
	}
	// Only present when the workload wrote to terminationMessagePath; usually empty.
	if t.Message != "" {
		message += ": " + t.Message
	}
	return reason, message, restarts, nil
}
