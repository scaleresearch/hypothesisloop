package domain

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// TriedCluster records one placement of a job that the environment failed: the cluster it was on
// (by runtime-derived cluster_id, never cluster_name), when, and the code of the eviction reason
// that ended it. Reason is empty on entries written before it was kept, which were all scale-up
// failovers. See Experiment.TriedClusters.
type TriedCluster struct {
	ClusterID string    `json:"cluster_id"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason,omitempty"`
}

// Experiment is an agent-submitted job within a platform experiment.
type Experiment struct {
	ID                   string  `json:"id"`
	ParentID             *string `json:"parent_id,omitempty"`
	AgentID              string  `json:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id"`
	// Actor is whose credential submitted the job. AgentID is whose job it is and what it
	// spends from, which is another fact: a person or an API key may submit an agent's job.
	Actor Actor `json:"actor"`
	// ProjectID is the platform experiment's project, read with the job and stored there only.
	ProjectID string `json:"project_id"`
	// OrgID is the org of that project, read with the job and stored there only. Admission
	// places a job on a cluster of this org or on a shared one.
	OrgID string `json:"org_id"`
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
	// ResolvedJob is Job as the runtime must run it, written once at admission onto the specific
	// cluster the job landed on: any MaxResourceSentinel ("max") cpu/memory/storage resolved to a
	// literal, and the shutdown and checkpoint grace resolved against the operator's limits (see
	// GraceLimits.Resolve). Nil until admitted. Job itself is NEVER rewritten, so a GET always shows exactly what
	// was submitted; every downstream consumer that needs the runnable/executable values (cost
	// estimation, BuildJobs, Footprint, NodeShapes) must call EffectiveJob() instead of reading
	// Job directly.
	ResolvedJob *JobSpec `json:"resolved_job,omitempty"`
	// HypothesisID references a row registered via POST /hypotheses. Required: every
	// experiment must test a specific, previously-registered hypothesis, not free text ad hoc.
	HypothesisID string `json:"hypothesis_id"`
	Hypothesis   string `json:"hypothesis"`
	// Theory is the agent's specific prediction or bet they want to verify with this run.
	// Agents should check for duplicate submissions before submitting (GET /experiments?status=QUEUED&status=RUNNING).
	Theory                 string           `json:"theory,omitempty"`
	AcceleratorType        AcceleratorType  `json:"accelerator_type"`
	AcceleratorCount       int              `json:"accelerator_count"`
	EstimatedDurationHours float64          `json:"estimated_duration_hours"`
	EstimatedCostAccH      float64          `json:"estimated_cost_acch"` // cost in accelerator-hours (AccH), H100-equivalent
	CapacityTier           CapacityTier     `json:"capacity_tier"`
	NoveltyScore           float64          `json:"novelty_score,omitempty"` // computed at admission; advisory only
	PriorityScore          float64          `json:"priority_score"`
	Status                 ExperimentStatus `json:"status"`
	QueuedAt               *time.Time       `json:"queued_at,omitempty"`
	SubmittedAt            *time.Time       `json:"submitted_at,omitempty"`
	EvictionReason         string           `json:"eviction_reason,omitempty"`
	// NotAdmittedReason is the scheduler's current decision explaining why a QUEUED job was
	// skipped. Overwritten each decision and cleared on admission; not tick history.
	NotAdmittedReason string `json:"not_admitted_reason,omitempty"`
	// PhaseDetail is the runtime's latest explanation for why this job's container hasn't
	// started or has been restarting, merged in live from the metrics store on every read —
	// never persisted to PostgreSQL (metrics only in the metrics store, important.md #3). Nil
	// when nothing has been reported yet.
	PhaseDetail *PhaseDetail `json:"phase_detail,omitempty"`
	// Metrics is what the job reported on each declared metric of its platform experiment, in
	// declared order, merged in live from the metrics store like PhaseDetail. Metric is the one on
	// the first ranking metric, nil until the job reports it.
	Metrics []JobMetric `json:"metrics,omitempty"`
	Metric  *JobMetric  `json:"metric,omitempty"`
	// Summary is the write-up the job's agent filed for it. Set on a read of one job only.
	Summary string `json:"summary,omitempty"`
	// Data is the job's durable-data addressing and key, assembled on the way out to a runtime.
	// Nil on every other read of an experiment.
	Data *DataAccess `json:"data,omitempty"`
	// Token is the job's own platform token, good for reporting its metrics and nothing else.
	// Like Data it is assembled on the way out to a runtime and absent on every other read.
	Token string `json:"token,omitempty"`
	// DataKey is the job's permanent object-store key, created at submission and stored with
	// the row. Never serialized: only Data carries it, and only to the runtime.
	DataKey DataKey `json:"-"`
	// SecretEnv is the secret_env the job was submitted with, stored with the row and apart from
	// Job. Never serialized: ForRuntime alone puts it back on the spec, for the runtime.
	SecretEnv map[string]string `json:"-"`
	// QuotaSettledAt is set once this (terminal) experiment's final usage has been durably
	// written to the metrics DB. nil means settlement is pending — the signal a background
	// reconciler uses to retry after a crash or outage. Meaningless for non-terminal experiments.
	// AttemptCount is how many attempts of this experiment have already run and ended; 0 on
	// the first. A gang retry and an infrastructure requeue both write it. A single-pod job's
	// own-failure retries are the runtime's BackoffLimit and are invisible here, because for a
	// one-pod job the runtime can express "restart the failed unit" itself — for an N-pod gang
	// it cannot, so the control plane makes that decision and writes it as desired state.
	//
	// It is load-bearing beyond counting: a requeue reuses the experiment id, so the rebuilt
	// workload is byte-identical to the terminal one still sitting in the cluster unless
	// something in desired state changes. The runtimes put this number in the job's environment
	// (HYPOTHESISLOOP_ATTEMPT) for exactly that reason, which is why every requeue advances it
	// and only the agent's own failures are charged against its budget.
	AttemptCount int `json:"attempt_count"`
	// InfraRequeueCount is how many of AttemptCount's attempts ended in an infrastructure fault
	// and were requeued for free. The agent's retry allowance is the difference of the two
	// (RetriesUsed): AttemptCount is the generation counter that makes each attempt a distinct
	// desired state, so it must advance on every requeue, while the budget must not.
	InfraRequeueCount int `json:"infra_requeue_count"`
	// TriedClusters is this job's own history of placements the environment failed: every
	// cluster (by runtime-derived cluster_id) it was requeued from for an infrastructure fault, a
	// scale-up that timed out or was refused included, with the time it was appended. Placement
	// leaves out any entry still within scheduler.tried_cluster_ttl_seconds, which is a delay and
	// no bound: what bounds the attempts is InfraRequeueCount, spent by the same write that
	// appends here. Never cleared, only grown; stale entries simply age out of the TTL check.
	//
	// Kept out of every answer of the API: a cluster's id is the name its settings are kept
	// under, and a job tried on a shared cluster would show its org the id of a cluster another
	// org owns. No client reads the list.
	TriedClusters []TriedCluster `json:"-"`

	QuotaSettledAt *time.Time `json:"quota_settled_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// FinishedAt is when the job reached its terminal status, nil while it can still run. Nothing
	// writes updated_at on a terminal row but the transition that ended it, a requeue aside,
	// which makes the row non-terminal again.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// EffectiveJob returns e.Job as it should actually be run: e's own ResolvedJob if admission has
// already resolved one, otherwise e.Job verbatim (a never-admitted experiment, or one with
// nothing to resolve). Every consumer of the runnable/executable job shape (cost estimation,
// BuildJobs, Footprint, NodeShapes) must read this instead of e.Job directly, so a "max" sentinel
// resolved at admission is honored everywhere the job actually runs while GET still shows exactly
// what was submitted.
func (e *Experiment) EffectiveJob() JobSpec {
	if e.ResolvedJob != nil {
		return *e.ResolvedJob
	}
	return e.Job
}

// ForRuntime is the job as a cluster's runtime is handed it: what a runtime builds a workload
// from, and nothing it has no use for. A runtime may be another org's, so the hypothesis, the
// objective, the theory and where the job sits in its org stay with the platform. The fields
// kept are the ones the executors and the workload environment read
// (runtime/shared/workloadenv), under the names they already decode. This is the one place the
// job's secret_env leaves the platform: on the spec, where the runtime reads the rest of the
// job's environment.
func (e *Experiment) ForRuntime() *Experiment {
	job := e.Job
	job.SecretEnv = e.SecretEnv
	resolved := e.ResolvedJob
	if resolved != nil {
		withSecrets := *resolved
		withSecrets.SecretEnv = e.SecretEnv
		resolved = &withSecrets
	}
	return &Experiment{
		ID:                     e.ID,
		AgentID:                e.AgentID,
		ProjectID:              e.ProjectID,
		CodeRef:                e.CodeRef,
		ConfigHash:             e.ConfigHash,
		DataRef:                e.DataRef,
		Job:                    job,
		ResolvedJob:            resolved,
		AcceleratorType:        e.AcceleratorType,
		AcceleratorCount:       e.AcceleratorCount,
		EstimatedDurationHours: e.EstimatedDurationHours,
		CapacityTier:           e.CapacityTier,
		AttemptCount:           e.AttemptCount,
		Data:                   e.Data,
		Token:                  e.Token,
	}
}

// NodeShapes is the placement view of e's EffectiveJob — see JobSpec.NodeShapes. It is on
// Experiment, not JobSpec, specifically so a resolved "max" value is reflected in what the
// scheduler thinks the job's footprint actually is, never the original possibly-"max" spec.
func (e *Experiment) NodeShapes() []NodeShape {
	return e.EffectiveJob().NodeShapes()
}

// RetriesUsed is how much of the agent's max_retries allowance this experiment has spent: every
// attempt so far minus the ones the environment, not the agent, ended. This is the figure
// RequeueForRetry compares against max_retries, computed the same way in SQL so the read and the
// write cannot disagree.
func (e *Experiment) RetriesUsed() int {
	return e.AttemptCount - e.InfraRequeueCount
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
// from e.EffectiveJob()'s quantity strings, so what admission resolved (a "max", or the slot a
// job holds whole) is what the job is counted as holding — plus its accelerator, if
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
	// Summed over the job's node groups, exactly as JobSpec.Footprint does. An ungrouped job has
	// one group of Nodes() identical replicas, so this is the per-node shape x Nodes() it has
	// always been; a grouped job's true demand is the sum of its groups, never a multiple of any
	// one of them.
	job := e.EffectiveJob()
	for _, group := range job.NodeGroups() {
		perNode := NewFootprint()
		if group.CPU != "" {
			if q, err := resource.ParseQuantity(group.CPU); err == nil {
				perNode.Add(ResourceKey{Kind: ResourceKindCPU}, q.MilliValue())
			}
		}
		if group.Memory != "" {
			if q, err := resource.ParseQuantity(group.Memory); err == nil {
				perNode.Add(ResourceKey{Kind: ResourceKindMemory}, q.Value())
			}
		}
		if group.Storage != "" {
			if q, err := resource.ParseQuantity(group.Storage); err == nil {
				perNode.Add(ResourceKey{Kind: ResourceKindStorage}, q.Value())
			}
		}
		for name, qty := range job.ExtraResources {
			if q, err := resource.ParseQuantity(qty); err == nil {
				perNode.Add(ResourceKey{Kind: ResourceKindExtended, Flavor: name}, q.Value())
			}
		}
		fp.AddFootprint(perNode.Scale(int64(group.Replicas)))
	}
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
	// Scope keeps the experiments of the platform experiments the caller may see.
	Scope                TenantScope
	AgentID              string
	PlatformExperimentID string
	HypothesisID         string
	Status               ExperimentStatus
	Since                time.Time
	// Search matches against hypothesis/theory text, case-insensitively, substring
	// match (mirrors PlatformExperimentsFilter.Search).
	Search string
	// NeedsSummary selects only COMPLETED experiments with no finding filed against them -- the
	// exact set the admission summary gate blocks on. See db.UnsummarizedCompletedPredicate.
	NeedsSummary bool
	Limit        int
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
	// EvictionsByClass folds EvictionsByReason into whose fault each was (see FaultClass), so a
	// reader can tell an agent's own failures from a bad node and from the platform's own
	// decisions without knowing the fifteen reason codes. Derived here at read time from the
	// same tally, never stored: a class is a pure function of a reason.
	EvictionsByClass map[string]int `json:"evictions_by_class"`
	// InFlightAccelerators and InFlightCores sum accelerator_count over SUBMITTED and RUNNING,
	// the first for jobs on an accelerator type and the second for jobs on cores (CPUType):
	// devices and cores are never added into one number. The
	// max_concurrent_accelerators cap is checked against the two together.
	InFlightAccelerators int `json:"in_flight_accelerators"`
	InFlightCores        int `json:"in_flight_cores"`
	// Groups holds the same stats once per value of the requested group field, keyed by that
	// value. Absent when no grouping was asked for.
	Groups map[string]*ExperimentStats `json:"groups,omitempty"`
}

// ExperimentStatsGroupFields are the fields ExperimentStats can be split by, mapped to their
// SQL column.
var ExperimentStatsGroupFields = map[string]string{
	"agent":      "agent_id",
	"hypothesis": "hypothesis_id",
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

// Placement is where an experiment's workload is supposed to be running and which generation of
// it is current — what a cluster's status report is fenced against. All of it must come from one
// row read: judged against a cluster from one read and an attempt from another, a report can be
// accepted for a generation that no longer exists.
type Placement struct {
	ClusterName  string
	AttemptCount int
	// Slot is the slot the job was admitted to (NodeSlotLabel), "" on a cluster without slots.
	Slot string
}
