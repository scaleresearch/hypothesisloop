package k8sexec

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// buildAffinity translates the DSL's placement vocabulary into native affinity rules: which
// hardware the resolved accelerator placement pins to, and how this job's ranks sit relative to
// each other (JobSpec.Topology). JobSpec.NodeSelector is unrelated placement and is applied
// directly as a pod nodeSelector, not here. Returns nil when nothing was requested.
func (c *JobWorkloadClient) buildAffinity(experimentID string, spec clusterapi.JobSpec, placement AcceleratorPlacement, distributed bool) *corev1.Affinity {
	aff := &corev1.Affinity{
		NodeAffinity:    acceleratorNodeAffinity(placement),
		PodAffinity:     nil,
		PodAntiAffinity: nil,
	}

	if distributed {
		selector := &metav1.LabelSelector{
			MatchLabels: map[string]string{workloadkeys.ExperimentID: experimentID},
		}

		spreadAcrossHosts := true
		sameZone := false
		if spec.Topology != nil {
			if spec.Topology.SpreadAcrossHosts != nil {
				spreadAcrossHosts = *spec.Topology.SpreadAcrossHosts
			}
			sameZone = spec.Topology.SameZone
		}

		if spreadAcrossHosts {
			// Hard requirement: no two ranks may share a physical host. Trivially satisfiable
			// for the first rank scheduled (no siblings yet), so it never risks the job going
			// unschedulable — unlike a same-zone *requirement* would (see below).
			aff.PodAntiAffinity = &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: selector,
					TopologyKey:   "kubernetes.io/hostname",
				}},
			}
			// Scheduler topology admission proves NumNodes distinct hosts each report
			// AcceleratorCount free before this hard rule enters desired state.
		}
		if sameZone {
			// Soft preference, not a requirement: the first rank has no sibling to co-locate
			// with, so a *required* same-zone affinity would make it permanently
			// unschedulable. Preferred affinity nudges later ranks toward the first ranks'
			// zone, minimizing cross-zone latency for gradient all-reduce, with no deadlock risk.
			aff.PodAffinity = &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: selector,
						TopologyKey:   "topology.kubernetes.io/zone",
					},
				}},
			}
		}
	}

	if aff.NodeAffinity == nil && aff.PodAffinity == nil && aff.PodAntiAffinity == nil {
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
