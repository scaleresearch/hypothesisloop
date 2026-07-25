package domain

import (
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
	// ClusterName is the target execution cluster this experiment is (or will be) admitted onto
	// (a k8s cluster today, but domain has no dependency on that). Empty means "not yet
	// assigned" or "default" for old rows / single-cluster deployments.
	ClusterName string `json:"cluster_name,omitempty"`
	CodeRef     string `json:"code_ref"`
	ConfigHash  string `json:"config_hash"`
	DataRef     string `json:"data_ref"`
	// Job is the agent's execution definition in the platform's own DSL — never a raw
	// execution-engine manifest (see JobSpec doc). AcceleratorType/AcceleratorCount below are the
	// billing/admission-facing canonical values derived from it once at submission time.
	Job JobSpec `json:"job"`
	// HypothesisID references a row registered via POST /registry/hypotheses. Required: every
	// experiment must test a specific, previously-registered hypothesis, not free text ad hoc.
	HypothesisID string `json:"hypothesis_id"`
	Hypothesis   string `json:"hypothesis"`
	Objective    string `json:"objective"`
	// Theory is the agent's specific prediction or bet they want to verify with this run.
	// Agents should check for duplicate submissions before submitting (GET /experiments?status=QUEUED&status=RUNNING).
	Theory                 string          `json:"theory,omitempty"`
	AcceleratorType        AcceleratorType `json:"accelerator_type"`
	AcceleratorCount       int             `json:"accelerator_count"`
	EstimatedDurationHours float64         `json:"estimated_duration_hours"`
	EstimatedCostAccH      float64         `json:"estimated_cost_acch"` // cost in accelerator-hours (AccH), H100-equivalent
	// EstimatedCPUCoreHours/EstimatedRAMGBHours/EstimatedStorageGBHours mirror EstimatedCostAccH
	// for the 3 additional dimensions — zero when the platform experiment doesn't track it.
	EstimatedCPUCoreHours   float64          `json:"estimated_cpu_core_hours,omitempty"`
	EstimatedRAMGBHours     float64          `json:"estimated_ram_gb_hours,omitempty"`
	EstimatedStorageGBHours float64          `json:"estimated_storage_gb_hours,omitempty"`
	CapacityTier            CapacityTier     `json:"capacity_tier"`
	NoveltyScore            float64          `json:"novelty_score,omitempty"` // computed at admission; advisory only
	PriorityScore           float64          `json:"priority_score"`
	Status                  ExperimentStatus `json:"status"`
	QueuedAt                *time.Time       `json:"queued_at,omitempty"`
	SubmittedAt             *time.Time       `json:"submitted_at,omitempty"`
	EvictionReason          string           `json:"eviction_reason,omitempty"`
	// NotAdmittedReason is the scheduler's current decision explaining why a QUEUED job was
	// skipped. Overwritten each decision and cleared on admission; not tick history.
	NotAdmittedReason string   `json:"not_admitted_reason,omitempty"`
	Artifacts         []string `json:"artifacts"`
	// QuotaSettledAt is set once this (terminal) experiment's final usage has been durably
	// written to the metrics DB. nil means settlement is pending — the signal a background
	// reconciler uses to retry after a crash or outage. Meaningless for non-terminal experiments.
	QuotaSettledAt *time.Time `json:"quota_settled_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// RequestedCPUCores returns CPU cores requested, derived from estimated_cpu_core_hours /
// estimated_duration_hours. Zero for accelerator jobs or unset duration. Used by the admission
// loop's live CPU-capacity check.
func (e *Experiment) RequestedCPUCores() float64 {
	if e.EstimatedDurationHours <= 0 {
		return 0
	}
	return e.EstimatedCPUCoreHours / e.EstimatedDurationHours
}

// Footprint returns e's physical resource footprint in canonical units, across every dimension
// with live per-cluster capacity reporting: CPU (millicores), memory/storage (bytes) — read
// straight from e.Job's own quantity strings, scaled by e.Job.Nodes() — plus its accelerator, if
// any, keyed by the accelerator type string itself — the same driver-published "key=value" the
// agent submitted and that capacity reports, so the two sides join with no translation step.
//
// The accelerator dimension deliberately uses e.AcceleratorType/e.AcceleratorCount, not
// e.Job.AcceleratorType/e.Job.AcceleratorCount (the originally requested type before
// substitution): once a job lands on a different AcceptableAcceleratorTypes flavor than
// requested, admission rewrites e.AcceleratorType to the flavor it actually holds, and
// e.AcceleratorCount is already the job's TOTAL footprint (see JobSpec.TotalAccelerators) — so
// this always reports what a RUNNING job really holds, not what it originally asked for.
//
// A malformed CPU/memory/storage quantity string should never reach this point (ValidateExperiment
// parses all three at submission); if one does, that dimension is simply omitted rather than
// erroring, since every caller treats Footprint() as unconditional (not (Footprint, error)).
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
	for name, qty := range e.Job.ExtraResources {
		if q, err := resource.ParseQuantity(qty); err == nil {
			fp.Add(ResourceKey{Kind: ResourceKindExtended, Flavor: name}, q.Value())
		}
	}
	fp = fp.Scale(int64(e.Job.Nodes()))
	if e.AcceleratorCount > 0 && e.AcceleratorType != "" {
		fp.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: string(e.AcceleratorType)}, int64(e.AcceleratorCount))
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
