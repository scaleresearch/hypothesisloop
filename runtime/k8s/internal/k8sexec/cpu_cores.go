package k8sexec

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// nodeCores counts each node's whole cores as units of domain.CPUType: total is what platform
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
		for _, ctr := range pod.Spec.Containers {
			milli := ctr.Resources.Requests.Cpu().MilliValue()
			held[pod.Spec.NodeName] += milli
			if pod.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue {
				heldByOthers[pod.Spec.NodeName] += milli
			}
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

// onCores reports whether exp holds cores and no device, so that nothing but its cpu is requested
// for it and it needs no placement.
func onCores(exp *domain.Experiment) bool {
	return exp.AcceleratorType.Kind() == domain.TypeKindCPU
}
