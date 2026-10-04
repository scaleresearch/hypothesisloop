package k8sexec

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	kube := chartKube(t, node, daemonset, platform)
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

// A label counts the devices of the one extended resource its domain advertises on the node. A
// node with two resources in a domain used to report whichever the map walk reached last per node
// and both added together in the total; placement refuses such a node, so it reports none.
func TestANodeWithTwoResourcesInOneDomainReportsNoCapacityForIt(t *testing.T) {
	gpuNode := func(name string, resources map[corev1.ResourceName]int64) *corev1.Node {
		node := totalCapacityTestNode(name, 8, 32)
		node.Labels = map[string]string{"nvidia.com/gpu.product": "H100"}
		for resourceName, count := range resources {
			node.Status.Allocatable[resourceName] = *resource.NewQuantity(count, resource.DecimalSI)
		}
		return node
	}
	holder := requestingPod("holder", "plain", true, 1000)
	holder.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = *resource.NewQuantity(1, resource.DecimalSI)
	c := newDRATestClient(t)
	c.kube = chartKube(t,
		gpuNode("plain", map[corev1.ResourceName]int64{"nvidia.com/gpu": 4}),
		gpuNode("mixed", map[corev1.ResourceName]int64{"nvidia.com/gpu": 2, "nvidia.com/mig-1g.5gb": 7}),
		holder)

	const key = "nvidia.com/gpu.product=H100"
	for range 20 {
		available, total, byNode, _, err := c.GetLiveAcceleratorCapacitySnapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, mixedReported := byNode["mixed"][key]
		if total[key] != 4 || available[key] != 3 || byNode["plain"][key] != 3 || mixedReported {
			t.Fatalf("total %d, available %d, by node %v; want the plain node's 4 with 3 free and nothing for the mixed node", total[key], available[key], byNode)
		}
	}
}

// A node's whole cores are units of the CPU type, counted from what Kubernetes already says and
// written nowhere. A chip job and a job on cores share a node: the cores the chip job holds are
// gone from the free count, and the job on cores is a pod that requests its cpu and nothing else.
func TestANodesCoresAreUnitsOfTheCPUTypeWithNothingWrittenToTheCluster(t *testing.T) {
	milliNode := func(name string, cpuMilli int64) *corev1.Node {
		node := totalCapacityTestNode(name, 0, 64)
		node.Status.Allocatable[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)
		return node
	}
	cordoned := milliNode("cordoned", 16000)
	cordoned.Spec.Unschedulable = true
	c := newDRATestClient(t)
	kube := chartKube(t, milliNode("box", 15800), milliNode("small", 3999), cordoned,
		requestingPod("cni-agent", "box", false, 300), requestingPod("chip-job", "box", true, 4000))
	c.kube = kube
	ctx := context.Background()

	available, total, byNode, _, err := c.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cores := string(domain.CPUType)
	if total[cores] != 18 || available[cores] != 14 || byNode["box"][cores] != 11 || byNode["small"][cores] != 3 || len(total) != 1 {
		t.Errorf("total %v, available %v, by node %v; want 18 cores in all (15 and 3), 14 free, 11 of them on box", total, available, byNode)
	}
	totals, err := c.GetNodeTotalCapacity(ctx)
	if err != nil || totals["box"][domain.NodeResourceCPUMillicores]/1000 != 15 {
		t.Errorf("box's cores for platform jobs = %v, %v; want the 15 its units add up to", totals["box"], err)
	}
	for _, action := range kube.Actions() {
		if verb := action.GetVerb(); verb != "list" && verb != "get" && verb != "watch" {
			t.Errorf("reading capacity made a %s on %s", verb, action.GetResource().Resource)
		}
	}

	exp := attemptTestExperiment(0)
	exp.AcceleratorType, exp.AcceleratorCount = domain.CPUType, 4
	exp.Job.AcceleratorType, exp.Job.AcceleratorCount, exp.Job.CPU = domain.CPUType, 4, "4"
	placement, err := c.resolvePlacementFor(ctx, exp)
	if err != nil || placement != (AcceleratorPlacement{}) {
		t.Fatalf("placement of a job on cores = %+v, %v; want none needed", placement, err)
	}
	job, err := c.BuildJob(exp, placement)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template
	requests := pod.Spec.Containers[0].Resources.Requests
	for name := range requests {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
			t.Errorf("a job on cores requests %s", name)
		}
	}
	pinned := pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil
	if requests.Cpu().Value() != 4 || pinned || len(pod.Spec.ResourceClaims) != 0 {
		t.Errorf("requests %v, pinned to nodes %v, claims %v; want 4 cores on any node", requests, pinned, pod.Spec.ResourceClaims)
	}
	if got := pod.Annotations[AcceleratorTypeAnnotation]; got != cores {
		t.Errorf("the pod is recorded as %q, want the CPU type", got)
	}
}

// The builder's BuildKit is a sidecar of the cluster-agent pod, and the scheduler holds its
// request on the node. Capacity counted from the containers alone reported box-a's 4 builder
// cores as free for jobs, and a job sized to them waited Pending.
func TestASidecarsRequestIsHeldOfItsNodeAsTheSchedulerHoldsIt(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	agent := requestingPod("cluster-agent", "box", false, 10)
	agent.Spec.InitContainers = []corev1.Container{
		{Name: "setup", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: *resource.NewQuantity(6, resource.DecimalSI)}}},
		{Name: "buildkitd", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(8<<30, resource.BinarySI)}}},
	}
	agent.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(100, resource.DecimalSI)}
	if got := podRequests(agent); got.Cpu().MilliValue() != 6100 || got.Memory().Value() != 8<<30 {
		t.Fatalf("pod requests %v; want cpu 6100m (the one-shot init's 6 cores exceed 4010m running) and the sidecar's 8Gi", got)
	}
	agent.Spec.InitContainers = agent.Spec.InitContainers[1:]

	c := newDRATestClient(t)
	c.kube = chartKube(t, totalCapacityTestNode("box", 16, 64), agent)
	ctx := context.Background()
	available, total, _, _, err := c.GetLiveAcceleratorCapacitySnapshot(ctx)
	cores := string(domain.CPUType)
	if err != nil || total[cores] != 11 || available[cores] != 11 {
		t.Fatalf("cores total %v, available %v, %v; want 11 of 16 once the agent's 4110m are held", total, available, err)
	}
	totals, err := c.GetNodeTotalCapacity(ctx)
	if err != nil || totals["box"][domain.NodeResourceMemoryBytes] != 56<<30 {
		t.Fatalf("box's totals %v, %v; want 56Gi of memory left after the sidecar's 8Gi", totals["box"], err)
	}
	free, err := c.GetLiveNodeResourceCapacity(ctx)
	if err != nil || free["box"][domain.NodeResourceCPUMillicores] != 11890 {
		t.Fatalf("box's free %v, %v; want 11890m", free["box"], err)
	}
}
