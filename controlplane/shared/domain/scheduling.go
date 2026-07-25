package domain

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

	// MaxAcceleratorCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap a job's
	// total resource request (per node × num_nodes) at admission, before any quota debit.
	// 0 means unlimited. See config.QuotaConfig for the operator-facing doc.
	MaxAcceleratorCountPerJob int     `json:"max_accelerator_count_per_job,omitempty"`
	MaxCPUCoresPerJob         float64 `json:"max_cpu_cores_per_job,omitempty"`
	MaxRAMGBPerJob            float64 `json:"max_ram_gb_per_job,omitempty"`
	MaxStorageGBPerJob        float64 `json:"max_storage_gb_per_job,omitempty"`
}

// DefaultQuotaConfig returns fallback quota constants used in tests and local runs
// without a config file. Production services load these from settings/hypothesisloop.yaml.
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
