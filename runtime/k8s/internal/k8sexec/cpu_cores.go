package k8sexec

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// nodeCores counts each node's whole cores as units of clusterapi.CPUType: total is what platform
// jobs can hold in all (the cores GetNodeTotalCapacity reports), free what no pod has requested.
// Nothing is written to the cluster for it. A job on cores requests plain cpu, so the cores an
// accelerator job holds are gone from the free count like any other.
func nodeCores(nodes []corev1.Node, pods []corev1.Pod) (free, total map[string]int64) {
	held := make(map[string]int64, len(nodes))
	heldByOthers := make(map[string]int64, len(nodes))
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		requests := podRequests(&pod)
		milli := requests.Cpu().MilliValue()
		held[pod.Spec.NodeName] += milli
		if pod.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue {
			heldByOthers[pod.Spec.NodeName] += milli
		}
	}
	free, total = make(map[string]int64, len(nodes)), make(map[string]int64, len(nodes))
	for _, node := range nodes {
		allocatable := node.Status.Allocatable.Cpu().MilliValue()
		free[node.Name] = max(allocatable-held[node.Name], 0) / 1000
		total[node.Name] = max(allocatable-heldByOthers[node.Name], 0) / 1000
	}
	return free, total
}

// podRequests is what the scheduler holds of a node for pod: its containers, its sidecars (init
// containers that keep running) on top of them, at least what any one-shot init container needs
// beside the sidecars started before it, and the pod's overhead.
func podRequests(pod *corev1.Pod) corev1.ResourceList {
	reqs := corev1.ResourceList{}
	for _, ctr := range pod.Spec.Containers {
		addRequests(reqs, ctr.Resources.Requests)
	}
	sidecars, initMax := corev1.ResourceList{}, corev1.ResourceList{}
	for _, ctr := range pod.Spec.InitContainers {
		need := corev1.ResourceList{}
		if ctr.RestartPolicy != nil && *ctr.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			addRequests(sidecars, ctr.Resources.Requests)
			addRequests(need, sidecars)
		} else {
			addRequests(need, ctr.Resources.Requests)
			addRequests(need, sidecars)
		}
		for name, q := range need {
			if cur, ok := initMax[name]; !ok || q.Cmp(cur) > 0 {
				initMax[name] = q
			}
		}
	}
	addRequests(reqs, sidecars)
	for name, q := range initMax {
		if cur, ok := reqs[name]; !ok || q.Cmp(cur) > 0 {
			reqs[name] = q
		}
	}
	addRequests(reqs, pod.Spec.Overhead)
	return reqs
}

func addRequests(into, from corev1.ResourceList) {
	for name, q := range from {
		sum := into[name]
		sum.Add(q)
		into[name] = sum
	}
}

// onCores reports whether exp holds cores and no device, so that nothing but its cpu is requested
// for it and it needs no placement.
func onCores(exp *clusterapi.DesiredJob) bool {
	return exp.AcceleratorType.Kind() == clusterapi.TypeKindCPU
}
