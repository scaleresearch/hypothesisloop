package domain

import (
	"math"
	"strings"
	"time"
)

// MinRemainingHours is the floor for RemainingEstimatedHours (15 min).
const MinRemainingHours = 0.25

// Phase1ExploreFraction is the fraction of total budget consumed before phase-2 eviction
// triggers. Agents' initial quotas are capped to this fraction so no single agent can
// exhaust the explore window alone.
const Phase1ExploreFraction = 0.40

// ExperimentStatus represents the lifecycle state of an agent job.
type ExperimentStatus string

const (
	StatusSubmitted ExperimentStatus = "SUBMITTED"
	StatusQueued    ExperimentStatus = "QUEUED"
	StatusAdmitted  ExperimentStatus = "ADMITTED"
	StatusRunning   ExperimentStatus = "RUNNING"
	StatusCompleted ExperimentStatus = "COMPLETED"
	StatusFailed    ExperimentStatus = "FAILED"
	StatusEvicted   ExperimentStatus = "EVICTED"
	StatusRejected  ExperimentStatus = "REJECTED"
)

// GPUType identifies the hardware tier for a job. It is deliberately an open, string-based
// identifier, not a closed enum: the const block below only seeds a fallback rate table for
// tests/local runs without config — the real catalog is entirely operator-defined via
// openresearch.yaml's gpu_types (see config.GPUTypeConfig), and any vendor's model name works
// (NVIDIA H100, AMD MI300X, ...). Nothing in this codebase switches on these specific consts.
type GPUType string

const (
	GPUT4   GPUType = "T4"
	GPUL40  GPUType = "L40"
	GPUA100 GPUType = "A100"
	GPUH100 GPUType = "H100"
	GPUH200 GPUType = "H200"
)

// gpuRateRegistry is populated by SetGPURates() at startup from openresearch.yaml.
// Falls back to hardcoded defaults when not set (tests, local runs without config).
var gpuRateRegistry = map[GPUType]float64{
	GPUT4:   1.0,
	GPUL40:  2.0,
	GPUA100: 3.0,
	GPUH100: 8.0,
	GPUH200: 10.0,
}

// SetGPURates registers T4h rates loaded from config. Call once at startup.
func SetGPURates(rates map[string]float64) {
	for name, rate := range rates {
		gpuRateRegistry[GPUType(name)] = rate
	}
}

// FlavorName returns the flavor name for this GPU type ("flavor-t4", etc.).
func (g GPUType) FlavorName() string {
	return "flavor-" + strings.ToLower(string(g))
}

// Cost returns the T4-GPU-hour cost per GPU-hour for this GPU type, falling back to 1.0 (the T4
// baseline rate) for an unregistered type. Safe for pre-flight estimates, where a wrong guess
// just gets corrected once the job is actually admitted onto a real, catalog-known type — but NOT
// safe for final billing, where a typo'd or deprecated type must not silently settle at the wrong
// rate. Billing code should use LookupCost instead and treat "not found" as an error.
func (g GPUType) Cost() float64 {
	if r, ok := gpuRateRegistry[g]; ok {
		return r
	}
	return 1.0
}

// LookupCost returns this GPU type's registered T4-GPU-hour rate and whether it was found in the
// catalog (populated by SetGPURates from openresearch.yaml's gpu_types at startup). Unlike Cost,
// this never silently substitutes a fallback rate — used by final-settlement paths (see
// metricsdb.ObservedGPUCost) where an unrecognized type must fail loudly rather than bill at the
// wrong tier's price.
func (g GPUType) LookupCost() (float64, bool) {
	r, ok := gpuRateRegistry[g]
	return r, ok
}

// cpuCoreHourRate/ramGBHourRate/storageGBHourRate are flat per-unit rates (1.0 by default —
// unlike GPU there is no hardware-tier variation to normalize away), operator-overridable via
// openresearch.yaml so tiering (e.g. "fast NVMe" vs "standard" storage) can be introduced later
// without another migration. There is deliberately no cross-resource exchange rate between
// these and gpuRateRegistry — CPU-hours, RAM-GB-hours, and storage-GB-hours are independent
// pools, not converted into T4h credits.
var (
	cpuCoreHourRate   = 1.0
	ramGBHourRate     = 1.0
	storageGBHourRate = 1.0
)

// SetCPUCoreHourRate/SetRAMGBHourRate/SetStorageGBHourRate register the flat per-unit rate for
// each non-GPU resource dimension, loaded from config at startup. Zero/negative is ignored
// (keeps the 1.0 default) so an absent config key doesn't zero out the rate.
func SetCPUCoreHourRate(rate float64) {
	if rate > 0 {
		cpuCoreHourRate = rate
	}
}

func SetRAMGBHourRate(rate float64) {
	if rate > 0 {
		ramGBHourRate = rate
	}
}

func SetStorageGBHourRate(rate float64) {
	if rate > 0 {
		storageGBHourRate = rate
	}
}

// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate return the current per-unit rate for each
// resource dimension (see the SetX functions above).
func CPUCoreHourRate() float64   { return cpuCoreHourRate }
func RAMGBHourRate() float64     { return ramGBHourRate }
func StorageGBHourRate() float64 { return storageGBHourRate }

// CapacityTier specifies whether a job uses guaranteed or burst capacity.
type CapacityTier string

const (
	CapacityGuaranteed CapacityTier = "guaranteed"
	CapacityBurst      CapacityTier = "burst"
)

// ResourceType identifies one quota-tracked resource dimension. GPU-hours is the original,
// always-populated dimension (billed at GPUType's own tiered rate); the other three are flat
// per-unit pools (see cpuCoreHourRate/ramGBHourRate/storageGBHourRate) with no exchange rate
// between dimensions — each is checked/debited/refunded independently against its own
// guaranteed/burst pool on AgentQuota.
type ResourceType string

const (
	ResourceGPUHours       ResourceType = "gpu_hours"
	ResourceCPUCoreHours   ResourceType = "cpu_core_hours"
	ResourceRAMGBHours     ResourceType = "ram_gb_hours"
	ResourceStorageGBHours ResourceType = "storage_gb_hours"
)

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// MetricDefinition declares a single metric that jobs in a PlatformExperiment must emit.
// The key is the Prometheus metric name; direction is "maximize" or "minimize".
type MetricDefinition struct {
	Key       string `json:"key"`
	Direction string `json:"direction"` // "maximize" | "minimize"
}

// PlatformExperiment is the operator-defined compute envelope agents compete within.
type PlatformExperiment struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	BudgetT4Hours float64 `json:"budget_t4_hours"` // total compute in T4-GPU-hours
	// BudgetCPUCoreHours/BudgetRAMGBHours/BudgetStorageGBHours are optional additional resource
	// budgets tracked the same way as BudgetT4Hours (guaranteed/burst split, debited at
	// submission, refunded on completion/eviction). Zero means "not tracked for this platform
	// experiment" — existing GPU-only platform experiments are unaffected.
	BudgetCPUCoreHours    float64                  `json:"budget_cpu_core_hours,omitempty"`
	BudgetRAMGBHours      float64                  `json:"budget_ram_gb_hours,omitempty"`
	BudgetStorageGBHours  float64                  `json:"budget_storage_gb_hours,omitempty"`
	MaxAgents             int                      `json:"max_agents"`
	Metrics               []MetricDefinition       `json:"metrics"`                 // metrics jobs must emit; used for ranking
	ReportIntervalSeconds int                      `json:"report_interval_seconds"` // expected reporting cadence (for silent-eviction guard)
	StartsAt              time.Time                `json:"starts_at"`
	EndsAt                time.Time                `json:"ends_at"`
	Status                PlatformExperimentStatus `json:"status"`
	Phase                 int                      `json:"phase"` // 1 or 2
	Phase2TriggeredAt     *time.Time               `json:"phase2_triggered_at,omitempty"`
	SignedUpAgents        []string                 `json:"signed_up_agents,omitempty"`
	SignupCount           int                      `json:"signup_count"`
	CreatedAt             time.Time                `json:"created_at"`
	UpdatedAt             time.Time                `json:"updated_at"`
}

// AgentQuota is the compute allocation for an agent within a platform experiment. GPU-hours
// (T4h-normalized) is the original, always-populated dimension; CPU/RAM/storage fields are
// zero ("not tracked") unless the platform experiment sets a non-zero budget for them.
type AgentQuota struct {
	ID                   string  `json:"id"`
	AgentID              string  `json:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id"`
	GuaranteedT4Hours    float64 `json:"guaranteed_t4_hours"`
	BurstT4Hours         float64 `json:"burst_t4_hours"`
	UsedGuaranteedT4H    float64 `json:"used_guaranteed_t4h"`
	UsedBurstT4H         float64 `json:"used_burst_t4h"`

	GuaranteedCPUCoreHours float64 `json:"guaranteed_cpu_core_hours,omitempty"`
	BurstCPUCoreHours      float64 `json:"burst_cpu_core_hours,omitempty"`
	UsedGuaranteedCPUCoreH float64 `json:"used_guaranteed_cpu_core_h,omitempty"`
	UsedBurstCPUCoreH      float64 `json:"used_burst_cpu_core_h,omitempty"`

	GuaranteedRAMGBHours float64 `json:"guaranteed_ram_gb_hours,omitempty"`
	BurstRAMGBHours      float64 `json:"burst_ram_gb_hours,omitempty"`
	UsedGuaranteedRAMGBH float64 `json:"used_guaranteed_ram_gb_h,omitempty"`
	UsedBurstRAMGBH      float64 `json:"used_burst_ram_gb_h,omitempty"`

	GuaranteedStorageGBHours float64 `json:"guaranteed_storage_gb_hours,omitempty"`
	BurstStorageGBHours      float64 `json:"burst_storage_gb_hours,omitempty"`
	UsedGuaranteedStorageGBH float64 `json:"used_guaranteed_storage_gb_h,omitempty"`
	UsedBurstStorageGBH      float64 `json:"used_burst_storage_gb_h,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// AvailableGuaranteed returns T4h available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteed() float64 {
	return math.Max(0, q.GuaranteedT4Hours-q.UsedGuaranteedT4H)
}

// AvailableBurst returns T4h available for new burst jobs.
func (q *AgentQuota) AvailableBurst() float64 {
	return math.Max(0, q.BurstT4Hours-q.UsedBurstT4H)
}

// AvailableGuaranteedCPU returns CPU-core-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedCPU() float64 {
	return math.Max(0, q.GuaranteedCPUCoreHours-q.UsedGuaranteedCPUCoreH)
}

// AvailableBurstCPU returns CPU-core-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstCPU() float64 {
	return math.Max(0, q.BurstCPUCoreHours-q.UsedBurstCPUCoreH)
}

// AvailableGuaranteedRAM returns RAM-GB-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedRAM() float64 {
	return math.Max(0, q.GuaranteedRAMGBHours-q.UsedGuaranteedRAMGBH)
}

// AvailableBurstRAM returns RAM-GB-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstRAM() float64 {
	return math.Max(0, q.BurstRAMGBHours-q.UsedBurstRAMGBH)
}

// AvailableGuaranteedStorage returns storage-GB-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedStorage() float64 {
	return math.Max(0, q.GuaranteedStorageGBHours-q.UsedGuaranteedStorageGBH)
}

// AvailableBurstStorage returns storage-GB-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstStorage() float64 {
	return math.Max(0, q.BurstStorageGBHours-q.UsedBurstStorageGBH)
}

// Experiment is an agent-submitted job within a platform experiment.
type Experiment struct {
	ID                   string  `json:"id"`
	ParentID             *string `json:"parent_id,omitempty"`
	AgentID              string  `json:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id"`
	ProjectID            string  `json:"project_id"`
	// ClusterName is the target execution cluster this experiment is (or will be) admitted
	// onto (a Kubernetes cluster today, but domain has no dependency on that — it's whatever
	// unit the configured workload.Backend groups capacity by). Empty means "not yet
	// assigned" (pre-admission) or "default" for old rows / single-cluster deployments.
	ClusterName string `json:"cluster_name,omitempty"`
	CodeRef     string `json:"code_ref"`
	ConfigHash  string `json:"config_hash"`
	DataRef     string `json:"data_ref"`
	// Job is the agent's execution definition in the platform's own DSL — never a raw
	// execution-engine manifest (see JobSpec doc). GPUType/GPUCount below are the billing/
	// admission-facing totals derived from it once at submission time (Job.GPUCount is
	// per node; GPUType/GPUCount here are cached for cheap reads by scheduler/quota code
	// that doesn't need the rest of the job definition).
	Job JobSpec `json:"job"`
	// HypothesisID references a row registered via POST /registry/hypotheses. Required:
	// every experiment must test a specific, previously-registered hypothesis rather than
	// restating free text ad hoc — see Hypothesis doc.
	HypothesisID string `json:"hypothesis_id"`
	Hypothesis   string `json:"hypothesis"`
	Objective    string `json:"objective"`
	// Theory is the agent's specific prediction or bet they want to verify with this run.
	// Agents should check for duplicate submissions before submitting (GET /experiments?status=QUEUED&status=RUNNING).
	Theory string `json:"theory,omitempty"`
	// Summary is the post-run write-up written by the agent after completion or cancellation.
	// Populated via POST /experiments/{id}/summary. Helps other agents learn from this run.
	Summary                string  `json:"summary,omitempty"`
	GPUType                GPUType `json:"gpu_type"`
	GPUCount               int     `json:"gpu_count"`
	EstimatedDurationHours float64 `json:"estimated_duration_hours"`
	EstimatedCostT4H       float64 `json:"estimated_cost_t4h"` // cost in T4-GPU-hours
	// EstimatedCPUCoreHours/EstimatedRAMGBHours/EstimatedStorageGBHours mirror EstimatedCostT4H
	// for the 3 additional resource dimensions — zero when the platform experiment doesn't
	// track that dimension (see PlatformExperiment.BudgetCPUCoreHours etc).
	EstimatedCPUCoreHours   float64          `json:"estimated_cpu_core_hours,omitempty"`
	EstimatedRAMGBHours     float64          `json:"estimated_ram_gb_hours,omitempty"`
	EstimatedStorageGBHours float64          `json:"estimated_storage_gb_hours,omitempty"`
	CapacityTier            CapacityTier     `json:"capacity_tier"`
	NoveltyScore            float64          `json:"novelty_score,omitempty"` // computed at admission; advisory only
	PriorityScore           float64          `json:"priority_score"`
	Status                  ExperimentStatus `json:"status"`
	QueuedAt                *time.Time       `json:"queued_at,omitempty"`
	SubmittedAt             *time.Time       `json:"submitted_at,omitempty"`
	StartedAt               *time.Time       `json:"started_at,omitempty"`
	PreemptCount            int              `json:"preempt_count"`
	EvictionReason          string           `json:"eviction_reason,omitempty"`
	// NotAdmittedReason explains why a QUEUED job wasn't admitted on its most recent skipped
	// tick (see NotAdmittedCapacityUnavailable etc.) — stale/meaningless once the job leaves
	// QUEUED. Pre-admission counterpart to EvictionReason.
	NotAdmittedReason    string    `json:"not_admitted_reason,omitempty"`
	ActualDurationHours  *float64  `json:"actual_duration_hours,omitempty"`
	ActualCostT4H        *float64  `json:"actual_cost_t4h,omitempty"`
	ActualCPUCoreHours   *float64  `json:"actual_cpu_core_hours,omitempty"`
	ActualRAMGBHours     *float64  `json:"actual_ram_gb_hours,omitempty"`
	ActualStorageGBHours *float64  `json:"actual_storage_gb_hours,omitempty"`
	Artifacts            []string  `json:"artifacts"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

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
	// non-GPU resources each node gets. Empty means "use the operator's per-cluster default".
	// Storage is ephemeral scratch space (k8s ephemeral-storage) — not a persistent volume.
	CPU     string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty" yaml:"memory,omitempty"`
	Storage string `json:"storage,omitempty" yaml:"storage,omitempty"`

	// GPUType is the hardware tier this run is billed against (T4/L40/A100/H100/H200).
	GPUType GPUType `json:"gpu_type" yaml:"gpu_type"`
	// GPUCount is GPUs requested per node (not the job total — see TotalGPUs).
	GPUCount int `json:"gpu_count" yaml:"gpu_count"`
	// AcceptableGPUTypes, if set, lets the run land on any of these hardware tiers
	// interchangeably (e.g. H100 or H200) instead of requiring exactly GPUType. The rate
	// charged is still adjusted to whichever tier it actually lands on.
	AcceptableGPUTypes []GPUType `json:"acceptable_gpu_types,omitempty" yaml:"acceptable_gpu_types,omitempty"`

	// NumNodes is how many identical nodes this job spans. 1 (default) is a plain
	// single-node job; >1 requests a distributed run — the backend handles rank
	// assignment, per-node retry isolation, and completion gated on rank 0 automatically,
	// with no further DSL needed from the agent.
	NumNodes int `json:"num_nodes,omitempty" yaml:"num_nodes,omitempty"`
	// MaxRetries is how many times a failing node is retried before the job is marked
	// failed. Empty means "use the operator's per-cluster default".
	MaxRetries *int `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`

	// Topology controls where a distributed (NumNodes > 1) job's nodes land relative to
	// each other. Only meaningful when NumNodes > 1; ignored otherwise. See TopologySpec.
	Topology *TopologySpec `json:"topology,omitempty" yaml:"topology,omitempty"`

	// ShmSize sets the size of /dev/shm (e.g. "4Gi"). k8s defaults /dev/shm to a tiny
	// tmpfs, which silently breaks PyTorch's multiprocess DataLoader workers and NCCL's
	// shared-memory IPC — both need real shared memory for multi-GPU/multi-process
	// training. Empty means "use the operator's per-cluster default".
	ShmSize string `json:"shm_size,omitempty" yaml:"shm_size,omitempty"`

	// ExtraResources requests any k8s extended resource GPUType/GPUCount doesn't cover —
	// TPUs (google.com/tpu), other AI accelerators (habana.ai/gaudi, aws.amazon.com/neuron,
	// ...), or anything else a device plugin advertises — as plain quantity strings per node
	// (e.g. {"google.com/tpu": "8"}), same convention as CPU/Memory/Storage. These are NOT
	// billed or capped by the quota system today (no rate/cap exists for an open-ended
	// resource-name map, unlike the fixed GPU/CPU/RAM/storage dimensions) — they pass straight
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
	// host — the whole point of NumNodes > 1 is more physical GPUs, so two ranks sharing a
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

// TotalGPUs returns the job's total GPU footprint across all nodes (GPUCount × NumNodes),
// which is what admission/quota/eviction actually reserve and bill against.
func (j JobSpec) TotalGPUs() int {
	return j.GPUCount * j.Nodes()
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

// ElapsedHours returns hours since StartedAt, or 0 if not started.
func (e *Experiment) ElapsedHours() float64 {
	if e.StartedAt == nil {
		return 0
	}
	return time.Since(*e.StartedAt).Hours()
}

// CompletionFraction returns elapsed/estimated clamped to [0,1].
func (e *Experiment) CompletionFraction() float64 {
	if e.EstimatedDurationHours <= 0 {
		return 0
	}
	f := e.ElapsedHours() / e.EstimatedDurationHours
	return math.Max(0, math.Min(1, f))
}

// RemainingEstimatedHours returns the estimated hours still to run, floored at MinRemainingHours.
func (e *Experiment) RemainingEstimatedHours() float64 {
	r := e.EstimatedDurationHours - e.ElapsedHours()
	return math.Max(MinRemainingHours, r)
}

// GPUHours returns the total GPU-hours requested (gpu_count × estimated_duration_hours).
// Used as a scheduling tiebreak: prefer jobs with smaller total GPU-hours (shorter or fewer GPUs).
func (e *Experiment) GPUHours() float64 {
	return float64(e.GPUCount) * e.EstimatedDurationHours
}

// RequestedCPUCores returns the CPU cores this experiment requests, derived from
// estimated_cpu_core_hours / estimated_duration_hours. Zero for GPU jobs or when duration is
// unset. Used by the admission loop's live CPU-capacity check for CPU-only jobs.
func (e *Experiment) RequestedCPUCores() float64 {
	if e.EstimatedDurationHours <= 0 {
		return 0
	}
	return e.EstimatedCPUCoreHours / e.EstimatedDurationHours
}

// Agent represents an autonomous research agent interacting with the platform.
type Agent struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	PerformanceScore float64   `json:"performance_score"`
	Top3Count        int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt        time.Time `json:"created_at"`
}

// AgentBalance is an agent's all-time credit_ledger balance (sum of every entry ever
// appended for them). Global per-agent budgets are superseded by per-platform-experiment
// quotas (see quota.PlatformExperimentsService) — nothing currently appends to
// credit_ledger, so balance is always 0 today; this exists so the agents dashboard has a
// live (if currently empty) endpoint instead of a 404, and starts reflecting real numbers
// the moment anything writes to the ledger again.
type AgentBalance struct {
	AgentID          string  `json:"agent_id"`
	Balance          float64 `json:"balance"`
	PerformanceBonus float64 `json:"performance_bonus"`
	ExperienceBonus  float64 `json:"experience_bonus"`
	BorrowingUsed    float64 `json:"borrowing_used"`
}

// CreditLedgerEntry records a single compute transaction for an agent.
type CreditLedgerEntry struct {
	ID                   string    `json:"id"`
	AgentID              string    `json:"agent_id"`
	Amount               float64   `json:"amount"` // positive = credit, negative = debit (in T4h)
	Reason               string    `json:"reason"` // allocation | spend | refund
	ExperimentID         *string   `json:"experiment_id,omitempty"`
	PlatformExperimentID *string   `json:"platform_experiment_id,omitempty"`
	Period               int       `json:"period"`
	CreatedAt            time.Time `json:"created_at"`
}

// MetricDataPoint is a single metric observation during job execution.
type MetricDataPoint struct {
	ExperimentID     string    `json:"experiment_id"`
	MetricName       string    `json:"metric_name"` // Prometheus metric key
	FractionComplete float64   `json:"fraction_complete"`
	MetricValue      float64   `json:"metric_value"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// MetricSeriesPoint is one (time, value) sample within an AgentMetricSeries.
type MetricSeriesPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// AgentMetricSeries is one competing job's full metric history within a platform
// experiment — the unit the leaderboard/competition dashboard plots one line per.
type AgentMetricSeries struct {
	AgentID      string              `json:"agent_id"`
	ExperimentID string              `json:"experiment_id"`
	MetricName   string              `json:"metric_name"`
	Points       []MetricSeriesPoint `json:"points"`
}

// DonationRequest is an agent's public ask for extra compute from peers.
type DonationRequest struct {
	ID                   string    `json:"id"`
	AgentID              string    `json:"agent_id"`
	AgentName            string    `json:"agent_name,omitempty"`
	PlatformExperimentID string    `json:"platform_experiment_id"`
	CreditsWant          float64   `json:"credits_want"`
	Reason               string    `json:"reason"`
	Status               string    `json:"status"` // open | fulfilled | cancelled
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// EvictionReason classifies why a job was terminated early.
type EvictionReason string

const (
	EvictionSilent           EvictionReason = "silent"
	EvictionOverrun          EvictionReason = "overrun"
	EvictionCrashLoop        EvictionReason = "crash_loop"
	EvictionQuotaExhaustion  EvictionReason = "quota_exhaustion"
	EvictionExperimentClosed EvictionReason = "experiment_closed"
	EvictionAgentRemoved     EvictionReason = "agent_removed"
	EvictionCancelled        EvictionReason = "cancelled"
	EvictionPhase2Hold       EvictionReason = "phase2_hold"
	EvictionMetricDecline    EvictionReason = "metric_decline"
	// EvictionStuckPending marks a job that was admitted (SUBMITTED/ADMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
)

// NotAdmittedReason classifies why a QUEUED job wasn't admitted on its most recent skipped
// tick — updated by Loop.tick each time it skips a job, cleared on admission.
const (
	// NotAdmittedCapacityUnavailable: no capacity for this flavor existed at all at the start
	// of the tick (even before any of this tick's own admissions), and — for guaranteed jobs —
	// preempting all same-flavor burst jobs still wasn't enough.
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	// NotAdmittedOutranked: capacity for this flavor existed at the start of the tick, but
	// other (higher-ranked) jobs in the same tier admitted ahead of this one already consumed
	// it this tick.
	NotAdmittedOutranked = "outranked"
	// NotAdmittedSummaryGate: the agent has a COMPLETED experiment without a summary — all of
	// its other jobs are held until one is written.
	NotAdmittedSummaryGate = "summary_gate"
)

// SchedulingWeights controls how the priority components are combined.
type SchedulingWeights struct {
	W1Novelty        float64 `json:"w1_novelty"`
	W3CostEfficiency float64 `json:"w3_cost_efficiency"`
}

// DefaultSchedulingWeights returns the canonical scheduling weight configuration.
func DefaultSchedulingWeights() SchedulingWeights {
	return SchedulingWeights{
		W1Novelty:        0.5,
		W3CostEfficiency: 0.4,
	}
}

// QuotaConfig holds tunable constants for quota allocation.
type QuotaConfig struct {
	// Top3BonusFraction is the fraction of base_share added for top-3 placement in any past experiment.
	Top3BonusFraction float64 `json:"top3_bonus_fraction"`
	// BurstFraction is burst_quota = guaranteed * burst_fraction.
	BurstFraction float64 `json:"burst_fraction"`
	// Phase1ExploreFraction is the fraction of total budget allocated as initial per-agent
	// quota (the explore window). Matches the phase-2 boundary fraction.
	Phase1ExploreFraction float64 `json:"phase1_explore_fraction"`
	// MaxSubmissionsPerHour caps how many experiments an agent may submit per hour within
	// a single platform experiment. 0 means unlimited.
	MaxSubmissionsPerHour int `json:"max_submissions_per_hour"`
	// MetricDeclineFraction triggers metric-decline eviction when a job's primary metric
	// has been declining for this fraction of its estimated_duration_hours (e.g. 0.3 = 30%).
	MetricDeclineFraction float64 `json:"metric_decline_fraction"`

	// MaxGPUCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap a single
	// job's total resource request (per node × num_nodes), checked at admission before any
	// quota debit. 0 means unlimited. See config.QuotaConfig for the operator-facing doc.
	MaxGPUCountPerJob  int     `json:"max_gpu_count_per_job,omitempty"`
	MaxCPUCoresPerJob  float64 `json:"max_cpu_cores_per_job,omitempty"`
	MaxRAMGBPerJob     float64 `json:"max_ram_gb_per_job,omitempty"`
	MaxStorageGBPerJob float64 `json:"max_storage_gb_per_job,omitempty"`
}

// DefaultQuotaConfig returns fallback quota constants used in tests and local runs
// without a config file. Production services load these from settings/openresearch.yaml.
func DefaultQuotaConfig() QuotaConfig {
	return QuotaConfig{
		Top3BonusFraction:     0.25,
		BurstFraction:         2.0,
		Phase1ExploreFraction: Phase1ExploreFraction,
		MaxSubmissionsPerHour: 100,
		MetricDeclineFraction: 0.3,
	}
}

// CreditConfig is an alias kept for backward compatibility.
type CreditConfig = QuotaConfig

// DefaultCreditConfig returns DefaultQuotaConfig.
func DefaultCreditConfig() QuotaConfig { return DefaultQuotaConfig() }

// ExperimentFilter constrains which experiments are returned from the store.
// It is the unified filter type used by all services; unused fields are ignored.
type ExperimentFilter struct {
	AgentID              string
	ProjectID            string
	PlatformExperimentID string
	HypothesisID         string
	Status               ExperimentStatus
	Since                time.Time
	Limit                int
}

// Hypothesis is a registered research claim an agent intends to test. Agents register (or
// retrieve, if an equivalent one already exists — see NormalizeHypothesisText) a Hypothesis
// before submitting an experiment against it; ExperimentMeta.HypothesisID is required and
// must reference a row here. This is the real uniqueness check: a DB-level UNIQUE index on
// the normalized text (see schema.sql) rejects a second registration of the same claim,
// replacing the old always-novel dedup stub.
type Hypothesis struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// NormalizeHypothesisText collapses a hypothesis statement to a canonical form (lowercased,
// whitespace-collapsed, trimmed) so that trivially-different phrasings of the same claim
// ("Model X improves accuracy" vs "model x improves accuracy.") collide on the same
// registered hypothesis instead of creating near-duplicate rows.
func NormalizeHypothesisText(text string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	return strings.Join(fields, " ")
}
