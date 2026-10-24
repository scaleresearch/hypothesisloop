package k8sexec

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// buildAffinity translates the DSL's placement vocabulary into native affinity rules: which
// hardware the resolved accelerator placement pins to, and that no two ranks of a distributed job
// share a host (JobSpec.Topology). JobSpec.NodeSelector, the zone or placement group the control
// plane chose among them, is applied directly as a pod nodeSelector, not here. Returns nil when
// nothing was requested.
func (c *JobWorkloadClient) buildAffinity(experimentID string, spec clusterapi.JobSpec, placement AcceleratorPlacement, distributed bool) *corev1.Affinity {
	aff := &corev1.Affinity{
		NodeAffinity:    acceleratorNodeAffinity(placement),
		PodAntiAffinity: nil,
	}

	spreadAcrossHosts := spec.Topology == nil || spec.Topology.SpreadAcrossHosts == nil || *spec.Topology.SpreadAcrossHosts
	if distributed && spreadAcrossHosts {
		// Hard requirement: no two ranks may share a physical host. Trivially satisfiable for the
		// first rank scheduled (no siblings yet), so it never risks the job going unschedulable.
		// Scheduler topology admission proves NumNodes distinct hosts each report
		// AcceleratorCount free before this hard rule enters desired state.
		aff.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{workloadkeys.ExperimentID: experimentID}},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		}
	}

	if aff.NodeAffinity == nil && aff.PodAntiAffinity == nil {
		return nil
	}
	return aff
}

// acceleratorTolerations tolerates the NoSchedule taint keyed by the extended resource a node
// group's pods request, the one accelerator nodes carry to keep other pods off (the NVIDIA GPU
// Operator's nvidia.com/gpu, as Kubernetes' ExtendedResourceToleration admission plugin
// tolerates it). Nothing else is tolerated, so a job never lands on a node tainted for any other
// reason, a control-plane node among them.
func acceleratorTolerations(acceleratorCount int, placement AcceleratorPlacement) []corev1.Toleration {
	if acceleratorCount <= 0 || placement.ResourceName == "" {
		return nil
	}
	return []corev1.Toleration{{Key: placement.ResourceName, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
}
