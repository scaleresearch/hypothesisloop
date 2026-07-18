package workload

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// buildAffinity translates the DSL's hardware/placement vocabulary — GPUSpec.AcceptableGPUTypes
// and JobSpec.Topology — into the native affinity rules that make real multi-host GPU
// training actually work: which GPU models a node must advertise, and how this job's own
// ranks must (or should) be placed relative to each other. Returns nil when nothing was
// requested, so a plain single-node/single-GPU-type job gets no affinity section at all.
// resourceNameFor/taintKeyFor/nodeLabelKeyFor return gpuType's own vendor plumbing (from the
// per-type maps built from openresearch.yaml's gpu_types entries — see config.GPUTypeConfig),
// falling back to the client's cluster-wide default when gpuType has no entry (old rows,
// GPU-less dev clusters using the "cpu" substitution).
func (c *JobWorkloadClient) resourceNameFor(gpuType domain.GPUType) string {
	if v, ok := c.resourceNameByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return c.gpuResourceName
}

func (c *JobWorkloadClient) taintKeyFor(gpuType domain.GPUType) string {
	if v, ok := c.taintKeyByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return c.gpuTaintKey
}

func (c *JobWorkloadClient) nodeLabelKeyFor(gpuType domain.GPUType) string {
	if v, ok := c.nodeLabelKeyByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return "nvidia.com/gpu.product"
}

func (c *JobWorkloadClient) buildAffinity(experimentID string, spec domain.JobSpec, distributed bool) *corev1.Affinity {
	aff := &corev1.Affinity{
		NodeAffinity:    c.gpuNodeAffinity(spec.GPUType, spec.AcceptableGPUTypes),
		PodAffinity:     nil,
		PodAntiAffinity: nil,
	}

	if distributed {
		selector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"openresearch.io/experiment-id": experimentID},
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
			// Hard requirement: no two ranks of this job may share a physical host. This
			// is trivially satisfiable for the very first rank scheduled (no sibling pods
			// exist yet to conflict with), so it never risks the job going unschedulable —
			// unlike a same-zone *requirement* would (see below).
			aff.PodAntiAffinity = &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: selector,
					TopologyKey:   "kubernetes.io/hostname",
				}},
			}
			// KNOWN GAP: admission (scheduler/loop_tick.go) only checks the target cluster's
			// aggregate free GPU count against NumNodes*GPUCount before submitting this Job
			// - it never verifies NumNodes distinct hosts each have GPUCount free, which this
			// hard anti-affinity actually requires. A cluster can pass the aggregate check
			// while that capacity is concentrated on fewer hosts than NumNodes, so some ranks
			// schedule and hold GPUs while the rest stay Pending indefinitely (a real
			// deadlock - nothing but the generic stuck-pending timeout currently detects or
			// recovers from it). Fixing this needs per-node (not just per-cluster-per-flavor)
			// capacity reporting, which doesn't exist yet; see admissionUnit's doc comment in
			// controlplane/services/scheduler/loop_types.go.
		}
		if sameZone {
			// Soft preference, not a requirement: the first rank scheduled has no sibling
			// pod yet to co-locate with, so a *required* same-zone affinity would make
			// that first pod permanently unschedulable. Preferred affinity instead nudges
			// later ranks toward whichever zone the first ranks land in, minimizing
			// cross-zone network latency for gradient all-reduce without any deadlock risk.
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

// gpuTolerations tolerates the taint real GPU nodes carry (commonly applied by the NVIDIA
// GPU Operator, e.g. nvidia.com/gpu=present:NoSchedule) so that GPU-requesting pods can
// actually land there — non-GPU workloads are meant to stay off those nodes, but a pod that
// legitimately requests GPUs must be able to schedule onto one. This is purely an
// execution-engine concern the agent never declares in JobSpec: any request with
// gpuCount > 0 gets it automatically.
func gpuTolerations(gpuCount int, taintKey string) []corev1.Toleration {
	if gpuCount <= 0 {
		return nil
	}
	return []corev1.Toleration{{
		Key:      taintKey,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
}

// gpuNodeAffinity translates a JobSpec's GPU hardware requirement into a native nodeAffinity
// over the well-known nvidia.com/gpu.product node label (set by the NVIDIA GPU Feature
// Discovery add-on), using the operator's GPUType->product-label mapping (nodeLabelByType,
// from openresearch.yaml). The acceptable set is AcceptableGPUTypes if the agent gave one
// (interchangeable scheduling), otherwise just [gpuType] — a plain gpu_type with no
// acceptable_gpu_types must still land on hardware of exactly that type: the extended
// resource request (nvidia.com/gpu quantity) alone only guarantees *a* GPU node, never *the
// requested model*, since every GPU type advertises the same resource name. Returns nil only
// when the cluster has no node-label mapping configured at all (e.g. GPU-less dev clusters
// substituting "cpu"), so a plain resource request is all that's meaningful there.
// gpuNodeAffinity builds the nodeAffinity for gpuType (or, if set, the OR of acceptable —
// GPU-type interchangeability). The node-label KEY used is always gpuType's own
// (nodeLabelKeyFor) — acceptable entries whose vendor plumbing uses a *different* label key
// are silently skipped, since mixing vendors within one job's acceptable set is incoherent
// (the k8s resource request itself is one specific extended-resource name; you can't ask for
// "amd.com/gpu OR nvidia.com/gpu" in a single container). In practice this means
// acceptable_gpu_types should only ever list types from the same vendor as gpu_type.
func (c *JobWorkloadClient) gpuNodeAffinity(gpuType domain.GPUType, acceptable []domain.GPUType) *corev1.NodeAffinity {
	if len(c.nodeLabelByType) == 0 || gpuType == "" {
		return nil
	}
	labelKey := c.nodeLabelKeyFor(gpuType)
	wanted := acceptable
	if len(wanted) == 0 {
		wanted = []domain.GPUType{gpuType}
	}
	values := make([]string, 0, len(wanted))
	for _, t := range wanted {
		if c.nodeLabelKeyFor(t) != labelKey {
			continue // different vendor's label key — can't OR them into one nodeAffinity term
		}
		if v, ok := c.nodeLabelByType[string(t)]; ok && v != "" {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      labelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   values,
				}},
			}},
		},
	}
}
