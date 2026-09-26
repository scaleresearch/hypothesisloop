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
	// DefaultReportIntervalSeconds is the reporting cadence of a platform experiment created
	// without its own.
	DefaultReportIntervalSeconds int `json:"default_report_interval_seconds,omitempty"`
	// DefaultStages is the elimination ladder applied to a platform experiment created
	// without its own.
	DefaultStages []Stage `json:"default_stages"`
	// MaxSubmissionsPerHour caps how many experiments an agent may submit per hour within
	// a single platform experiment. 0 means unlimited.
	MaxSubmissionsPerHour int `json:"max_submissions_per_hour"`

	// MaxAcceleratorCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap a job's
	// total resource request (per node × num_nodes) at admission, before any quota debit.
	// 0 means unlimited. See config.QuotaConfig for the operator-facing doc.
	MaxAcceleratorCountPerJob int     `json:"max_accelerator_count_per_job,omitempty"`
	MaxCPUCoresPerJob         float64 `json:"max_cpu_cores_per_job,omitempty"`
	MaxRAMGBPerJob            float64 `json:"max_ram_gb_per_job,omitempty"`
	MaxStorageGBPerJob        float64 `json:"max_storage_gb_per_job,omitempty"`

	// DefaultMaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING) per
	// platform experiment when it does not set its own MaxConcurrentAccelerators. See
	// config.QuotaConfig for the operator-facing doc.
	DefaultMaxConcurrentAccelerators int `json:"default_max_concurrent_accelerators,omitempty"`
}
