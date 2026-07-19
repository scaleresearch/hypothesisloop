package domain

// JobSpec is the platform's own execution DSL — the only thing an agent ever writes to
// describe how a workload runs. It deliberately exposes no execution-engine concepts
// (no manifests, no pod templates, no CRDs): whatever backend is configured (a native k8s
// Job today; conceivably Slurm/Ray/etc tomorrow) compiles this down into its own native
// resource, merging in operator-owned per-cluster defaults for anything left unset (see
// workload.MergeJobDefaults). See settings/examples/experiment-submission.yaml for a fully-commented example.
type JobSpec struct {
	// Image is the container image to run. Required.
	Image string `json:"image" yaml:"image"`
	// Command overrides the image's entrypoint, if set.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	// Args are appended after Command (or after the image's own entrypoint if Command is unset).
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
	// Env is injected into every node's container, in addition to the platform's own
	// OPENRESEARCH_* env vars (experiment id, rank, world size, etc).
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// CPU/Memory/Storage are plain resource-quantity strings (e.g. "4", "16Gi") for the
	// non-Accelerator resources each node gets. Empty means "use the operator's per-cluster default".
	// Storage is ephemeral scratch space (k8s ephemeral-storage) — not a persistent volume.
	CPU     string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage string `json:"storage,omitempty" yaml:"storage,omitempty"`

	// AcceleratorType is the hardware tier this run is billed against (T4/L40/A100/H100/H200).
	AcceleratorType AcceleratorType `json:"accelerator_type" yaml:"accelerator_type"`
	// AcceleratorCount is accelerators requested per node (not the job total — see TotalAccelerators).
	AcceleratorCount int `json:"accelerator_count" yaml:"accelerator_count"`
	// AcceptableAcceleratorTypes, if set, lets the run land on any of these hardware tiers
	// interchangeably (e.g. H100 or H200) instead of requiring exactly AcceleratorType. The rate
	// charged is still adjusted to whichever tier it actually lands on.
	AcceptableAcceleratorTypes []AcceleratorType `json:"acceptable_accelerator_types,omitempty" yaml:"acceptable_accelerator_types,omitempty"`

	// NumNodes is how many identical nodes this job spans. 1 (default) is a plain
	// single-node job; >1 requests a distributed run — the backend handles rank
	// assignment and per-node retry isolation automatically, with no further DSL needed
	// from the agent. Every container gets both OPENRESEARCH_{RANK,WORLD_SIZE,MASTER_ADDR,
	// MASTER_PORT} and the standard unprefixed PyTorch/torchrun equivalents (RANK,
	// LOCAL_RANK, WORLD_SIZE, MASTER_ADDR, MASTER_PORT) with the same values, so an ordinary
	// torch.distributed.init_process_group(env://) script works with no glue code. The Job
	// completes only once every rank succeeds — there is no coordinator-only completion mode.
	NumNodes int `json:"num_nodes,omitempty" yaml:"num_nodes,omitempty"`
	// MaxRetries is how many times a failing node is retried before the job is marked
	// failed. Empty means "use the operator's per-cluster default".
	MaxRetries *int `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`

	// Topology controls where a distributed (NumNodes > 1) job's nodes land relative to
	// each other. Only meaningful when NumNodes > 1; ignored otherwise. See TopologySpec.
	Topology *TopologySpec `json:"topology,omitempty" yaml:"topology,omitempty"`

	// ShmSize sets the size of /dev/shm (e.g. "4Gi"). k8s defaults /dev/shm to a tiny
	// tmpfs, which silently breaks PyTorch's multiprocess DataLoader workers and NCCL's
	// shared-memory IPC — both need real shared memory for multi-Accelerator/multi-process
	// training. Empty means "use the operator's per-cluster default".
	ShmSize string `json:"shm_size,omitempty" yaml:"shm_size,omitempty"`

	// ExtraResources requests any k8s extended resource AcceleratorType/AcceleratorCount doesn't cover —
	// TPUs (google.com/tpu), other AI accelerators (habana.ai/gaudi, aws.amazon.com/neuron,
	// ...), or anything else a device plugin advertises — as plain quantity strings per node
	// (e.g. {"google.com/tpu": "8"}), same convention as CPU/Memory/Storage. These are NOT
	// billed or capped by the quota system today (no rate/cap exists for an open-ended
	// resource-name map, unlike the fixed accelerator/CPU/RAM/storage dimensions) — they pass straight
	// through to the pod's resource requests/limits so a job can still get scheduled onto the
	// right hardware; add a matching budget dimension if/when one of these needs billing.
	ExtraResources map[string]string `json:"extra_resources,omitempty" yaml:"extra_resources,omitempty"`
}

// TopologySpec expresses placement requirements for a distributed job's nodes — the
// difference between "8 pods that happen to exist" and "8 pods that can actually do
// synchronous multi-host gradient sync." Everything here is plain, backend-agnostic
// vocabulary; the k8s backend compiles it into pod (anti-)affinity, other backends would
// compile it into their own native placement/gang-scheduling primitives.
type TopologySpec struct {
	// SpreadAcrossHosts requires no two of this job's nodes to land on the same physical
	// host — the whole point of NumNodes > 1 is more physical accelerators, so two ranks sharing a
	// host silently halves your real parallelism. Defaults to true whenever NumNodes > 1
	// (set explicitly to false only for local/dev clusters with fewer hosts than NumNodes).
	SpreadAcrossHosts *bool `json:"spread_across_hosts,omitempty" yaml:"spread_across_hosts,omitempty"`
	// SameZone prefers placing every node of this job in the same topology zone/rack,
	// minimizing cross-zone network latency for gradient all-reduce (NCCL) — the standard
	// reason distributed training throughput falls off a cliff when ranks are scattered
	// across a datacenter. Best-effort (a preference, not a hard requirement): the very
	// first rank has no sibling to co-locate with yet, so this can never be a hard
	// same-zone requirement without risking the whole job going unschedulable.
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

// ExperimentMeta is exactly the fields a submission needs that describe the research
// intent and bookkeeping around a run — never how it executes (that's JobSpec).
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
	// preemptable) scheduling — translated by the backend into whatever native priority
	// mechanism it has (a k8s PriorityClass today).
	CapacityTier           CapacityTier `json:"capacity_tier" yaml:"capacity_tier"`
	EstimatedDurationHours float64      `json:"estimated_duration_hours" yaml:"estimated_duration_hours"`
}
