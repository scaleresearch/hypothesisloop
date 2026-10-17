// Package clusterapi is the wire contract between the control plane and the runtimes that pull
// desired state from /internal/clusters/{name}. It imports nothing of the platform: the control
// plane maps its own types onto these at the edge, and a runtime reads only these.
package clusterapi

import (
	"fmt"
	"time"
)

// MaxIdentifierLength bounds every id the control plane hands a runtime, at the DNS label limit
// Kubernetes object names are built from.
const MaxIdentifierLength = 63

// DesiredJob is one job a runtime is told should exist on its cluster: what it builds a workload
// from, and nothing it has no use for. A runtime may be another org's, so nothing about where
// the job sits in its org travels here.
type DesiredJob struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	ProjectID  string `json:"project_id"`
	CodeRef    string `json:"code_ref"`
	ConfigHash string `json:"config_hash"`
	DataRef    string `json:"data_ref"`
	// Job is the job as submitted, with its secret_env. ResolvedJob is it as admission resolved
	// it onto this cluster, nil when nothing was resolved: read EffectiveJob.
	Job         JobSpec  `json:"job"`
	ResolvedJob *JobSpec `json:"resolved_job,omitempty"`
	// EnvironmentKey is set on a job that runs in an environment image, which is pulled with the
	// reconcile answer's registry_pull.
	EnvironmentKey         string          `json:"environment_key,omitempty"`
	AcceleratorType        AcceleratorType `json:"accelerator_type"`
	AcceleratorCount       int             `json:"accelerator_count"`
	EstimatedDurationHours float64         `json:"estimated_duration_hours"`
	CapacityTier           CapacityTier    `json:"capacity_tier"`
	// SubmittedAt is when the job was admitted, which a runtime that waits for a machine to rent
	// measures that wait from.
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	// Data is the job's durable-data address and key, and Token its own platform token, good
	// for reporting its metrics and reading its repository through the git proxy.
	Data  *DataAccess `json:"data,omitempty"`
	Token string      `json:"token,omitempty"`
	// GitURL is where the job fetches its commit from with Token: its platform experiment's
	// repository through the control plane's git proxy.
	GitURL string `json:"git_url,omitempty"`
	// AttemptCount numbers the job's generation. A requeue reuses the job's id and advances it,
	// so every attempt is a distinct desired state and its workload a distinct one.
	AttemptCount int `json:"attempt_count"`
}

// EffectiveJob is the job as it should be run: ResolvedJob when admission resolved one, Job
// otherwise.
func (j *DesiredJob) EffectiveJob() JobSpec {
	if j.ResolvedJob != nil {
		return *j.ResolvedJob
	}
	return j.Job
}

// CapacityTier is whether a job holds guaranteed or burst capacity.
type CapacityTier string

const (
	CapacityGuaranteed CapacityTier = "guaranteed"
	CapacityBurst      CapacityTier = "burst"
)

// DataAccess is where a job writes durable data, where it may read it, and the key for both.
type DataAccess struct {
	// URI is the job's own prefix, writable. Shared is its platform experiment's, readable.
	URI    string `json:"uri"`
	Shared string `json:"shared"`

	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`

	// The job's own permanent key, scoped by the store to exactly the two grants above.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// JobSpec is the platform's own execution DSL — the only thing an agent ever writes to describe
// how a workload runs. It deliberately exposes no execution-engine concepts (no manifests, pod
// templates, or CRDs): whatever backend is configured (k8s today; conceivably Slurm/Ray tomorrow)
// compiles this down into its own native resource, deterministically, from the same desired state.
type JobSpec struct {
	// Image is the container image to run. Left out, the job is placed only on a cluster that
	// runs one image (Cluster.Runs), and admission writes that image into
	// Experiment.ResolvedJob, which always carries the exact reference the runtime runs.
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
	// Command overrides the image's entrypoint, if set.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	// Args are appended after Command (or after the image's own entrypoint if Command is unset).
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
	// Env is injected into every node's container, in addition to the platform's own
	// HYPOTHESISLOOP_* env vars (experiment id, rank, world size, etc).
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// SecretEnv is injected like Env and never shown again: a submission carries it, the
	// platform stores it apart from the spec (Experiment.SecretEnv), and only the runtime that
	// runs the job is handed it. No read of a job returns it. See ValidateSecretEnv.
	SecretEnv map[string]string `json:"secret_env,omitempty" yaml:"secret_env,omitempty" writeOnly:"true"`

	// CPU/Memory/Storage are plain resource-quantity strings (e.g. "4", "16Gi") for the
	// non-Accelerator resources each node gets.
	// Storage is ephemeral scratch space (k8s ephemeral-storage), not a persistent volume.
	// All three are always stated as quantities: a submit that leaves one out is refused. On a
	// cluster of slots admission sizes them to the slot the job holds, into
	// domain.Experiment.ResolvedJob's copy. Job itself is NEVER rewritten, so every downstream
	// consumer (Footprint, BuildJobs, cost estimation) reads domain.Experiment.EffectiveJob()
	// instead of this field directly.
	//
	// A job that names no accelerator runs on cores: cpu is then a whole number and the job holds
	// that many units of CPUType. Submission writes the count into the job.
	// cpu is written as a number (cpu: 4) or a string ("4", "500m") and is kept and answered as
	// the string.
	CPU     string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage string `json:"storage,omitempty" yaml:"storage,omitempty"`

	// AcceleratorCount is accelerators requested per node (not the job total — see TotalAccelerators).
	//
	// required:"false" without omitempty: the schema must accept its absence, because a grouped
	// job REJECTS this field (each group carries its own count) and so cannot send it — but the
	// zero value must still serialize, since a CPU-only job asking for 0 accelerators is saying
	// something, not omitting it. Whether it is required is a rule about the whole spec, which
	// ValidateGroups and ValidateExperiment own; huma's per-field default of "required" cannot
	// express "required unless groups is set" and, left alone, refused every grouped submission.
	AcceleratorCount int `json:"accelerator_count" required:"false" yaml:"accelerator_count"`

	// AcceleratorType names the hardware this job wants, as the driver itself publishes it —
	// e.g. "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3" or "tenstorrent.com/chipArch=blackhole".
	// See AcceleratorType. Required when AcceleratorCount > 0. How that hardware is
	// actually requested (a DRA claim, or an extended resource plus node affinity) is resolved
	// from the cluster's live inventory, never declared here.
	AcceleratorType AcceleratorType `json:"accelerator_type,omitempty" yaml:"accelerator_type,omitempty"`

	// AcceptableAcceleratorTypes are alternatives the job runs equally well on, in preference
	// order, written in the same driver-published form. Admission may place the job on any of
	// them when the requested type is busy, and rewrites the experiment's type to whichever it
	// actually got — so billing always reflects the hardware that really ran. Every alternative
	// is of the kind of AcceleratorType: a count is cores or devices, never either.
	AcceptableAcceleratorTypes []AcceleratorType `json:"acceptable_accelerator_types,omitempty" yaml:"acceptable_accelerator_types,omitempty"`

	// NodeSelector constrains placement to nodes carrying these labels. Plain Kubernetes node
	// selection for anything the job cares about that is not the accelerator — a zone, a disk
	// class, a kernel version. The accelerator is identified by AcceleratorType alone (see
	// Experiment.AcceleratorType); this is ANDed with wherever that hardware lives, and may be
	// omitted entirely.
	NodeSelector map[string]string `json:"node_selector,omitempty" yaml:"node_selector,omitempty"`

	// AcceleratorTolerations are taint keys the job tolerates so it can land on tainted
	// accelerator nodes. Kept explicit rather than derived: whether a cluster taints its
	// accelerator nodes is a property of that cluster, not of the hardware model.
	AcceleratorTolerations []string `json:"accelerator_tolerations,omitempty" yaml:"accelerator_tolerations,omitempty"`

	// AcceleratorPodResources are extra per-node resource requests an accelerator needs
	// alongside the device itself (e.g. {"hugepages-1Gi": "4Gi"}). What a type always needs is
	// the catalog's (AcceleratorType.PodResources) and admission writes it into ResolvedJob, so a
	// job states only what is its own.
	AcceleratorPodResources map[string]string `json:"accelerator_pod_resources,omitempty" yaml:"accelerator_pod_resources,omitempty"`

	// Groups expresses a job whose nodes are NOT identical — a learner alongside a fleet of
	// actors, a parameter server alongside workers. Each group carries its own per-replica
	// resources, command, args and env; everything that describes the JOB rather than a node
	// within it (image, accelerator type, tolerations, node selector, topology, max_retries)
	// stays at the top level and is shared by every group.
	//
	// Optional. Absent, the job is the identical-nodes shape described by NumNodes and the
	// top-level resource fields, and nothing about it changes. Present, the top-level per-node
	// resource fields (cpu/memory/storage/accelerator_count) and num_nodes are REJECTED at
	// submission: there is one way to say a thing and no merge rules. See ValidateGroups.
	//
	// A grouped job is still ONE experiment — one quota holder, one admission decision, one
	// eviction, one lineage entry — which is exactly what submitting the groups as separate jobs
	// cannot give. Nodes(), TotalAccelerators() and Footprint() therefore SUM the groups where
	// their ungrouped counterparts multiply one shape by NumNodes.
	//
	// One accelerator type per job: a group may name AcceleratorType, but every group that does
	// must name the same one, because a single AcceleratorType on the experiment is what
	// admission, accelerator type substitution and billing all key on.
	Groups []JobGroup `json:"groups,omitempty" yaml:"groups,omitempty"`

	// NumNodes is how many identical nodes this job spans. 1 (default) is a single-node job;
	// >1 requests a distributed run, and the backend assigns ranks and publishes the rendezvous.
	// Every container gets both HYPOTHESISLOOP_{RANK,WORLD_SIZE,MASTER_ADDR,MASTER_PORT} and the
	// standard unprefixed PyTorch equivalents. The Job completes only once every rank succeeds —
	// no coordinator-only completion mode — and any rank failing fails the whole job, because a
	// gang is one unit (see MaxRetries).
	//
	// RANK and WORLD_SIZE are the NODE index and NODE count, and LOCAL_RANK is hardcoded 0 under
	// this code's assumption of one process per pod. At one accelerator per node that is exactly
	// what torch.distributed.init_process_group(env://) wants, and such a script runs with no glue
	// code. At several accelerators per node it is NOT: the workload must start the per-device
	// processes itself, which is what HYPOTHESISLOOP_ACCELERATOR_COUNT (per node, never the job
	// total) is for:
	//
	//	torchrun --nnodes=$WORLD_SIZE --node_rank=$RANK \
	//	         --nproc_per_node=$HYPOTHESISLOOP_ACCELERATOR_COUNT \
	//	         --master_addr=$MASTER_ADDR --master_port=$MASTER_PORT train.py
	//
	// torchrun then overrides the inherited node-level values with each process's real ones. This
	// is worth stating because getting it wrong is silent: an agent reading only the variable
	// names concludes WORLD_SIZE is its process count, builds a job that uses one device per node
	// out of several, and still trains and still reports metrics — just under-parallelised.
	NumNodes int `json:"num_nodes,omitempty" yaml:"num_nodes,omitempty"`
	// MaxRetries is how many times a failing node is RETRIED, not how many attempts run: N
	// means up to N+1 attempts, so max_retries 2 gives 3 runs before the job is marked failed
	// and max_retries 0 means a single attempt. Worth being deliberate about — every retry of a
	// job that fails for a deterministic reason (a code bug, a bad config) re-burns its full
	// accelerator time to reach the same failure, so 0 is the honest setting for a diagnostic
	// run you expect to fail. Required and non-negative.
	//
	// It bounds pod-level retries. A container can also be restarted in place by the kubelet,
	// which is a separate mechanism this field does not reach — so max_retries 0 is executed
	// with in-place restarts disabled too, to make "no retries" mean one attempt rather than
	// one pod. For N > 0 the two layers still compose: up to N+1 pods, each of which the
	// kubelet may also restart.
	//
	// The retried unit is the whole job, which for num_nodes > 1 means the whole gang: every
	// rank is torn down and all N nodes start again together. It is deliberately not per-rank.
	// Restarting one rank of a synchronous job leaves the survivors blocked in a collective
	// holding their accelerators until the backend's timeout, while the replacement rejoins a
	// rendezvous the others have already left — the gang burns its full allocation and fails
	// anyway. An agent whose ranks really are independent should submit independent jobs.
	//
	// Where the budget is enforced differs by shape, because only one layer can enforce it in
	// each case: a single-pod job's retries are the runtime's (k8s BackoffLimit), and a gang's
	// are the control plane's, since Kubernetes can recreate a failed index — stranding the
	// survivors — or fail the Job outright, but has no way to restart a gang.
	//
	// Left out, it is 0: the job runs once. Submission writes that into the job.
	MaxRetries *int `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`

	// TerminationGracePeriodSeconds overrides the platform's default shutdown grace period.
	// Empty means "use the platform-wide configured default"; capped at the configured max
	// regardless of what's requested here. Admission writes the resolved value into
	// Experiment.ResolvedJob, which always carries it.
	TerminationGracePeriodSeconds *int64 `json:"termination_grace_period_seconds,omitempty" yaml:"termination_grace_period_seconds,omitempty"`

	// CheckpointGraceSeconds is how long the job asks for, after it is told termination is
	// coming, to write a checkpoint it can resume from. It applies only to a policy-class
	// termination (see FaultClass) — preemption, a stage cut, quota exhaustion, the duration cap
	// — because those are the only ones where the job is fine and there is something left to
	// save. An infrastructure or workload failure gets no window: there is nothing to save, or
	// nothing left to save it with.
	//
	// Unset means today's behaviour unchanged: the job is stopped with the ordinary shutdown
	// grace above. Capped at the configured max_checkpoint_grace_seconds regardless of what is
	// requested here, and that cap is the whole reason this is safe to offer — without it a job
	// could hold contended accelerators indefinitely by claiming it was still saving.
	//
	// Nothing about the window tells the job what to do with it. It is told termination is
	// coming (SIGTERM, delivered to every rank of every group at once) and then has this long
	// before it is killed. Resuming needs no new platform concept: the job's data prefix is
	// keyed on its experiment id and a requeue keeps that id, so a checkpoint written here is at
	// the same URI when the job starts again — it reads its own prefix and continues, or finds
	// it empty and begins.
	CheckpointGraceSeconds *int64 `json:"checkpoint_grace_seconds,omitempty" yaml:"checkpoint_grace_seconds,omitempty"`

	// Topology controls where a distributed (NumNodes > 1) job's nodes land relative to
	// each other. Only meaningful when NumNodes > 1; ignored otherwise. See TopologySpec.
	Topology *TopologySpec `json:"topology,omitempty" yaml:"topology,omitempty"`

	// ShmSize sets the size of /dev/shm (e.g. "4Gi"). k8s defaults /dev/shm to a tiny tmpfs,
	// which silently breaks PyTorch's DataLoader workers and NCCL's shared-memory IPC. Empty
	// means no explicit /dev/shm volume.
	ShmSize string `json:"shm_size,omitempty" yaml:"shm_size,omitempty"`

	// ExtraResources requests any k8s extended resource AcceleratorType/AcceleratorCount doesn't
	// cover — TPUs, other AI accelerators, or anything a device plugin advertises — as plain
	// quantity strings per node (e.g. {"google.com/tpu": "8"}). Not billed or capped by the quota
	// system today; passes straight through to pod resource requests/limits so the job can still
	// get scheduled onto the right hardware.
	ExtraResources map[string]string `json:"extra_resources,omitempty" yaml:"extra_resources,omitempty"`

	// HostMounts bind-mounts a static, already-populated directory on whichever node the job
	// lands on, read-only, into the container — keyed by the container path the job expects to
	// read it at, valued by the host path on the node. This is the platform's lightweight
	// hostPath-volume equivalent: for data that already exists on disk out of band (a dataset
	// fetched once, ahead of time), never a fetch-on-demand cache — if the named host path
	// doesn't exist on the node a job actually gets placed on, the job fails fast (missing mount)
	// rather than silently running without the data. Because it's node-local, a job only sees the
	// mount if it lands on a node that has it; NodeSelector is the tool for pinning a job to nodes
	// known to carry a given mount. Supported identically by both the k8s and bare-metal backends.
	HostMounts map[string]string `json:"host_mounts,omitempty" yaml:"host_mounts,omitempty"`
}

// TopologySpec expresses placement requirements for a distributed job's nodes — the difference
// between "8 pods that happen to exist" and "8 pods that can do synchronous multi-host gradient
// sync." Backend-agnostic vocabulary; the k8s backend compiles it into pod (anti-)affinity.
type TopologySpec struct {
	// SpreadAcrossHosts requires no two of this job's nodes to land on the same physical host —
	// two ranks sharing a host silently halves real parallelism. Defaults to true whenever
	// NumNodes > 1 (set false only for local/dev clusters with fewer hosts than NumNodes).
	SpreadAcrossHosts *bool `json:"spread_across_hosts,omitempty" yaml:"spread_across_hosts,omitempty"`
	// SameZone prefers co-locating every node in the same topology zone/rack, minimizing
	// cross-zone latency for gradient all-reduce (NCCL). Best-effort only: the first rank has no
	// sibling to co-locate with yet, so this can't be a hard requirement without risking the
	// whole job going unschedulable.
	SameZone bool `json:"same_zone,omitempty" yaml:"same_zone,omitempty"`
}

// JobGroup is one homogeneous slice of a heterogeneous job: Replicas identical nodes with their
// own resources and their own process to run. See JobSpec.Groups.
type JobGroup struct {
	// Name identifies the group to the workload (HYPOTHESISLOOP_GROUP) and names the native
	// resource each backend compiles it into, so it must be a DNS-label-safe token. Required and
	// unique within a job.
	Name string `json:"name" yaml:"name"`
	// Replicas is how many nodes this group spans. Required and at least 1 — a group of zero
	// replicas is a group that should not have been written down.
	Replicas int `json:"replicas" yaml:"replicas"`

	// CPU/Memory/Storage are this group's PER-REPLICA resource-quantity strings, identical in
	// meaning to JobSpec's own, and always stated.
	CPU     string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage string `json:"storage,omitempty" yaml:"storage,omitempty"`

	// AcceleratorCount is accelerators per replica of this group. Zero is legitimate and is the
	// point of the feature: 64 CPU-only actors alongside one 8-accelerator learner.
	AcceleratorCount int `json:"accelerator_count,omitempty" yaml:"accelerator_count,omitempty"`
	// AcceleratorType is optional and, when set, must equal every other group's and the job's.
	// It exists so a spec can be explicit, never so groups can differ — see JobSpec.Groups.
	AcceleratorType AcceleratorType `json:"accelerator_type,omitempty" yaml:"accelerator_type,omitempty"`

	// Command/Args/Env are this group's own process. Env is merged over the job-level Env,
	// which is the one place a group value shadows a job value — the job-level map is the
	// job's shared environment and the group's is the part specific to its role.
	Command []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Args    []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// NodeGroups is the shapes the job consists of: its groups, or for an ungrouped job one group of
// the top-level shape repeated Nodes() times.
func (j JobSpec) NodeGroups() []JobGroup {
	if len(j.Groups) > 0 {
		return j.Groups
	}
	return []JobGroup{{
		Replicas:         j.Nodes(),
		CPU:              j.CPU,
		Memory:           j.Memory,
		Storage:          j.Storage,
		AcceleratorCount: j.AcceleratorCount,
		AcceleratorType:  j.AcceleratorType,
		Command:          j.Command,
		Args:             j.Args,
		Env:              j.Env,
	}}
}

// TotalAccelerators is the job's accelerators across all its nodes.
func (j JobSpec) TotalAccelerators() int {
	total := 0
	for _, g := range j.NodeGroups() {
		total += g.AcceleratorCount * g.Replicas
	}
	return total
}

// Nodes is how many nodes the job spans: the sum of its groups' replicas, or NumNodes floored
// at 1.
func (j JobSpec) Nodes() int {
	if len(j.Groups) > 0 {
		total := 0
		for _, g := range j.Groups {
			total += g.Replicas
		}
		return total
	}
	if j.NumNodes < 1 {
		return 1
	}
	return j.NumNodes
}

// ResolvedGrace is the grace a runtime applies, from a job admission resolved: the ordinary
// shutdown grace and the checkpoint window (0 when the job declared none). A job without a
// shutdown grace was not resolved by admission and is refused.
func (j JobSpec) ResolvedGrace() (shutdown, checkpoint int64, err error) {
	if j.TerminationGracePeriodSeconds == nil {
		return 0, 0, fmt.Errorf("job spec carries no resolved termination grace")
	}
	if j.CheckpointGraceSeconds != nil {
		checkpoint = *j.CheckpointGraceSeconds
	}
	return *j.TerminationGracePeriodSeconds, checkpoint, nil
}
