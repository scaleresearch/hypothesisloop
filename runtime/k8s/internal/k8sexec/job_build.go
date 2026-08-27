package k8sexec

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
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
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
const AcceleratorTypeAnnotation = workloadkeys.AcceleratorType

// AcceleratorCountAnnotation carries spec.AcceleratorCount onto the pod template — read back by
// GetLiveAcceleratorCapacitySnapshot's DRA branch, which (unlike the classic-mode branch) has no
// extended-resource request on the pod to sum directly.
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
func (c *JobWorkloadClient) BuildJob(exp *domain.Experiment, placement AcceleratorPlacement) (*batchv1.Job, error) {
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
// ungrouped job, and one Indexed Job per group for a heterogeneous one (see domain.JobSpec.Groups).
// The group Jobs share one headless Service (CreateWorkload creates it before any of them), so
// every node of every group resolves every other by name and the whole set has one rendezvous.
//
// They are separate Jobs only because Kubernetes has no single object for "N pods of shape A plus
// M pods of shape B". They remain one gang: each carries the same FailJob-on-any-non-zero-exit
// policy §1 established, and PollJobPhaseAndUID reports the SET's phase, so a failure anywhere
// ends the experiment and takes every group's pods with it.
func (c *JobWorkloadClient) BuildJobs(exp *domain.Experiment, placement AcceleratorPlacement) ([]*batchv1.Job, error) {
	// EffectiveJob, never exp.Job directly: an admitted experiment's literal CPU/memory/storage
	// live in exp.ResolvedJob (see domain.Experiment.ResolvedJob), because exp.Job may still carry
	// the "max" sentinel the agent submitted — GET must keep returning exactly what was
	// submitted, so nothing ever rewrites exp.Job in place.
	spec := exp.EffectiveJob()
	if spec.MaxRetries == nil || *spec.MaxRetries < 0 {
		return nil, fmt.Errorf("workload: CPU, memory, storage, and non-negative max_retries are required desired state")
	}
	if spec.TotalAccelerators() > 0 && placement.ResourceName == "" && placement.DeviceClassName == "" {
		return nil, fmt.Errorf("workload: accelerator %q has no resolved placement", exp.AcceleratorType)
	}
	groups := spec.NodeGroups()
	for _, group := range groups {
		if group.CPU == "" || group.Memory == "" || group.Storage == "" {
			return nil, fmt.Errorf("workload: CPU, memory, storage, and non-negative max_retries are required desired state")
		}
		// Defense-in-depth: an admitted experiment must never reach BuildJobs with "max" still
		// unresolved — the scheduler tick resolves it before admission ever writes SUBMITTED (see
		// scheduler.Loop.resolveClusterLocalResources). Reaching here with a literal "max" is a
		// control-plane bug, not a runtime concern, and must fail loudly rather than compile a Job
		// that requests a "max" quantity string k8s would reject with an opaque parse error.
		if group.CPU == domain.MaxResourceSentinel || group.Memory == domain.MaxResourceSentinel || group.Storage == domain.MaxResourceSentinel {
			return nil, fmt.Errorf("workload: experiment %s reached BuildJobs with an unresolved %q sentinel still in its job spec — this is a control-plane bug, not a placement failure", exp.ID, domain.MaxResourceSentinel)
		}
	}
	// One rendezvous for the whole job: node 0 of the FIRST group, named through the shared
	// Service. Every group is told the same address, so groups meet each other exactly the way
	// ranks of a single Indexed Job already do.
	plans := make([]groupPlan, 0, len(groups))
	rankOffset := 0
	for _, group := range groups {
		plans = append(plans, groupPlan{
			group:      group,
			grouped:    len(spec.Groups) > 0,
			rankOffset: rankOffset,
			totalNodes: spec.Nodes(),
			jobName:    groupJobName(exp.ID, group.Name),
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
		// The same rule applies to the durable-data session: the control plane mints a fresh one on
		// every reconcile pass, so hashing it would re-hash every running job every few seconds and
		// drift-delete it mid-training. The addressing it carries is desired state and stays hashed;
		// only the rotating credentials are dropped.
		identityExp := *exp
		identityExp.Data = exp.Data.WithoutCredentials()
		identity, err := c.compileJob(&identityExp, AcceleratorPlacement{}, plan)
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
	group      domain.JobGroup
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
func (c *JobWorkloadClient) compileJob(exp *domain.Experiment, placement AcceleratorPlacement, plan groupPlan) (*batchv1.Job, error) {
	spec := exp.EffectiveJob()
	group := plan.group

	// Parallelism is this GROUP's replica count; distributed is a property of the whole job. A
	// single-replica group inside a multi-node job is still Indexed and still gets a rendezvous:
	// what makes a pod part of a gang is the job it belongs to, not how many siblings it has.
	parallelism := int32(group.Replicas)
	distributed := plan.totalNodes > 1

	backoff := int32(*spec.MaxRetries)

	terminationGrace := c.defaultTerminationGracePeriodSeconds
	if spec.TerminationGracePeriodSeconds != nil {
		terminationGrace = *spec.TerminationGracePeriodSeconds
	}
	if terminationGrace > c.maxTerminationGracePeriodSeconds {
		terminationGrace = c.maxTerminationGracePeriodSeconds
	}

	// The pod's grace is the longer of its ordinary shutdown grace and the checkpoint window it
	// declared, because the window has to be a property of the pod for the kubelet to honour it:
	// when the Job is deleted it is Kubernetes' garbage collector that deletes these pods, and
	// it uses whatever the pod itself declares. Capped separately from the shutdown grace —
	// 30 seconds does not checkpoint a serious model, and minutes is not a sane default for
	// every other kill.
	//
	// Which terminations actually get to spend that window is not decided here and is not a rule
	// the runtime is handed: DeleteWorkload shortens the pod back to graceSeconds below whenever
	// the control plane did not grant this termination its window.
	podGrace := terminationGrace
	if spec.CheckpointGraceSeconds != nil {
		checkpointGrace := *spec.CheckpointGraceSeconds
		if checkpointGrace > c.maxCheckpointGraceSeconds {
			checkpointGrace = c.maxCheckpointGraceSeconds
		}
		if checkpointGrace > podGrace {
			podGrace = checkpointGrace
		}
	}

	priorityClass := PriorityClassGuaranteed
	if exp.CapacityTier == domain.CapacityBurst {
		priorityClass = PriorityClassBurst
	}

	restartPolicy := corev1.RestartPolicyOnFailure
	if distributed {
		// PodFailurePolicy (below) requires RestartPolicy=Never, and the requirement is not
		// incidental: the policy acts on failed *pods*, so a container the kubelet restarts in
		// place never surfaces as one and a dead rank would go unnoticed by the rule meant to
		// tear the gang down.
		restartPolicy = corev1.RestartPolicyNever
	} else if spec.MaxRetries != nil && *spec.MaxRetries == 0 {
		// Retries happen at two independent layers: BackoffLimit recreates the pod, and
		// RestartPolicy=OnFailure has the kubelet restart the container in place. max_retries
		// only reaches the first, so under OnFailure a job asking for zero retries still got
		// restarted — observed live as restart_count=1 with max_retries=0, on a diagnostic run
		// chosen specifically to fail once and cheaply. Honour the request: with no retries
		// asked for there is no reason to keep the in-place layer, and Never makes the failure
		// surface as a failed pod instead.
		restartPolicy = corev1.RestartPolicyNever
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	// A "max" sentinel is always resolved to a plain literal once an experiment is admitted (see
	// scheduler.Loop.resolveClusterLocalResources; EffectiveJob above reads exp.ResolvedJob), so it should
	// never reach here — but resource.MustParse("max") panics with a cryptic k8s parser error
	// rather than naming the actual problem, so guard explicitly and fail clearly instead.
	for name, qty := range map[string]string{"cpu": group.CPU, "memory": group.Memory, "storage": group.Storage} {
		if qty == domain.MaxResourceSentinel {
			return nil, fmt.Errorf("workload: job.%s is still %q — it should have been resolved to a literal quantity at submission", name, domain.MaxResourceSentinel)
		}
	}
	if group.CPU != "" {
		resources.Requests[corev1.ResourceCPU] = resource.MustParse(group.CPU)
		resources.Limits[corev1.ResourceCPU] = resource.MustParse(group.CPU)
	}
	if group.Memory != "" {
		resources.Requests[corev1.ResourceMemory] = resource.MustParse(group.Memory)
		resources.Limits[corev1.ResourceMemory] = resource.MustParse(group.Memory)
	}
	if group.Storage != "" {
		resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(group.Storage)
		resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(group.Storage)
	}
	// jobUsesDRA tracks whether this job needs a ResourceClaimTemplate/PodResourceClaim instead
	// of a plain extended-resource request. CreateWorkload reads this back off the returned
	// Job (non-empty Spec.ResourceClaims) to know whether to also create the ResourceClaimTemplate.
	jobUsesDRA := group.AcceleratorCount > 0 && placement.UsesDRA()
	if group.AcceleratorCount > 0 && !jobUsesDRA {
		acceleratorQty := resource.MustParse(fmt.Sprintf("%d", group.AcceleratorCount))
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

	// Durable data is desired state, not a cluster-side setting: the control plane computes the
	// address and hands it over with the credentials, so a job's checkpoint prefix is identical
	// whichever cluster admission placed it on. Absent means the control plane is misconfigured,
	// and a job that silently runs without a place to save is worse than one that never starts.
	if exp.Data == nil {
		return nil, fmt.Errorf("workload: experiment %s has no durable-data access in desired state", exp.ID)
	}
	env := []corev1.EnvVar{
		{Name: "AWS_ACCESS_KEY_ID", Value: exp.Data.AccessKeyID},
		{Name: "AWS_ENDPOINT_URL", Value: exp.Data.Endpoint},
		{Name: "AWS_ENDPOINT_URL_S3", Value: exp.Data.Endpoint},
		{Name: "AWS_REGION", Value: exp.Data.Region},
		{Name: "AWS_SECRET_ACCESS_KEY", Value: exp.Data.SecretAccessKey},
		{Name: "AWS_SESSION_TOKEN", Value: exp.Data.SessionToken},
		{Name: "HYPOTHESISLOOP_DATA_SHARED", Value: exp.Data.Shared},
		{Name: "HYPOTHESISLOOP_DATA_URI", Value: exp.Data.URI},
		{Name: "HYPOTHESISLOOP_EXPERIMENT_ID", Value: exp.ID},
		{Name: "HYPOTHESISLOOP_AGENT_ID", Value: exp.AgentID},
		{Name: "HYPOTHESISLOOP_PROJECT_ID", Value: exp.ProjectID},
		{Name: "HYPOTHESISLOOP_CODE_REF", Value: exp.CodeRef},
		{Name: "HYPOTHESISLOOP_CONFIG_HASH", Value: exp.ConfigHash},
		{Name: "HYPOTHESISLOOP_DATA_REF", Value: exp.DataRef},
		{Name: "HYPOTHESISLOOP_API_URL", Value: c.apiURL},
		{Name: "HYPOTHESISLOOP_ACCELERATOR_TYPE", Value: string(exp.AcceleratorType)},
		// spec.AcceleratorCount (per node), not exp.AcceleratorCount (the job total across
		// nodes). A pod only ever holds the per-node count, so at accelerator_count 4 /
		// num_nodes 2 the total told every pod it had 8 devices, and anything sizing
		// --nproc_per_node from it launched four times the processes it had hardware for.
		// The total stays on the experiment, where billing and admission read it.
		{Name: "HYPOTHESISLOOP_ACCELERATOR_COUNT", Value: fmt.Sprintf("%d", group.AcceleratorCount)},
		{Name: "HYPOTHESISLOOP_DURATION_SECONDS", Value: fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		// Which attempt this is, 0 on the first. Always 0 for a single-pod job, whose retries are
		// the runtime's own and never re-enter this path.
		//
		// This is load-bearing beyond telling the workload, and the reason is worth stating: a
		// gang retry requeues the same experiment id, so the rebuilt Job has the same name as the
		// terminal one still sitting in the cluster. Without the attempt in desired state the
		// spec hash is byte-identical, the reconciler reads the FAILED Job as matching desired
		// state, and nothing is ever recreated — the watcher then re-observes that same dead Job
		// and spends the whole retry budget without running a single new attempt. Carrying the
		// attempt here makes each one a genuinely different desired state, so the existing
		// drift-repair path deletes and recreates the Job with no new mechanism.
		{Name: "HYPOTHESISLOOP_ATTEMPT", Value: fmt.Sprintf("%d", exp.AttemptCount)},
		{Name: "TRACEPARENT", Value: traceparentFromID(exp.ID)},
	}
	// The job's shared environment, with this group's own entries layered over it: the job-level
	// map says what is true of every node, the group's says what is true of its role.
	merged := make(map[string]string, len(spec.Env)+len(group.Env))
	for k, v := range spec.Env {
		merged[k] = v
	}
	for k, v := range group.Env {
		merged[k] = v
	}
	envNames := make([]string, 0, len(merged))
	for name := range merged {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)
	for _, k := range envNames {
		env = append(env, corev1.EnvVar{Name: k, Value: merged[k]})
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
		masterAddr := plan.masterAddr
		rankFieldRef := &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
			},
		}
		worldSize := fmt.Sprintf("%d", plan.totalNodes)
		masterPort := fmt.Sprintf("%d", DistributedMasterPort)
		env = append(env,
			corev1.EnvVar{Name: "HYPOTHESISLOOP_WORLD_SIZE", Value: worldSize},
			corev1.EnvVar{Name: "HYPOTHESISLOOP_MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "HYPOTHESISLOOP_MASTER_PORT", Value: masterPort},
			// Standard unprefixed names torch.distributed.init_process_group(env://) and
			// torchrun read directly, alongside the HYPOTHESISLOOP_-prefixed vars above. One
			// process per pod, so LOCAL_RANK is always 0.
			corev1.EnvVar{Name: "LOCAL_RANK", Value: "0"},
			corev1.EnvVar{Name: "WORLD_SIZE", Value: worldSize},
			corev1.EnvVar{Name: "MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "MASTER_PORT", Value: masterPort},
		)
		if plan.grouped {
			// A grouped job's global rank is this group's offset plus the pod's index within the
			// group, and Kubernetes cannot express that sum: the downward API can hand a pod its
			// completion index, and env expansion concatenates strings, but nothing adds. So the
			// two terms are published separately and the workload adds them
			// (RANK=$((HYPOTHESISLOOP_RANK_OFFSET + HYPOTHESISLOOP_GROUP_RANK))).
			//
			// RANK/HYPOTHESISLOOP_RANK are deliberately NOT set here. Setting them to the
			// group-local index would be silently wrong for every group after the first — two
			// pods claiming rank 0 in one process group is a hang or a corrupt all-reduce, not an
			// error anyone sees — and an absent variable fails loudly at the workload's first read.
			env = append(env,
				corev1.EnvVar{Name: "HYPOTHESISLOOP_GROUP", Value: group.Name},
				corev1.EnvVar{Name: "HYPOTHESISLOOP_GROUP_RANK", ValueFrom: rankFieldRef},
				corev1.EnvVar{Name: "HYPOTHESISLOOP_GROUP_REPLICAS", Value: fmt.Sprintf("%d", group.Replicas)},
				corev1.EnvVar{Name: "HYPOTHESISLOOP_RANK_OFFSET", Value: fmt.Sprintf("%d", plan.rankOffset)},
			)
		} else {
			env = append(env,
				corev1.EnvVar{Name: "HYPOTHESISLOOP_RANK", ValueFrom: rankFieldRef},
				corev1.EnvVar{Name: "RANK", ValueFrom: rankFieldRef},
			)
		}
	}

	// The group's own process when it names one, the job's shared entrypoint otherwise. An
	// ungrouped job's synthetic group carries the job-level values, so this is spec.Command/Args
	// exactly as before.
	command, args := group.Command, group.Args
	if len(command) == 0 {
		command, args = spec.Command, spec.Args
	}
	container := corev1.Container{
		Name:  "experiment",
		Image: spec.Image,
		// IfNotPresent rather than k8s's default (Always for :latest-tagged images): otherwise
		// a locally-built image is ignored and the kubelet always attempts a registry pull,
		// which fails on any cluster without a real registry behind the image reference.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         command,
		Args:            args,
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
	// HostMounts (JobSpec.HostMounts): a static, already-populated directory bind-mounted
	// read-only from whichever node the pod lands on — the k8s side of the same feature
	// runtime/bare-metal/internal/podexec implements via a plain bind mount. HostPathType
	// Directory (not DirectoryOrCreate) makes the kubelet itself fail the pod if the path isn't
	// actually there, matching the bare-metal backend's fail-fast-at-admission behavior rather
	// than silently creating an empty directory and running without the data.
	containerPaths := make([]string, 0, len(spec.HostMounts))
	for containerPath := range spec.HostMounts {
		containerPaths = append(containerPaths, containerPath)
	}
	sort.Strings(containerPaths)
	hostPathType := corev1.HostPathDirectory
	for i, containerPath := range containerPaths {
		volName := fmt.Sprintf("host-mount-%d", i)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: spec.HostMounts[containerPath],
					Type: &hostPathType,
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: containerPath,
			ReadOnly:  true,
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
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
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
					RestartPolicy:                 restartPolicy,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					Affinity:                      c.buildAffinity(exp.ID, spec, placement, distributed),
					Tolerations:                   acceleratorTolerations(group.AcceleratorCount, spec.AcceleratorTolerations),
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
		job.Labels[workloadkeys.JobGroup] = group.Name
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
		// Note the retry budget is NOT expressed here. BackoffLimit cannot restart a gang: on an
		// Indexed Job it recreates the failed *index* and leaves the survivors blocked, which is
		// the behaviour above under another name, and FailJob is terminal so it bypasses
		// BackoffLimit entirely. Gang retry is therefore a control-plane decision written as
		// desired state (see the scheduler's job watcher), and BackoffLimit is pinned to 0 so
		// there is exactly one retry authority rather than two disagreeing ones.
		zero := int32(0)
		job.Spec.BackoffLimit = &zero

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
