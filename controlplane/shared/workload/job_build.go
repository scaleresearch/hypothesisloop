package workload

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// AcceleratorTypeLabel tracks which accelerator type/tier this run is billed against — set by the control
// plane from exp.AcceleratorType, purely for observability (kubectl get jobs -l ...); it is never
// read back to derive accounting, since exp.AcceleratorType/exp.AcceleratorCount are already known from the
// agent's JobSpec at submission time.
const AcceleratorTypeLabel = "openresearch.io/accelerator-type"

// AttemptLabel carries exp.Attempt (see domain.Experiment.Attempt) — set on every Job so a
// recreation after the previous Job disappeared is observable (kubectl get jobs -l ...) and,
// if a status report ever needs to correlate back to a specific attempt, distinguishable from
// the attempt before it, even though both share the same Job name.
const AttemptLabel = "openresearch.dev/attempt"

// BuildJob compiles exp's JobSpec DSL (merged with this cluster's JobDefaults) down into a
// native batch/v1 Job — the only place the platform's DSL vocabulary (image, command, env,
// cpu/memory/accelerator, num_nodes, max_retries, acceptable_accelerator_types) is translated into
// execution-engine concepts (containers, resource requests, node affinity, Indexed
// completion mode). Nothing upstream of this function ever constructs or reads a k8s type.
//
// Distributed jobs (JobSpec.NumNodes > 1) get native Indexed completion mode, per-index
// retry, an OOM-aware pod failure policy, and rank/world-size env vars, matching torchrun/
// Horovod's RANK/WORLD_SIZE convention.
func (c *JobWorkloadClient) BuildJob(ctx context.Context, exp *domain.Experiment) (*batchv1.Job, error) {
	spec := mergeJobDefaults(exp.Job, c.jobDefaults(ctx))

	nodes := spec.NumNodes
	if nodes < 1 {
		nodes = 1
	}
	parallelism := int32(nodes)
	distributed := parallelism > 1

	backoff := c.jobBackoffLimit
	if spec.MaxRetries != nil {
		backoff = int32(*spec.MaxRetries)
	}

	deadline := int64(exp.EstimatedDurationHours * c.jobDeadlineMultiplier * 3600)
	if deadline < c.minJobDeadlineSeconds {
		deadline = c.minJobDeadlineSeconds
	}

	attempt := exp.Attempt
	if attempt < 1 {
		attempt = 1
	}

	priorityClass := PriorityClassGuaranteed
	if exp.CapacityTier == domain.CapacityBurst {
		priorityClass = PriorityClassBurst
	}

	restartPolicy := corev1.RestartPolicyOnFailure
	if distributed {
		// PodFailurePolicy (below) requires RestartPolicy=Never (k8s API validation):
		// failed containers must surface as failed pods so the Job controller's
		// per-index failure accounting sees them, rather than being retried in-place by
		// the kubelet.
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
	if spec.AcceleratorCount > 0 {
		acceleratorQty := resource.MustParse(fmt.Sprintf("%d", spec.AcceleratorCount))
		acceleratorResource := corev1.ResourceName(c.resourceNameFor(spec.AcceleratorType))
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

	env := []corev1.EnvVar{
		{Name: "OPENRESEARCH_EXPERIMENT_ID", Value: exp.ID},
		{Name: "OPENRESEARCH_AGENT_ID", Value: exp.AgentID},
		{Name: "OPENRESEARCH_PROJECT_ID", Value: exp.ProjectID},
		{Name: "OPENRESEARCH_CODE_REF", Value: exp.CodeRef},
		{Name: "OPENRESEARCH_CONFIG_HASH", Value: exp.ConfigHash},
		{Name: "OPENRESEARCH_DATA_REF", Value: exp.DataRef},
		{Name: "OPENRESEARCH_REGISTRY_URL", Value: c.registryURL},
		{Name: "OPENRESEARCH_ACCELERATOR_TYPE", Value: string(exp.AcceleratorType)},
		{Name: "OPENRESEARCH_ACCELERATOR_COUNT", Value: fmt.Sprintf("%d", exp.AcceleratorCount)},
		{Name: "OPENRESEARCH_DURATION_SECONDS", Value: fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		{Name: "TRACEPARENT", Value: traceparentFromID(exp.ID)},
	}
	for k, v := range spec.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	if distributed {
		// OPENRESEARCH_MASTER_ADDR is rank 0's stable DNS name — resolvable because BuildJob
		// sets pod.Spec.Subdomain to the job's own name and CreateWorkload creates a matching
		// headless Service, so k8s's Indexed Job hostname convention
		// ($job-name-$index.$subdomain.$namespace.svc.cluster.local) applies. This is the
		// rendezvous endpoint distributed training/orchestration frameworks need: PyTorch DDP
		// (torchrun --master-addr/--master-port), Ray (`ray start --head` on rank 0,
		// `ray start --address=$OPENRESEARCH_MASTER_ADDR:$OPENRESEARCH_MASTER_PORT` on the
		// rest), or any other rendezvous scheme the training code implements.
		masterAddr := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", jobName(exp.ID), jobName(exp.ID), OpenResearchNamespace)
		rankFieldRef := &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
			},
		}
		worldSize := fmt.Sprintf("%d", parallelism)
		masterPort := fmt.Sprintf("%d", DistributedMasterPort)
		env = append(env,
			corev1.EnvVar{Name: "OPENRESEARCH_RANK", ValueFrom: rankFieldRef},
			corev1.EnvVar{Name: "OPENRESEARCH_WORLD_SIZE", Value: worldSize},
			corev1.EnvVar{Name: "OPENRESEARCH_MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "OPENRESEARCH_MASTER_PORT", Value: masterPort},
			// Standard unprefixed names PyTorch's torch.distributed.init_process_group(env://)
			// and torchrun read directly, alongside the OPENRESEARCH_-prefixed vars above (kept
			// for anything already depending on them). One process per pod, so LOCAL_RANK is
			// always 0.
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
		// IfNotPresent rather than k8s's own default (Always for :latest-tagged images):
		// without this, a locally-imported/locally-built image is ignored and the kubelet
		// always attempts a registry pull, which fails hard on any cluster (dev or air-gapped
		// production) that doesn't have a real registry behind the image reference.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         spec.Command,
		Args:            spec.Args,
		Env:             env,
		Resources:       resources,
	}

	var volumes []corev1.Volume
	if spec.ShmSize != "" {
		// PyTorch's multiprocess DataLoader and NCCL's shared-memory IPC both need real
		// /dev/shm — k8s defaults it to a tiny tmpfs that silently breaks both under load.
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
			Namespace: OpenResearchNamespace,
			Labels: map[string]string{
				"openresearch.io/managed-by":    "openresearch",
				"openresearch.io/experiment-id": exp.ID,
				"openresearch.io/agent-id":      sanitizeLabel(exp.AgentID),
				"openresearch.io/capacity-tier": string(exp.CapacityTier),
				AcceleratorTypeLabel:                    string(exp.AcceleratorType),
				AttemptLabel:                    fmt.Sprintf("%d", attempt),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"openresearch.io/experiment-id": exp.ID},
				},
				Spec: corev1.PodSpec{
					PriorityClassName: priorityClass,
					RestartPolicy:     restartPolicy,
					Containers:        []corev1.Container{container},
					Volumes:           volumes,
					Affinity:          c.buildAffinity(exp.ID, spec, distributed),
					Tolerations:       acceleratorTolerations(spec.AcceleratorCount, c.taintKeyFor(spec.AcceleratorType)),
				},
			},
		},
	}

	if distributed {
		// Subdomain must match the headless Service CreateWorkload creates alongside this
		// Job (same name) — that's what makes the OPENRESEARCH_MASTER_ADDR DNS name above
		// actually resolve; see the Indexed Job hostname convention this relies on.
		job.Spec.Template.Spec.Subdomain = job.Name

		// Each rank gets its own retry budget: a flaky worker doesn't burn through the
		// whole job's shared backoff budget, and a worker that exhausts its own retries
		// fails just that index rather than nuking every other rank's progress.
		completionMode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &completionMode
		job.Spec.Completions = &parallelism
		job.Spec.Parallelism = &parallelism
		job.Spec.BackoffLimitPerIndex = &backoff
		// No custom SuccessPolicy: the DSL has no coordinator-only completion workload type,
		// so every distributed job uses k8s's default Indexed Job semantics — the Job is
		// Complete only once *all* indexes succeed. A custom policy keyed on index 0 would let
		// k8s declare success (and terminate remaining ranks) on a lone rank-0 success,
		// hiding a real failure on any other rank.
		// Exit code 137 (SIGKILL, typically OOM) is a hard per-index failure rather than
		// counted toward BackoffLimitPerIndex retries — retrying an OOM with the same
		// resources just wastes accelerator-hours on a guaranteed repeat failure.
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

	return job, nil
}
