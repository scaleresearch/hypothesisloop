package domain

import (
	"strings"
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
	// HypothesisID references a row registered via POST /hypotheses. Required: every
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
	NotAdmittedReason string `json:"not_admitted_reason,omitempty"`
	// PhaseDetail is the runtime's latest explanation for why this job's container hasn't
	// started or has been restarting, merged in live from the metrics store on every read —
	// never persisted to PostgreSQL (metrics only in the metrics store, important.md #3). Nil
	// when nothing has been reported yet.
	PhaseDetail *PhaseDetail `json:"phase_detail,omitempty"`
	Artifacts   []string     `json:"artifacts"`
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

// RatedCost is the single formula settlement and live running-cost accounting both use to bill
// any resource dimension: estimated's admission-time per-hour rate (estimated /
// EstimatedDurationHours) times hours actually observed. Using the admission-time estimate as the
// rate — rather than a live catalog lookup (domain.AcceleratorType.LookupCost) — keeps the two
// call sites priced identically for the life of the job: it's invariant across a preemption
// requeue's proportional rescale (see experiments_store_lifecycle.RequeuePreempted), and it can't
// be knocked out by an operator retiring the accelerator flavor from the rate table mid-run, since
// it never looks the flavor up again after admission. Zero for an unset/invalid duration.
func (e *Experiment) RatedCost(estimated, hours float64) float64 {
	if e.EstimatedDurationHours <= 0 {
		return 0
	}
	return (estimated / e.EstimatedDurationHours) * hours
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
		// Lowercased so Fits()'s exact-match join works regardless of casing (see
		// AcceleratorType.MatchesLabels and JobSpec.Footprint, which does the same).
		fp.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: strings.ToLower(string(e.AcceleratorType))}, int64(e.AcceleratorCount))
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
	// Search matches against hypothesis/objective/theory text, case-insensitively, substring
	// match (mirrors PlatformExperimentsFilter.Search).
	Search string
	// NeedsSummary selects only COMPLETED experiments with no finding filed against them -- the
	// exact set the admission summary gate blocks on. See db.UnsummarizedCompletedPredicate.
	NeedsSummary bool
	Limit  int
	// Offset skips this many matching rows before applying Limit — used for page-by-page
	// listing. Zero means "from the start".
	Offset int
	// Sort selects the ORDER BY column and direction, as "field" (ascending) or "-field"
	// (descending). Empty keeps the store's default order. See ExperimentSortFields for the
	// allowed field names.
	Sort string
}

// ExperimentSortFields are the column names ExperimentFilter.Sort may reference, mapped to
// their underlying SQL column. Keeping an explicit allowlist (rather than passing the query
// param straight into ORDER BY) is what makes Sort safe against SQL injection.
var ExperimentSortFields = map[string]string{
	"created_at":     "created_at",
	"updated_at":     "updated_at",
	"priority_score": "priority_score",
	"status":         "status",
}

// ExperimentStats is the whole-table shape of the experiment set matching a filter, counted in
// PostgreSQL rather than by reading the rows. A dashboard needs the totals, not the rows: every
// list read is bounded to one page, so counting client-side would silently report the page.
type ExperimentStats struct {
	Total int `json:"total"`
	// ByStatus counts every status present; a status with no rows is absent, not zero.
	ByStatus map[string]int `json:"by_status"`
	// ByCapacityTier counts guaranteed vs burst across all statuses, RunningByCapacityTier the
	// same restricted to RUNNING — how much of the live load is preemptable.
	ByCapacityTier        map[string]int `json:"by_capacity_tier"`
	RunningByCapacityTier map[string]int `json:"running_by_capacity_tier"`
	EvictedByCapacityTier map[string]int `json:"evicted_by_capacity_tier"`
	// EvictionsByReason is keyed by the reason *code* — the part before any ": detail"
	// explanation — so a reason carrying per-job detail stays one category.
	EvictionsByReason map[string]int `json:"evictions_by_reason"`
}

// ValidSortField reports whether sort names a field in allowed, ignoring a leading "-" for
// descending. Empty is valid and means the store's default order. Callers check this before
// querying: an unrecognized field otherwise falls back to the default silently, so a caller who
// mistyped one gets a differently-ordered page and no indication the sort was dropped.
func ValidSortField(sort string, allowed map[string]string) bool {
	if sort == "" {
		return true
	}
	_, ok := allowed[strings.TrimPrefix(sort, "-")]
	return ok
}
