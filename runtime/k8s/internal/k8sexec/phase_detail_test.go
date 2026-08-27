package k8sexec

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
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

// Two of three ranks bound, one Pending with the autoscaler's own refusal text — the exact
// "2 nodes free, 3 more needed" case autoscaler.md's gang-scheduling section describes.
// scheduledNodes must count only the bound pods, and schedulingReason must surface the refusal
// from the still-Pending one so the watcher's early-exit path has something to read.
func TestPollPhaseDetailCountsScheduledNodesAndSurfacesTheAutoscalerRefusalOnAPartialGang(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = fake.NewClientset(
		schedulingPod("gang-exp-0", true, ""),
		schedulingPod("gang-exp-1", true, ""),
		schedulingPod("gang-exp-2", false, "0/3 nodes are available: 3 Insufficient nvidia.com/gpu."),
	)

	_, _, _, scheduledNodes, schedulingReason, err := c.PollPhaseDetail(context.Background(), "gang-exp", 0)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 2 {
		t.Errorf("scheduledNodes = %d, want 2 — a landed rank must be counted, a still-Pending one must not", scheduledNodes)
	}
	if schedulingReason != "0/3 nodes are available: 3 Insufficient nvidia.com/gpu." {
		t.Errorf("schedulingReason = %q, want the Pending pod's PodScheduled=False condition message", schedulingReason)
	}
}

// A fully-bound gang has no Pending pod to explain, so schedulingReason must be empty even though
// every pod carries a PodScheduled=True condition with no message.
func TestPollPhaseDetailReportsNoSchedulingReasonWhenEveryPodHasBound(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = fake.NewClientset(
		schedulingPod("gang-exp-0", true, ""),
		schedulingPod("gang-exp-1", true, ""),
	)

	_, _, _, scheduledNodes, schedulingReason, err := c.PollPhaseDetail(context.Background(), "gang-exp", 0)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 2 {
		t.Errorf("scheduledNodes = %d, want 2", scheduledNodes)
	}
	if schedulingReason != "" {
		t.Errorf("schedulingReason = %q, want empty — nothing is Pending", schedulingReason)
	}
}

// A terminating pod from a superseded attempt (recreated after a failover, or an Indexed Job's
// replaced index) must not inflate the current attempt's scheduledNodes — otherwise a genuinely
// 0-of-2-bound gang reads as fully bound because the previous attempt's pods are still around,
// which disables the placement-deadline check that is supposed to bound exactly this case.
func TestPollPhaseDetailCountsOnlyTheRequestedAttemptsPods(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = fake.NewClientset(
		schedulingPodAttempt("gang-exp-0-old", true, "", 0),
		schedulingPodAttempt("gang-exp-1-old", true, "", 0),
	)

	_, _, _, scheduledNodes, _, err := c.PollPhaseDetail(context.Background(), "gang-exp", 1)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 0 {
		t.Errorf("scheduledNodes = %d, want 0 — attempt 0's bound pods must not count toward attempt 1", scheduledNodes)
	}
}

// workload.AttemptUnknown (the executor could not resolve an attempt) must fall back to counting
// every pod under the experiment, today's pre-attempt-label behaviour, rather than filtering on a
// label value that doesn't exist.
func TestPollPhaseDetailCountsEveryPodWhenAttemptIsUnknown(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = fake.NewClientset(
		schedulingPodAttempt("gang-exp-0", true, "", 3),
	)

	_, _, _, scheduledNodes, _, err := c.PollPhaseDetail(context.Background(), "gang-exp", workload.AttemptUnknown)
	if err != nil {
		t.Fatalf("PollPhaseDetail: %v", err)
	}
	if scheduledNodes != 1 {
		t.Errorf("scheduledNodes = %d, want 1", scheduledNodes)
	}
}

// The kube-system namespace UID is the runtime's stable cluster fingerprint (autoscaler.md's
// "Cluster identity") — it must come back verbatim, not derived or reformatted.
func TestGetClusterIDReturnsTheKubeSystemNamespaceUID(t *testing.T) {
	c := phaseDetailTestClient()
	c.kube = fake.NewClientset(&corev1.Namespace{
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
