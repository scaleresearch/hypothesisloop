package domain

import (
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

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
	Theory                 string  `json:"theory,omitempty"`
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
	// Attempt counts distinct Job creations for this experiment: starts at 1, and is bumped
	// by the control plane whenever a cluster-agent reports the Job gone while this
	// experiment is still desired-running — see ClusterQueueStore.BumpAttemptOnRecreate.
	Attempt        int    `json:"attempt"`
	EvictionReason string `json:"eviction_reason,omitempty"`
	// NotAdmittedReason explains why a QUEUED job wasn't admitted on its most recent skipped
	// tick (see NotAdmittedCapacityUnavailable etc.) — stale/meaningless once the job leaves
	// QUEUED. Pre-admission counterpart to EvictionReason.
	NotAdmittedReason    string   `json:"not_admitted_reason,omitempty"`
	ActualDurationHours  *float64 `json:"actual_duration_hours,omitempty"`
	ActualCostT4H        *float64 `json:"actual_cost_t4h,omitempty"`
	ActualCPUCoreHours   *float64 `json:"actual_cpu_core_hours,omitempty"`
	ActualRAMGBHours     *float64 `json:"actual_ram_gb_hours,omitempty"`
	ActualStorageGBHours *float64 `json:"actual_storage_gb_hours,omitempty"`
	Artifacts            []string `json:"artifacts"`
	// QuotaSettledAt is set once this (terminal) experiment's final observed usage has been
	// durably written to the metrics DB. nil means settlement is still pending/outstanding —
	// the sole signal a background reconciler needs to retry it after a crash or a metrics-DB
	// outage. Meaningless (left nil) for non-terminal experiments.
	QuotaSettledAt *time.Time `json:"quota_settled_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
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

// RequestedCPUCores returns the CPU cores this experiment requests, derived from
// estimated_cpu_core_hours / estimated_duration_hours. Zero for GPU jobs or when duration is
// unset. Used by the admission loop's live CPU-capacity check for CPU-only jobs.
func (e *Experiment) RequestedCPUCores() float64 {
	if e.EstimatedDurationHours <= 0 {
		return 0
	}
	return e.EstimatedCPUCoreHours / e.EstimatedDurationHours
}

// Footprint returns e's physical resource footprint in canonical units, across every dimension
// that now has real live per-cluster capacity reporting: CPU (millicores), memory/storage
// (bytes) — read straight from e.Job's own quantity strings (the canonical source of truth,
// not a second derived representation — see the plan's "no request columns" correction) and
// scaled by e.Job.Nodes() — plus its accelerator, if any (whole units, keyed by
// GPUType.FlavorName() to match capacity's own key convention).
//
// The accelerator dimension deliberately uses e.GPUType/e.GPUCount (the experiment's own
// top-level, admission-facing fields), not e.Job.GPUType/e.Job.GPUCount (the originally
// *requested* type before any substitution): once a job actually lands on a different
// AcceptableGPUTypes flavor than requested, UpdateAdmittedFlavor rewrites e.GPUType to the
// flavor it actually holds, and e.GPUCount is already the job's TOTAL footprint (per-node
// GPUCount x NumNodes, set once at submission — see JobSpec.TotalGPUs) — so this always reports
// which capacity a RUNNING job is really holding, not what it originally asked for.
//
// A malformed CPU/memory/storage quantity string should never reach this point (ValidateExperiment
// requires and parses all three at submission) — if one somehow does, that dimension is simply
// omitted here rather than erroring, since every caller of Footprint() treats it as an
// unconditional value, not (Footprint, error).
func (e *Experiment) Footprint() Footprint {
	fp := NewFootprint()
	if e.Job.CPU != "" {
		if q, err := resource.ParseQuantity(e.Job.CPU); err == nil {
			fp.Add(ResourceKey{Kind: ResourceKindCPU}, q.MilliValue())
		}
	}
	if e.Job.Memory != "" {
		if q, err := resource.ParseQuantity(e.Job.Memory); err == nil {
			fp.Add(ResourceKey{Kind: ResourceKindMemory}, q.Value())
		}
	}
	if e.Job.Storage != "" {
		if q, err := resource.ParseQuantity(e.Job.Storage); err == nil {
			fp.Add(ResourceKey{Kind: ResourceKindStorage}, q.Value())
		}
	}
	fp = fp.Scale(int64(e.Job.Nodes()))
	if e.GPUCount > 0 {
		fp.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: e.GPUType.FlavorName()}, int64(e.GPUCount))
	}
	return fp
}

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
