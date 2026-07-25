package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// AcceleratorTypeAnnotation records which accelerator type this run holds, set from
// exp.AcceleratorType and read back by ResolveAdmittedAcceleratorType to report what a pod
// actually landed on.
//
// An annotation, not a label, because an accelerator type is a driver-published "key=value"
// (see domain.AcceleratorType) and Kubernetes label *values* admit neither "/" nor "=" — a Job
// carrying one as a label is rejected outright by the API server. Sanitizing it into a legal
// label would break the round-trip this value exists for: the string read back has to be
// byte-identical to the one submitted, or the admitted type no longer matches the type quota
// billed against. Annotations have no such value restrictions. Nothing selects on this, so
// nothing is lost by not being a label.
const AcceleratorTypeAnnotation = "hypothesisloop.io/accelerator-type"

// AcceleratorCountAnnotation carries spec.AcceleratorCount onto the pod template — read back by
// GetLiveAcceleratorCapacitySnapshot's DRA branch, which (unlike the classic-mode branch) has no
// extended-resource request on the pod to sum directly.
const AcceleratorCountAnnotation = "hypothesisloop.io/accelerator-count"

const DesiredSpecHashAnnotation = "hypothesisloop.io/desired-spec-hash"

// BuildJob deterministically compiles exp's complete JobSpec DSL into a native batch/v1 Job —
// the only place the DSL vocabulary (image, command, env, cpu/memory/accelerator, num_nodes,
// max_retries, acceptable_accelerator_types) is translated into k8s concepts (containers,
// resource requests, node affinity, Indexed completion mode). Nothing upstream ever
// constructs or reads a k8s type.
//
// Distributed jobs (NumNodes > 1) get native Indexed completion mode, per-index retry, an
// OOM-aware pod failure policy, and rank/world-size env vars matching torchrun/Horovod convention.
//
// Pure: same desired state plus same placement always compiles to the same Job. It performs no
// cluster reads of its own, because its output feeds the desired-spec hash — resolving live
// inventory in here would make the hash drift whenever the cluster changed, and every job would
// look permanently "drifted" to reconcile. Callers resolve placement once (see
// ResolveAcceleratorPlacement) and pass it in.
func (c *JobWorkloadClient) BuildJob(exp *domain.Experiment, placement AcceleratorPlacement) (*batchv1.Job, error) {
	spec := exp.Job
	if spec.CPU == "" || spec.Memory == "" || spec.Storage == "" || spec.MaxRetries == nil || *spec.MaxRetries < 0 {
		return nil, fmt.Errorf("workload: CPU, memory, storage, and non-negative max_retries are required desired state")
	}
	if spec.AcceleratorCount > 0 && placement.ResourceName == "" && placement.DeviceClassName == "" {
		return nil, fmt.Errorf("workload: accelerator %q has no resolved placement", exp.AcceleratorType)
	}

	nodes := spec.NumNodes
	if nodes < 1 {
		nodes = 1
	}
	parallelism := int32(nodes)
	distributed := parallelism > 1

	backoff := int32(*spec.MaxRetries)

	deadline := int64(exp.EstimatedDurationHours * c.jobDeadlineMultiplier * 3600)
	if deadline < c.minJobDeadlineSeconds {
		deadline = c.minJobDeadlineSeconds
	}

	terminationGrace := c.defaultTerminationGracePeriodSeconds
	if spec.TerminationGracePeriodSeconds != nil {
		terminationGrace = *spec.TerminationGracePeriodSeconds
	}
	if terminationGrace > c.maxTerminationGracePeriodSeconds {
		terminationGrace = c.maxTerminationGracePeriodSeconds
	}

	priorityClass := PriorityClassGuaranteed
	if exp.CapacityTier == domain.CapacityBurst {
		priorityClass = PriorityClassBurst
	}

	restartPolicy := corev1.RestartPolicyOnFailure
	if distributed {
		// PodFailurePolicy (below) requires RestartPolicy=Never: failed containers must
		// surface as failed pods for the Job controller's per-index accounting, rather
		// than being retried in-place by the kubelet.
		restartPolicy = corev1.RestartPolicyNever
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if spec.CPU != "" {
		resources.Requests[corev1.ResourceCPU] = resource.MustParse(spec.CPU)
		resources.Limits[corev1.ResourceCPU] = resource.MustParse(spec.CPU)
	}
	if spec.Memory != "" {
		resources.Requests[corev1.ResourceMemory] = resource.MustParse(spec.Memory)
		resources.Limits[corev1.ResourceMemory] = resource.MustParse(spec.Memory)
	}
	if spec.Storage != "" {
		resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(spec.Storage)
		resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(spec.Storage)
	}
	// jobUsesDRA tracks whether this job needs a ResourceClaimTemplate/PodResourceClaim instead
	// of a plain extended-resource request. CreateWorkload reads this back off the returned
	// Job (non-empty Spec.ResourceClaims) to know whether to also create the ResourceClaimTemplate.
	jobUsesDRA := spec.AcceleratorCount > 0 && placement.UsesDRA()
	if spec.AcceleratorCount > 0 && !jobUsesDRA {
		acceleratorQty := resource.MustParse(fmt.Sprintf("%d", spec.AcceleratorCount))
		acceleratorResource := corev1.ResourceName(placement.ResourceName)
		resources.Requests[acceleratorResource] = acceleratorQty
		resources.Limits[acceleratorResource] = acceleratorQty
	}
	for name, qty := range spec.ExtraResources {
		if qty == "" {
			continue
		}
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return nil, fmt.Errorf("workload: extra_resources[%q]: %w", name, err)
		}
		resources.Requests[corev1.ResourceName(name)] = q
		resources.Limits[corev1.ResourceName(name)] = q
	}
	// Accelerator-required resources win over job-provided extras: a workload can't lower a
	// hardware runtime prerequisite its accelerator needs.
	for name, qty := range spec.AcceleratorPodResources {
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return nil, fmt.Errorf("workload: accelerator_pod_resources[%q]: %w", name, err)
		}
		resources.Requests[corev1.ResourceName(name)] = q
		resources.Limits[corev1.ResourceName(name)] = q
	}

	env := []corev1.EnvVar{
		{Name: "HYPOTHESISLOOP_EXPERIMENT_ID", Value: exp.ID},
		{Name: "HYPOTHESISLOOP_AGENT_ID", Value: exp.AgentID},
		{Name: "HYPOTHESISLOOP_PROJECT_ID", Value: exp.ProjectID},
		{Name: "HYPOTHESISLOOP_CODE_REF", Value: exp.CodeRef},
		{Name: "HYPOTHESISLOOP_CONFIG_HASH", Value: exp.ConfigHash},
		{Name: "HYPOTHESISLOOP_DATA_REF", Value: exp.DataRef},
		{Name: "HYPOTHESISLOOP_REGISTRY_URL", Value: c.registryURL},
		{Name: "HYPOTHESISLOOP_ACCELERATOR_TYPE", Value: string(exp.AcceleratorType)},
		{Name: "HYPOTHESISLOOP_ACCELERATOR_COUNT", Value: fmt.Sprintf("%d", exp.AcceleratorCount)},
		{Name: "HYPOTHESISLOOP_DURATION_SECONDS", Value: fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		{Name: "TRACEPARENT", Value: traceparentFromID(exp.ID)},
	}
	envNames := make([]string, 0, len(spec.Env))
	for name := range spec.Env {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)
	for _, k := range envNames {
		v := spec.Env[k]
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	if distributed {
		// HYPOTHESISLOOP_MASTER_ADDR is rank 0's stable DNS name — resolvable because BuildJob
		// sets pod.Spec.Subdomain to the job's own name and CreateWorkload creates a matching
		// headless Service, so k8s's Indexed Job hostname convention applies
		// ($job-name-$index.$subdomain.$namespace.svc.cluster.local). This is the rendezvous
		// endpoint training frameworks need: PyTorch DDP (torchrun --master-addr/--master-port),
		// Ray (`ray start --head` on rank 0, `ray start --address=...` on the rest), etc.
		masterAddr := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", jobName(exp.ID), jobName(exp.ID), HypothesisLoopNamespace)
		rankFieldRef := &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
			},
		}
		worldSize := fmt.Sprintf("%d", parallelism)
		masterPort := fmt.Sprintf("%d", DistributedMasterPort)
		env = append(env,
			corev1.EnvVar{Name: "HYPOTHESISLOOP_RANK", ValueFrom: rankFieldRef},
			corev1.EnvVar{Name: "HYPOTHESISLOOP_WORLD_SIZE", Value: worldSize},
			corev1.EnvVar{Name: "HYPOTHESISLOOP_MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "HYPOTHESISLOOP_MASTER_PORT", Value: masterPort},
			// Standard unprefixed names torch.distributed.init_process_group(env://) and
			// torchrun read directly, alongside the HYPOTHESISLOOP_-prefixed vars above. One
			// process per pod, so LOCAL_RANK is always 0.
			corev1.EnvVar{Name: "RANK", ValueFrom: rankFieldRef},
			corev1.EnvVar{Name: "LOCAL_RANK", Value: "0"},
			corev1.EnvVar{Name: "WORLD_SIZE", Value: worldSize},
			corev1.EnvVar{Name: "MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "MASTER_PORT", Value: masterPort},
		)
	}

	container := corev1.Container{
		Name:  "experiment",
		Image: spec.Image,
		// IfNotPresent rather than k8s's default (Always for :latest-tagged images): otherwise
		// a locally-built image is ignored and the kubelet always attempts a registry pull,
		// which fails on any cluster without a real registry behind the image reference.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         spec.Command,
		Args:            spec.Args,
		Env:             env,
		Resources:       resources,
	}
	if jobUsesDRA {
		container.Resources.Claims = []corev1.ResourceClaim{{Name: draClaimName}}
	}

	var volumes []corev1.Volume
	if spec.ShmSize != "" {
		// PyTorch's DataLoader and NCCL's shared-memory IPC both need real /dev/shm — k8s
		// defaults it to a tiny tmpfs that silently breaks both under load.
		shmQty := resource.MustParse(spec.ShmSize)
		volumes = append(volumes, corev1.Volume{
			Name: "dshm",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &shmQty,
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "dshm",
			MountPath: "/dev/shm",
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(exp.ID),
			Namespace: HypothesisLoopNamespace,
			Labels: map[string]string{
				"hypothesisloop.io/managed-by":    "hypothesisloop",
				"hypothesisloop.io/experiment-id": exp.ID,
				"hypothesisloop.io/agent-id":      sanitizeLabel(exp.AgentID),
				"hypothesisloop.io/capacity-tier": string(exp.CapacityTier),
			},
			Annotations: map[string]string{
				AcceleratorTypeAnnotation: string(exp.AcceleratorType),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"hypothesisloop.io/experiment-id": exp.ID,
					},
					Annotations: map[string]string{
						AcceleratorCountAnnotation: fmt.Sprintf("%d", spec.AcceleratorCount),
						// Also on the pod, not just the Job: ResolveAdmittedAcceleratorType
						// reads it off the scheduled pod to report what the job really landed on.
						AcceleratorTypeAnnotation: string(exp.AcceleratorType),
					},
				},
				Spec: corev1.PodSpec{
					PriorityClassName:             priorityClass,
					RestartPolicy:                 restartPolicy,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					Affinity:                      c.buildAffinity(exp.ID, spec, placement, distributed),
					Tolerations:                   acceleratorTolerations(spec.AcceleratorCount, spec.AcceleratorTolerations),
					NodeSelector:                  spec.NodeSelector,
					TerminationGracePeriodSeconds: &terminationGrace,
				},
			},
		},
	}

	if jobUsesDRA {
		claimTemplateName := resourceClaimTemplateName(job.Name)
		job.Spec.Template.Spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:                      draClaimName,
			ResourceClaimTemplateName: &claimTemplateName,
		}}
	}

	if distributed {
		// Subdomain must match the headless Service CreateWorkload creates alongside this
		// Job (same name) — that's what makes HYPOTHESISLOOP_MASTER_ADDR above resolve.
		job.Spec.Template.Spec.Subdomain = job.Name

		// Each rank gets its own retry budget: a flaky worker doesn't burn the whole job's
		// shared backoff budget, and one exhausting its retries fails only that index.
		completionMode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &completionMode
		job.Spec.Completions = &parallelism
		job.Spec.Parallelism = &parallelism
		job.Spec.BackoffLimitPerIndex = &backoff
		// No custom SuccessPolicy: every distributed job uses k8s's default Indexed Job
		// semantics — Complete only once *all* indexes succeed. A policy keyed on index 0
		// would let k8s declare success and terminate other ranks on a lone rank-0 success,
		// hiding a real failure elsewhere.
		// Exit code 137 (SIGKILL, typically OOM) is a hard per-index failure rather than
		// counted toward BackoffLimitPerIndex retries — retrying an OOM just wastes
		// accelerator-hours on a guaranteed repeat failure.
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionFailIndex,
				OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
					Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
					Values:   []int32{137},
				},
			}},
		}
	}
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, fmt.Errorf("workload: hash desired Job spec: %w", err)
	}
	hash := sha256.Sum256(specJSON)
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[DesiredSpecHashAnnotation] = hex.EncodeToString(hash[:])

	return job, nil
}
