package domain

// JobSpec is the platform's own execution DSL — the only thing an agent ever writes to describe
// how a workload runs. It deliberately exposes no execution-engine concepts (no manifests, pod
// templates, or CRDs): whatever backend is configured (k8s today; conceivably Slurm/Ray tomorrow)
// compiles this down into its own native resource, deterministically, from the same desired state.
type JobSpec struct {
	// Image is the container image to run. Required.
	Image string `json:"image" yaml:"image"`
	// Command overrides the image's entrypoint, if set.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	// Args are appended after Command (or after the image's own entrypoint if Command is unset).
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
	// Env is injected into every node's container, in addition to the platform's own
	// HYPOTHESISLOOP_* env vars (experiment id, rank, world size, etc).
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// CPU/Memory/Storage are plain resource-quantity strings (e.g. "4", "16Gi") for the
	// non-Accelerator resources each node gets. All three are required.
	// Storage is ephemeral scratch space (k8s ephemeral-storage) — not a persistent volume.
	CPU     string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage string `json:"storage,omitempty" yaml:"storage,omitempty"`

	// AcceleratorCount is accelerators requested per node (not the job total — see TotalAccelerators).
	AcceleratorCount int `json:"accelerator_count" yaml:"accelerator_count"`

	// AcceleratorType names the hardware this job wants, as the driver itself publishes it —
	// e.g. "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3" or "tenstorrent.com/chipArch=blackhole".
	// See domain.AcceleratorType. Required when AcceleratorCount > 0. How that hardware is
	// actually requested (a DRA claim, or an extended resource plus node affinity) is resolved
	// from the cluster's live inventory, never declared here.
	AcceleratorType AcceleratorType `json:"accelerator_type,omitempty" yaml:"accelerator_type,omitempty"`

	// AcceptableAcceleratorTypes are alternatives the job runs equally well on, in preference
	// order, written in the same driver-published form. Admission may place the job on any of
	// them when the requested type is busy, and rewrites the experiment's type to whichever it
	// actually got — so billing always reflects the hardware that really ran.
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
	// alongside the device itself (e.g. {"hugepages-1Gi": "4Gi"}).
	AcceleratorPodResources map[string]string `json:"accelerator_pod_resources,omitempty" yaml:"accelerator_pod_resources,omitempty"`

	// NumNodes is how many identical nodes this job spans. 1 (default) is a single-node job;
	// >1 requests a distributed run — the backend handles rank assignment and per-node retry
	// isolation automatically. Every container gets both HYPOTHESISLOOP_{RANK,WORLD_SIZE,
	// MASTER_ADDR,MASTER_PORT} and the standard unprefixed PyTorch/torchrun equivalents, so an
	// ordinary torch.distributed.init_process_group(env://) script works with no glue code. The
	// Job completes only once every rank succeeds — no coordinator-only completion mode.
	NumNodes int `json:"num_nodes,omitempty" yaml:"num_nodes,omitempty"`
	// MaxRetries is how many times a failing node is retried before the job is marked
	// failed. Required and non-negative.
	MaxRetries *int `json:"max_retries" yaml:"max_retries"`

	// TerminationGracePeriodSeconds overrides the cluster's default pod shutdown grace period.
	// Empty means "use the platform-wide configured default"; capped at the configured max
	// regardless of what's requested here.
	TerminationGracePeriodSeconds *int64 `json:"termination_grace_period_seconds,omitempty" yaml:"termination_grace_period_seconds,omitempty"`

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

// TotalAccelerators returns the job's total accelerator footprint across all nodes (AcceleratorCount × NumNodes),
// which is what admission/quota/eviction actually reserve and bill against.
func (j JobSpec) TotalAccelerators() int {
	return j.AcceleratorCount * j.Nodes()
}

// Nodes returns NumNodes, floored at 1 (0/unset means a plain single-node job).
func (j JobSpec) Nodes() int {
	if j.NumNodes < 1 {
		return 1
	}
	return j.NumNodes
}

// ExperimentMeta holds the fields a submission needs to describe research intent and
// bookkeeping around a run — never how it executes (that's JobSpec).
type ExperimentMeta struct {
	AgentID              string  `json:"agent_id" yaml:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id" yaml:"platform_experiment_id"`
	ProjectID            string  `json:"project_id" yaml:"project_id"`
	ParentID             *string `json:"parent_id,omitempty" yaml:"parent_id,omitempty"`

	// HypothesisID is required: the ID of a hypothesis previously registered (or retrieved,
	// if equivalent text already existed) via POST /registry/hypotheses.
	HypothesisID string `json:"hypothesis_id" yaml:"hypothesis_id"`
	Hypothesis   string `json:"hypothesis" yaml:"hypothesis"`
	Objective    string `json:"objective" yaml:"objective"`
	Theory       string `json:"theory,omitempty" yaml:"theory,omitempty"`

	CodeRef    string `json:"code_ref" yaml:"code_ref"`
	ConfigHash string `json:"config_hash" yaml:"config_hash"`
	DataRef    string `json:"data_ref" yaml:"data_ref"`

	// CapacityTier picks guaranteed (FIFO, can preempt burst) vs. burst (best-effort,
	// preemptable) scheduling, translated into the backend's native priority mechanism (a k8s
	// PriorityClass today).
	CapacityTier           CapacityTier `json:"capacity_tier" yaml:"capacity_tier"`
	EstimatedDurationHours float64      `json:"estimated_duration_hours" yaml:"estimated_duration_hours"`
}
