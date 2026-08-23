package k8sexec

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

func totalCapacityTestNode(name string, cpuCores, memGi int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGi<<30, resource.BinarySI),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func requestingPod(name, node string, managed bool, cpuMilli int64) *corev1.Pod {
	labels := map[string]string{}
	if managed {
		labels[workloadkeys.ManagedBy] = workloadkeys.ManagedByValue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A DaemonSet pod (or any other non-platform-managed pod) permanently occupies real capacity;
// counting the node's raw Allocatable instead double-counts it. GetNodeTotalCapacity must exclude
// it, while a platform job pod's own request must NOT be subtracted (see the function's doc
// comment) — the fair-share denominator must not move just because a platform job is running.
func TestGetNodeTotalCapacityExcludesNonPlatformPodsButNotPlatformOnes(t *testing.T) {
	node := totalCapacityTestNode("node-a", 8, 32)
	daemonset := requestingPod("cni-agent", "node-a", false, 500) // 500m — not platform-managed
	platform := requestingPod("hl-job-1", "node-a", true, 2000)   // 2000m — platform-managed

	kube := fake.NewClientset(node, daemonset, platform)
	c := &JobWorkloadClient{kube: kube}

	got, err := c.GetNodeTotalCapacity(context.Background())
	if err != nil {
		t.Fatalf("GetNodeTotalCapacity: %v", err)
	}

	// Raw allocatable is 8000m. A naive read (no exclusion at all) would report 8000m here.
	const rawAllocatableMilli = 8000
	const daemonSetRequestMilli = 500
	want := int64(rawAllocatableMilli - daemonSetRequestMilli) // platform pod's 2000m is NOT subtracted
	if got["node-a"][domain.NodeResourceCPUMillicores] != want {
		t.Fatalf("node-a cpu_millicores = %d, want %d (allocatable %d minus only the non-platform pod's %d; the platform pod's own request must not move this denominator)",
			got["node-a"][domain.NodeResourceCPUMillicores], want, int64(rawAllocatableMilli), int64(daemonSetRequestMilli))
	}
	if got["node-a"][domain.NodeResourceCPUMillicores] == rawAllocatableMilli {
		t.Fatal("GetNodeTotalCapacity reported the raw allocatable total unchanged — the DaemonSet pod's permanent request was not excluded")
	}
}
