package k8sexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/boundedname"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// AcceleratorTypeAnnotation records which accelerator type this run holds, set from
// exp.AcceleratorType and read back by ResolveAdmittedAcceleratorType to report what a pod
// actually landed on.
//
// An annotation, not a label, because an accelerator type is a driver-published "key=value"
// (see clusterapi.AcceleratorType) and Kubernetes label *values* admit neither "/" nor "=" — a Job
// carrying one as a label is rejected outright by the API server. Sanitizing it into a legal
// label would break the round-trip this value exists for: the string read back has to be
// byte-identical to the one submitted, or the admitted type no longer matches the type quota
// billed against. Annotations have no such value restrictions. Nothing selects on this, so
// nothing is lost by not being a label.
const AcceleratorTypeAnnotation = workloadkeys.AcceleratorType

// AcceleratorCountAnnotation carries the group's per-pod accelerator count onto the pod template,
// read back by ensureResourceClaimTemplate as the count one pod claims.
const AcceleratorCountAnnotation = workloadkeys.AcceleratorCount

const DesiredSpecHashAnnotation = workloadkeys.DesiredSpecHash

// GraceSecondsAnnotation carries the pod's ordinary shutdown grace onto the pod template, so a
// termination that earned no checkpoint window can be deleted with it even though the pod's own
// TerminationGracePeriodSeconds may have been widened to hold that window.
const GraceSecondsAnnotation = workloadkeys.GraceSeconds

// BuildJob deterministically compiles exp's complete JobSpec DSL into a native batch/v1 Job —
// the only place the DSL vocabulary (image, command, env, cpu/memory/accelerator, num_nodes,
// max_retries, acceptable_accelerator_types) is translated into k8s concepts (containers,
// resource requests, node affinity, Indexed completion mode). Nothing upstream ever
// constructs or reads a k8s type.
//
// Distributed jobs (NumNodes > 1) get native Indexed completion mode, rank/world-size env vars
// matching torchrun/Horovod convention, and a pod failure policy that fails the whole Job on any
// rank's non-zero exit — a gang is one unit, so it succeeds or fails as one.
//
// Pure: same desired state plus same placement always compiles to the same Job. It performs no
// cluster reads of its own — callers resolve placement once (see ResolveAcceleratorPlacement) and
// pass it in.
func (c *JobWorkloadClient) BuildJob(exp *clusterapi.DesiredJob, placement AcceleratorPlacement) (*batchv1.Job, error) {
	jobs, err := c.BuildJobs(exp, placement)
	if err != nil {
		return nil, err
	}
	if len(jobs) != 1 {
		return nil, fmt.Errorf("workload: experiment %s compiles to %d Jobs; use BuildJobs", exp.ID, len(jobs))
	}
	return jobs[0], nil
}

// BuildJobs compiles exp into the complete set of native Jobs it runs as: exactly one for an
// ungrouped job, and one Indexed Job per group for a heterogeneous one (see clusterapi.JobSpec.Groups).
// The group Jobs share one headless Service (CreateWorkload creates it before any of them), so
// every node of every group resolves every other by name and the whole set has one rendezvous.
//
// They are separate Jobs only because Kubernetes has no single object for "N pods of shape A plus
// M pods of shape B". They remain one gang: each carries the same FailJob-on-any-non-zero-exit
// policy §1 established, and PollJobStatus reports the SET's phase, so a failure anywhere
// ends the experiment and takes every group's pods with it.
func (c *JobWorkloadClient) BuildJobs(exp *clusterapi.DesiredJob, placement AcceleratorPlacement) ([]*batchv1.Job, error) {
	// EffectiveJob, never exp.Job directly: the job as admission resolved it lives in
	// exp.ResolvedJob (see clusterapi.DesiredJob.ResolvedJob), and nothing rewrites exp.Job in place.
	spec := exp.EffectiveJob()
	if spec.MaxRetries == nil || *spec.MaxRetries < 0 {
		return nil, fmt.Errorf("workload: CPU, memory, storage, and non-negative max_retries are required desired state")
	}
	if exp.Image == "" {
		return nil, fmt.Errorf("workload: experiment %s has no image in desired state", exp.ID)
	}
	if spec.TotalAccelerators() > 0 && !onCores(exp) && placement.ResourceName == "" && placement.DeviceClassName == "" {
		return nil, fmt.Errorf("workload: accelerator %q has no resolved placement", exp.AcceleratorType)
	}
	groups := spec.NodeGroups()
	for _, group := range groups {
		if group.CPU == "" || group.Memory == "" || group.Storage == "" {
			return nil, fmt.Errorf("workload: CPU, memory, storage, and non-negative max_retries are required desired state")
		}
	}
	// One rendezvous for the whole job: node 0 of the FIRST group, named through the shared
	// Service. Every group is told the same address, so groups meet each other exactly the way
	// ranks of a single Indexed Job already do.
	plans := make([]groupPlan, 0, len(groups))
	rankOffset := 0
	for _, group := range groups {
		ranks := 0
		if spec.Nodes() > 1 {
			ranks = group.Replicas
		}
		plans = append(plans, groupPlan{
			group:      group,
			grouped:    len(spec.Groups) > 0,
			rankOffset: rankOffset,
			totalNodes: spec.Nodes(),
			jobName:    groupJobName(exp.ID, group.Name, ranks),
			service:    jobName(exp.ID),
		})
		rankOffset += group.Replicas
	}
	masterAddr := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", plans[0].jobName, plans[0].service, HypothesisLoopNamespace)

	jobs := make([]*batchv1.Job, 0, len(plans))
	for _, plan := range plans {
		plan.masterAddr = masterAddr
		job, err := c.compileJob(exp, placement, plan)
		if err != nil {
			return nil, err
		}
		// The identity a drift check compares is the control-plane's desired state, never this
		// runtime's translation of it into local inventory. Placement is resolved from live
		// DeviceClasses and node labels, so hashing it made a DeviceClass rename or a driver upgrade
		// re-hash every running job of that type and drift-delete it mid-training — the cluster
		// changing around a job is not the control plane asking for a different job.
		identity, err := c.compileJob(exp, AcceleratorPlacement{}, plan)
		if err != nil {
			return nil, err
		}
		specJSON, err := json.Marshal(identity.Spec)
		if err != nil {
			return nil, fmt.Errorf("workload: hash desired Job spec: %w", err)
		}
		hash := sha256.Sum256(specJSON)
		job.Annotations[DesiredSpecHashAnnotation] = hex.EncodeToString(hash[:])
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// groupPlan is everything one group needs to know about the job it belongs to. An ungrouped job
// has exactly one plan whose group carries the top-level shape and whose name is empty, which is
// what keeps its compiled Job byte-identical to the one this code produced before groups existed.
type groupPlan struct {
	group      clusterapi.JobGroup
	grouped    bool
	rankOffset int
	totalNodes int
	jobName    string
	service    string
	masterAddr string
}

// compileJob assembles the Job. Split from BuildJob so the desired-spec hash can be taken over the
// same assembly with placement left out, without enumerating (and later forgetting) which fields
// placement reaches.
func (c *JobWorkloadClient) compileJob(exp *clusterapi.DesiredJob, placement AcceleratorPlacement, plan groupPlan) (*batchv1.Job, error) {
	spec := exp.EffectiveJob()
	group := plan.group

	// Parallelism is this GROUP's replica count; distributed is a property of the whole job. A
	// single-replica group inside a multi-node job is still Indexed and still gets a rendezvous:
	// what makes a pod part of a gang is the job it belongs to, not how many siblings it has.
	parallelism := int32(group.Replicas)
	distributed := plan.totalNodes > 1

	terminationGrace, checkpointGrace, err := spec.ResolvedGrace()
	if err != nil {
		return nil, fmt.Errorf("workload: experiment %s: %w", exp.ID, err)
	}

	// The pod's grace is the longer of its ordinary shutdown grace and the checkpoint window it
	// declared, because the window has to be a property of the pod for the kubelet to honour it:
	// when the Job is deleted it is Kubernetes' garbage collector that deletes these pods, and
	// it uses whatever the pod itself declares.
	//
	// Which terminations actually get to spend that window is not decided here and is not a rule
	// the runtime is handed: DeleteWorkload shortens the pod back to graceSeconds below whenever
	// the control plane did not grant this termination its window.
	podGrace := max(terminationGrace, checkpointGrace) + workloadenv.LaunchKillMargin

	priorityClass := PriorityClassGuaranteed
	if exp.CapacityTier == clusterapi.CapacityBurst {
		priorityClass = PriorityClassBurst
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	for name, dimension := range map[corev1.ResourceName]struct{ field, qty string }{
		corev1.ResourceCPU:              {"cpu", group.CPU},
		corev1.ResourceMemory:           {"memory", group.Memory},
		corev1.ResourceEphemeralStorage: {"storage", group.Storage},
	} {
		q, err := resource.ParseQuantity(dimension.qty)
		if err != nil {
			return nil, fmt.Errorf("workload: job.%s %q: %w", dimension.field, dimension.qty, err)
		}
		resources.Requests[name] = q
		resources.Limits[name] = q
	}
	// jobUsesDRA tracks whether this job needs a ResourceClaimTemplate/PodResourceClaim instead
	// of a plain extended-resource request. CreateWorkload reads this back off the returned
	// Job (non-empty Spec.ResourceClaims) to know whether to also create the ResourceClaimTemplate.
	jobUsesDRA := group.AcceleratorCount > 0 && placement.UsesDRA()
	if group.AcceleratorCount > 0 && !jobUsesDRA && !onCores(exp) {
		acceleratorQty := *resource.NewQuantity(int64(group.AcceleratorCount), resource.DecimalSI)
		acceleratorResource := corev1.ResourceName(placement.ResourceName)
		resources.Requests[acceleratorResource] = acceleratorQty
		resources.Limits[acceleratorResource] = acceleratorQty
	}
	for name, qty := range exp.PodResources {
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return nil, fmt.Errorf("workload: pod_resources[%q]: %w", name, err)
		}
		resources.Requests[corev1.ResourceName(name)] = q
		resources.Limits[corev1.ResourceName(name)] = q
	}

	vars, err := workloadenv.Vars(exp, spec, group, c.apiURL, func(url string) string { return url })
	if err != nil {
		return nil, fmt.Errorf("workload: %w", err)
	}
	env := make([]corev1.EnvVar, 0, len(vars))
	for _, v := range vars {
		env = append(env, corev1.EnvVar{Name: v.Name, Value: v.Value})
	}
	if distributed {
		// HYPOTHESISLOOP_MASTER_ADDR is node 0 of the job's FIRST group — its stable DNS name,
		// resolvable because every group's pods take the shared Service as their Subdomain and
		// CreateWorkload creates that Service before any Job, so k8s's Indexed Job hostname
		// convention applies ($job-name-$index.$subdomain.$namespace.svc.cluster.local). This is
		// the rendezvous endpoint training frameworks need: PyTorch DDP (torchrun
		// --master-addr/--master-port), Ray (`ray start --head` on node 0, `ray start
		// --address=...` on the rest), etc. One address for the whole job, groups included: a
		// learner and its actors meet at exactly the same place two ranks of one Job would.
		//
		// A pod's index within its group is its completion index, which only the downward API
		// hands it: every pod of a group shares one template. Kubernetes cannot add the group's
		// offset to it either, which is why a grouped pod is told the two terms apart.
		rankFieldRef := &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
			},
		}
		for _, v := range workloadenv.Gang(spec, group, plan.rankOffset, 0, plan.masterAddr) {
			switch v.Name {
			case jobenv.Rank, jobenv.TorchRank, jobenv.GroupRank:
				env = append(env, corev1.EnvVar{Name: v.Name, ValueFrom: rankFieldRef})
			default:
				env = append(env, corev1.EnvVar{Name: v.Name, Value: v.Value})
			}
		}
	}

	container := corev1.Container{
		Name:  "experiment",
		Image: exp.Image,
		// IfNotPresent rather than k8s's default (Always for :latest-tagged images): otherwise
		// a locally-built image is ignored and the kubelet always attempts a registry pull,
		// which fails on any cluster without a real registry behind the image reference.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         workloadenv.Command(exp, spec, group),
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
		shmQty, err := resource.ParseQuantity(spec.ShmSize)
		if err != nil {
			return nil, fmt.Errorf("workload: shm_size %q: %w", spec.ShmSize, err)
		}
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
			Name:      plan.jobName,
			Namespace: HypothesisLoopNamespace,
			Labels: map[string]string{
				workloadkeys.ManagedBy:    workloadkeys.ManagedByValue,
				workloadkeys.ExperimentID: exp.ID,
				workloadkeys.AgentID:      sanitizeLabel(exp.AgentID),
				workloadkeys.CapacityTier: string(exp.CapacityTier),
				workloadkeys.Attempt:      fmt.Sprintf("%d", exp.AttemptCount),
			},
			Annotations: map[string]string{
				AcceleratorTypeAnnotation: string(exp.AcceleratorType),
			},
		},
		Spec: batchv1.JobSpec{
			// The control plane is the one retry authority (clusterapi.JobSpec.MaxRetries): a pod
			// is never recreated and a container never restarted in place, and a failure surfaces
			// as a failed pod, which is what the gang's PodFailurePolicy acts on.
			BackoffLimit: new(int32),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						workloadkeys.ManagedBy:    workloadkeys.ManagedByValue,
						workloadkeys.ExperimentID: exp.ID,
						// Also on the pod, not just the Job: PollPhaseDetail lists PODS filtered
						// by experiment-id+attempt to fence scheduledNodes to this generation
						// (see its doc comment) — without this, that selector matched zero pods
						// every time, permanently reading scheduledNodes=0 for a genuinely
						// running, progressing pod and evicting it as stuck_pending once the
						// deadline passed regardless of real health.
						workloadkeys.Attempt: fmt.Sprintf("%d", exp.AttemptCount),
					},
					Annotations: map[string]string{
						AcceleratorCountAnnotation: fmt.Sprintf("%d", group.AcceleratorCount),
						// Also on the pod, not just the Job: ResolveAdmittedAcceleratorType
						// reads it off the scheduled pod to report what the job really landed on.
						AcceleratorTypeAnnotation: string(exp.AcceleratorType),
						// The ordinary shutdown grace, recorded because the pod's own
						// TerminationGracePeriodSeconds may have been widened to hold a
						// checkpoint window. A termination that earned no window is deleted
						// with this instead — see DeleteWorkload.
						GraceSecondsAnnotation: fmt.Sprintf("%d", terminationGrace),
					},
				},
				Spec: corev1.PodSpec{
					PriorityClassName:             priorityClass,
					AutomountServiceAccountToken:  new(bool),
					RestartPolicy:                 corev1.RestartPolicyNever,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					Affinity:                      c.buildAffinity(exp.ID, spec, placement, distributed),
					Tolerations:                   acceleratorTolerations(group.AcceleratorCount, placement),
					NodeSelector:                  spec.NodeSelector,
					TerminationGracePeriodSeconds: &podGrace,
				},
			},
		},
	}

	if plan.grouped {
		// On the Job only, never on the pod template: the headless Service selects this
		// experiment's pods by experiment id alone, and a group label down there would split one
		// rendezvous into one Service's worth of pods per group.
		job.Labels[workloadkeys.JobGroup] = boundedname.Name(group.Name, group.Name, dnsLabelLength)
	}

	if exp.ImagePull != nil {
		// The image is read only with the login CreateWorkload writes here.
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName(exp.ID)}}
	}

	if jobUsesDRA {
		claimTemplateName := resourceClaimTemplateName(job.Name)
		job.Spec.Template.Spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:                      draClaimName,
			ResourceClaimTemplateName: &claimTemplateName,
		}}
	}

	if distributed {
		// Subdomain must match the headless Service CreateWorkload creates for this experiment —
		// that's what makes HYPOTHESISLOOP_MASTER_ADDR above resolve. One Service for every group,
		// so a pod of one group resolves a pod of another by exactly the same name it uses for a
		// sibling.
		job.Spec.Template.Spec.Subdomain = plan.service

		completionMode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &completionMode
		job.Spec.Completions = &parallelism
		job.Spec.Parallelism = &parallelism

		// A gang is one unit, so any rank exiting non-zero fails the whole Job and k8s tears
		// the survivors down with it.
		//
		// This replaced BackoffLimitPerIndex, whose stated intent — a flaky worker not burning
		// the shared backoff budget — is right for embarrassingly parallel work and wrong for
		// every synchronous one. When rank 3 died mid-run, ranks 0-2 sat blocked in a collective
		// until the NCCL or gloo timeout holding their accelerators the whole time, while the
		// replacement rank 3 started a fresh process and rejoined a rendezvous the others had
		// already left. The gang burned its full allocation and then failed anyway — the exact
		// cost the per-index budget was meant to avoid.
		//
		// No custom SuccessPolicy: every distributed job uses k8s's default Indexed Job
		// semantics — Complete only once *all* indexes succeed. A policy keyed on index 0
		// would let k8s declare success and terminate other ranks on a lone rank-0 success,
		// hiding a real failure elsewhere.
		//
		// Exit code 137 (SIGKILL, typically OOM) needs no rule of its own: it is non-zero, so
		// the rule below already fails the job outright rather than retrying it, which is what
		// the old dedicated 137 rule existed to achieve at index scope.
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionFailJob,
				OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
					Operator: batchv1.PodFailurePolicyOnExitCodesOpNotIn,
					Values:   []int32{0},
				},
			}},
		}
	}
	return job, nil
}
