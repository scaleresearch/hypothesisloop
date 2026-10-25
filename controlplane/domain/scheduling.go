package domain

import "time"

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

// SubmissionWindow is the span QuotaConfig.MaxSubmissionsPerHour counts an agent's submissions over.
const SubmissionWindow = time.Hour

// DefaultBudgetReference is the accelerator budgets are counted in when the settings name none.
const DefaultBudgetReference = "H100"

// QuotaConfig holds tunable constants for quota allocation.
type QuotaConfig struct {
	// Top3BonusFraction is the fraction of base_share added for top-3 placement in any past experiment.
	Top3BonusFraction float64 `json:"top3_bonus_fraction"`
	// BurstFraction is burst_quota = guaranteed * burst_fraction.
	BurstFraction float64 `json:"burst_fraction"`
	// BudgetReference names the accelerator an hour of which is one accelerator-hour.
	BudgetReference string `json:"budget_reference,omitempty"`
	// DefaultReportIntervalSeconds is the reporting cadence of a platform experiment created
	// without its own.
	DefaultReportIntervalSeconds int `json:"default_report_interval_seconds,omitempty"`
	// DefaultStages is the elimination ladder applied to a platform experiment created
	// without its own.
	DefaultStages []Stage `json:"default_stages"`
	// DefaultQuotaTier is the default_quota_tier of a platform experiment created without its own.
	DefaultQuotaTier QuotaTier `json:"default_quota_tier,omitempty"`
	// MaxRunLength is the longest wall clock a platform experiment may have, from its starts_at to its end.
	MaxRunLength time.Duration `json:"max_run_length,omitempty"`
	// BaselineLimit is the longest a platform experiment may stay open before the platform
	// closes it: the time it has to get its baseline declared and its run started. One created
	// whole has all of it, and a draft the minutes its plan gives, at most this.
	BaselineLimit time.Duration `json:"baseline_limit,omitempty"`
	// MinBaseline is the least a draft's plan may give its baseline.
	MinBaseline time.Duration `json:"min_baseline,omitempty"`
	// AgentsDue is how long a started platform experiment whose executors the platform runs
	// waits for the first one's machine to be running before the platform closes it: the time
	// one machine and every restart of it may take to come up, and never past its end.
	AgentsDue time.Duration `json:"agents_due,omitempty"`
	// MaxSubmissionsPerHour caps how many submissions an agent may make within a single platform
	// experiment in any SubmissionWindow, each the jobs of one hypothesis on one commit.
	MaxSubmissionsPerHour int `json:"max_submissions_per_hour"`

	// MaxAcceleratorCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap a job's
	// total resource request (per node × num_nodes) at admission, before any quota debit.
	// See config.QuotaConfig for the operator-facing doc.
	MaxAcceleratorCountPerJob int     `json:"max_accelerator_count_per_job,omitempty"`
	MaxCPUCoresPerJob         float64 `json:"max_cpu_cores_per_job,omitempty"`
	MaxRAMGBPerJob            float64 `json:"max_ram_gb_per_job,omitempty"`
	MaxStorageGBPerJob        float64 `json:"max_storage_gb_per_job,omitempty"`

	// DefaultMaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING) per
	// platform experiment when it does not set its own MaxConcurrentAccelerators. See
	// config.QuotaConfig for the operator-facing doc.
	DefaultMaxConcurrentAccelerators int `json:"default_max_concurrent_accelerators,omitempty"`

	// DefaultModelBudgetUSD is the model budget of a platform experiment that states none, and
	// MaxModelBudgetUSD the most one may state.
	DefaultModelBudgetUSD float64 `json:"default_model_budget_usd,omitempty"`
	MaxModelBudgetUSD     float64 `json:"max_model_budget_usd,omitempty"`
}
