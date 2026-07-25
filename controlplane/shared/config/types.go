// Package config loads hypothesisloop.yaml and exposes a typed Config used across all services.
package config

// AcceleratorTypeConfig defines platform-wide accelerator identity and exchange rate.
type AcceleratorTypeConfig struct {
	Name     string  `yaml:"name"`
	AccHRate float64 `yaml:"acch_rate"`
}

// AcceleratorTypeNames returns every accelerator type the catalog prices — the driver-published
// "key=value" strings, which are also the only types capacity reports and admission accepts.
func (c *Config) AcceleratorTypeNames() []string {
	names := make([]string, 0, len(c.AcceleratorTypes))
	for _, t := range c.AcceleratorTypes {
		names = append(names, t.Name)
	}
	return names
}

// QuotaConfig holds scheduling tunable constants.
type QuotaConfig struct {
	Top3BonusFraction float64 `yaml:"top3_bonus_fraction"`
	BurstFraction     float64 `yaml:"burst_fraction"`
	// MaxSubmissionsPerHour caps how many experiments an agent may submit per hour
	// within a single platform experiment. 0 means unlimited.
	MaxSubmissionsPerHour int `yaml:"max_submissions_per_hour"`
	// MetricDeclineFraction triggers metric-decline eviction when a job's primary metric
	// has been monotonically declining for this fraction of its estimated_duration_hours.
	// E.g. 0.3 = evict after metrics decline for 30% of the job's estimated time.
	MetricDeclineFraction float64 `yaml:"metric_decline_fraction"`

	// MaxAcceleratorCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap how much of
	// each resource dimension a single job may request (per node × num_nodes total), on top
	// of the guaranteed/burst quota check — this is what actually prevents one absurd
	// submission from blowing through an entire budget in one debit. 0 means unlimited.
	// Operators must size these sanely relative to burst-pool sizing: a cap alone doesn't
	// replace correct budget sizing, it only bounds a single job's blast radius.
	MaxAcceleratorCountPerJob int     `yaml:"max_accelerator_count_per_job"`
	MaxCPUCoresPerJob         float64 `yaml:"max_cpu_cores_per_job"`
	MaxRAMGBPerJob            float64 `yaml:"max_ram_gb_per_job"`
	MaxStorageGBPerJob        float64 `yaml:"max_storage_gb_per_job"`
}

// SchedulerConfig holds timing and eviction tuning constants.
type SchedulerConfig struct {
	LoopHeartbeatSeconds         int     `yaml:"loop_heartbeat_seconds"`
	JobPollIntervalSeconds       int     `yaml:"job_poll_interval_seconds"`
	ReconcileIntervalSeconds     int     `yaml:"reconcile_interval_seconds"`
	DefaultReportIntervalSeconds int     `yaml:"default_report_interval_seconds"`
	SilenceMultiplier            float64 `yaml:"silence_multiplier"`
	// MinSilenceWindowSeconds floors silence_multiplier * report_interval_seconds so a platform
	// experiment configured with an aggressively short report interval can't produce a silence
	// window narrower than realistic node-death/reschedule recovery time — without this, a
	// pod that's mid-reschedule (not hung, just briefly quiet while k8s recreates it elsewhere)
	// gets permanently EVICTED before it ever gets a chance to resume reporting.
	MinSilenceWindowSeconds int     `yaml:"min_silence_window_seconds"`
	OverrunMultiplier       float64 `yaml:"overrun_multiplier"`
	MetricWindowSize        int     `yaml:"metric_window_size"`
	// JobDeadlineMultiplier sets the backend's hard deadline ceiling as a multiple of
	// estimated_duration_hours. Acts as the backstop for the overrun guard.
	JobDeadlineMultiplier float64 `yaml:"job_deadline_multiplier"`
	// MinJobDeadlineSeconds is the floor for ActiveDeadlineSeconds so very short jobs still
	// get a sane minimum wall-clock budget.
	MinJobDeadlineSeconds int `yaml:"min_job_deadline_seconds"`
	// StuckPendingTimeoutSeconds bounds how long a job may stay SUBMITTED/ADMITTED without ever
	// reporting RUNNING before job_watcher evicts it (reason stuck_pending) and fully refunds
	// its reservation, instead of waiting out the much longer native ActiveDeadlineSeconds.
	StuckPendingTimeoutSeconds int `yaml:"stuck_pending_timeout_seconds"`
	// ClusterUnreachableAfterSeconds bounds how stale a cluster-agent's heartbeat may be before
	// the cluster is treated as Unreachable: its live-reported capacity (see GetLiveCPUCapacity)
	// ages out of admission (freezing new jobs to it) and it shows Disconnected in the UI.
	// Jobs already RUNNING there are left alone until it reconnects — avoids duplicate dispatch
	// if it comes back. ~15 missed polls at the ~2s desired-state cadence.
	ClusterUnreachableAfterSeconds int `yaml:"cluster_unreachable_after_seconds"`
	// GuaranteedFairnessWindowSeconds quantizes queued_at into age buckets for the guaranteed
	// tier's admission ordering: jobs within the same bucket are ordered by least-used-quota
	// agent first instead of exact submission timestamp, bounding the latency-fairness gap a
	// steady-submitting agent would otherwise get from pure exact-FIFO. Same shape as every
	// other *_seconds field here: 0/unset falls back to the default below, not "disabled".
	GuaranteedFairnessWindowSeconds int `yaml:"guaranteed_fairness_window_seconds"`
	// StaleDesiredStateSweepIntervalSeconds is how often the control-plane-side GC sweep runs:
	// flags (alert-only, never auto-corrects) SUBMITTED/ADMITTED/RUNNING experiments with no
	// recent actual-state phase metric — an orphaned desired-state entry that survived an
	// extended cluster-agent outage combined with a control-plane bug/crash.
	StaleDesiredStateSweepIntervalSeconds int `yaml:"stale_desired_state_sweep_interval_seconds"`
	// StaleDesiredStateThresholdSeconds is how old a missing/stale report must be before an
	// experiment is flagged — a large multiple of the ~3s status-report cadence so transient
	// gaps never false-positive.
	StaleDesiredStateThresholdSeconds int `yaml:"stale_desired_state_threshold_seconds"`
	// DefaultTerminationGracePeriodSeconds is used when a job doesn't request its own.
	DefaultTerminationGracePeriodSeconds int `yaml:"default_termination_grace_period_seconds"`
	// MaxTerminationGracePeriodSeconds caps whatever a job requests for itself.
	MaxTerminationGracePeriodSeconds int `yaml:"max_termination_grace_period_seconds"`
}

// Phase2Config holds phase-2 transition constants.
type Phase2Config struct {
	// BoundaryFraction is the fraction of total budget consumed before phase 2 triggers.
	BoundaryFraction float64 `yaml:"boundary_fraction"`
	// AdmissionPercentile is the metric percentile cut used to separate passing agents from
	// held agents at the phase-2 boundary. For maximize metrics, agents whose best value is
	// above this percentile pass. For minimize metrics, agents whose best value is below
	// (1 - AdmissionPercentile) pass. Default 0.75 (top quartile advances).
	AdmissionPercentile float64 `yaml:"admission_percentile"`
}

type ServicesConfig struct {
	QuotaPort            int    `yaml:"quota_port"`
	SchedulerPort        int    `yaml:"scheduler_port"`
	RegistryPort         int    `yaml:"registry_port"`
	MetricControllerPort int    `yaml:"metric_controller_port"`
	MetricsDBURL         string `yaml:"metrics_db_url"`
}

// Config is the top-level parsed config from hypothesisloop.yaml.
type Config struct {
	AcceleratorTypes      []AcceleratorTypeConfig `yaml:"accelerator_types"`
	Quota                 QuotaConfig             `yaml:"quota"`
	Scheduler             SchedulerConfig         `yaml:"scheduler"`
	Phase2                Phase2Config            `yaml:"phase2"`
	Services              ServicesConfig          `yaml:"services"`
	// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate are the flat per-unit quota rates for
	// the non-Accelerator resource dimensions (default 1.0 — see domain.SetCPUCoreHourRate etc).
	CPUCoreHourRate   float64 `yaml:"cpu_core_hour_rate"`
	RAMGBHourRate     float64 `yaml:"ram_gb_hour_rate"`
	StorageGBHourRate float64 `yaml:"storage_gb_hour_rate"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName map[string]float64 // accelerator name → AccH rate
}
