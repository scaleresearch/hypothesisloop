package k8sexec

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

func phaseDetailTestClient() *JobWorkloadClient {
	return &JobWorkloadClient{apiURL: "http://registry"}
}

func schedulingPod(name string, scheduled bool, message string) *corev1.Pod {
	return schedulingPodAttempt(name, scheduled, message, 0)
}

func schedulingPodAttempt(name string, scheduled bool, message string, attempt int) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: HypothesisLoopNamespace,
			Labels: map[string]string{
				workloadkeys.ExperimentID: "gang-exp",
				workloadkeys.Attempt:      fmt.Sprintf("%d", attempt),
			},
		},
	}
	status := corev1.ConditionFalse
	if scheduled {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: status, Message: message},
	}
	return pod
}

// Two of three ranks bound, one Pending: scheduledNodes must count only the bound pods.
func TestPollPhaseDetailCountsScheduledNodesOnAPartialGang(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = chartKube(t,
		schedulingPod("gang-exp-0", true, ""),
		schedulingPod("gang-exp-1", true, ""),
		schedulingPod("gang-exp-2", false, "0/3 nodes are available: 3 Insufficient nvidia.com/gpu."),
	)

	_, _, _, scheduledNodes, err := c.PollPhaseDetail(context.Background(), "gang-exp", 0)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 2 {
		t.Errorf("scheduledNodes = %d, want 2 — a landed rank must be counted, a still-Pending one must not", scheduledNodes)
	}
}

// A terminating pod from a superseded attempt (recreated after a failover, or an Indexed Job's
// replaced index) must not inflate the current attempt's scheduledNodes — otherwise a genuinely
// 0-of-2-bound gang reads as fully bound because the previous attempt's pods are still around,
// which disables the placement-deadline check that is supposed to bound exactly this case.
func TestPollPhaseDetailCountsOnlyTheRequestedAttemptsPods(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = chartKube(t,
		schedulingPodAttempt("gang-exp-0-old", true, "", 0),
		schedulingPodAttempt("gang-exp-1-old", true, "", 0),
	)

	_, _, _, scheduledNodes, err := c.PollPhaseDetail(context.Background(), "gang-exp", 1)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 0 {
		t.Errorf("scheduledNodes = %d, want 0 — attempt 0's bound pods must not count toward attempt 1", scheduledNodes)
	}
}

// clusterapi.AttemptUnknown (the executor could not resolve an attempt) must fall back to counting
// every pod under the experiment, today's pre-attempt-label behaviour, rather than filtering on a
// label value that doesn't exist.
func TestPollPhaseDetailCountsEveryPodWhenAttemptIsUnknown(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = chartKube(t,
		schedulingPodAttempt("gang-exp-0", true, "", 3),
	)

	_, _, _, scheduledNodes, err := c.PollPhaseDetail(context.Background(), "gang-exp", clusterapi.AttemptUnknown)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 1 {
		t.Errorf("scheduledNodes = %d, want 1", scheduledNodes)
	}
}

// One rank of a gang crashed while another still waits for its image. The wait was reported and
// the crash hidden, so the job failed with the words of a pull that was going fine.
func TestACrashedRankIsReportedBesideARankThatStillWaits(t *testing.T) {
	waiting := schedulingPod("gang-exp-0", true, "")
	waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating", Message: "pulling image"}},
	}}
	crashed := schedulingPod("gang-exp-1", true, "")
	crashed.Status.ContainerStatuses = []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
	}}
	c := phaseDetailTestClient()
	c.kube = chartKube(t, waiting, crashed)

	reason, message, _, scheduledNodes, err := c.PollPhaseDetail(context.Background(), "gang-exp", 0)
	if err != nil || reason != clusterapi.PhaseReasonOOMKilled || message != "container exited with code 137 (OOMKilled)" || scheduledNodes != 2 {
		t.Fatalf("reason %q, message %q, scheduled %d, %v, want the crash", reason, message, scheduledNodes, err)
	}

	// With no crash the wait is what there is to say.
	c.kube = chartKube(t, waiting)
	if reason, message, _, _, err := c.PollPhaseDetail(context.Background(), "gang-exp", 0); err != nil || reason != "" || message != "pulling image" {
		t.Fatalf("a waiting rank alone: reason %q, message %q, %v", reason, message, err)
	}
}

// The kube-system namespace UID is the runtime's stable cluster fingerprint (autoscaler.md's
// "Cluster identity") — it must come back verbatim, not derived or reformatted.
func TestGetClusterIDReturnsTheKubeSystemNamespaceUID(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = chartKube(t, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "fixed-cluster-uid-1234"},
	})

	id, err := c.GetClusterID(context.Background())
	if err != nil {
		t.Fatalf("GetClusterID: %v", err)
	}
	if id != "fixed-cluster-uid-1234" {
		t.Errorf("GetClusterID = %q, want the kube-system namespace UID", id)
	}
}

func TestAContainerThatExitedWithTheCodeUnreachableCodeIsReportedSo(t *testing.T) {
	reason, message, _ := terminatedPhaseDetail(&corev1.ContainerStateTerminated{ExitCode: clusterapi.ExitCodeCodeUnreachable, Reason: "Error"}, 0)
	if reason != clusterapi.PhaseReasonCodeUnreachable || message != "container exited with code 75 (Error)" {
		t.Fatalf("got %q %q", reason, message)
	}
	if reason, _, _ := terminatedPhaseDetail(&corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}, 0); reason != clusterapi.PhaseReasonContainerFailed {
		t.Fatalf("exit 1 is %q", reason)
	}
	if reason, _, _ := terminatedPhaseDetail(&corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}, 0); reason != clusterapi.PhaseReasonOOMKilled {
		t.Fatalf("an OOM kill is %q", reason)
	}
}
