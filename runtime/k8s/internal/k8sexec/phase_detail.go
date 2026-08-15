package k8sexec

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
// terminated container state and reports it. Read live on every call — no caching
// (important.md #4). Returns empty reason/message and nil error when nothing notable is
// happening (pod not scheduled yet, or every container running cleanly).
func (c *JobWorkloadClient) PollPhaseDetail(ctx context.Context, experimentID string) (reason, message string, restartCount int32, err error) {
	pods, err := c.kube.CoreV1().Pods(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return "", "", 0, fmt.Errorf("workload: list pods for %s: %w", experimentID, err)
	}

	var maxRestarts int32
	var firstWaiting *corev1.ContainerStateWaiting
	var firstTerminated *corev1.ContainerStateTerminated
	for i := range pods.Items {
		p := &pods.Items[i]
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
		return k8sPhaseReasons[firstWaiting.Reason], firstWaiting.Message, maxRestarts, nil
	case firstTerminated != nil:
		return terminatedPhaseDetail(firstTerminated, maxRestarts)
	default:
		return "", "", maxRestarts, nil
	}
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
