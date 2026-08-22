// Package config loads hypothesisloop.yaml and exposes a typed Config used across all services.
package config

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

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
// ObservationCadence is the one definition of how long a sample keeps counting as "still alive"
// before the gap to the next one is treated as the job genuinely not running.
//
// It belongs here, computed once, because the value only means anything when the quota service,
// the settler, the controller and the scheduler all use the same one. Three copies of the
// arithmetic is how they drifted apart in the first place, and a drifted value makes the same job
// cost different amounts depending on which code path is asked.
func (c SchedulerConfig) ObservationCadence() (gapCap time.Duration) {
	return time.Duration(c.SilenceMultiplier * float64(c.DefaultReportIntervalSeconds) * float64(time.Second))
}

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
	MinSilenceWindowSeconds int `yaml:"min_silence_window_seconds"`
	// StuckPendingTimeoutSeconds bounds how long a job may stay SUBMITTED/ADMITTED without ever
	// reporting RUNNING before job_watcher evicts it (reason stuck_pending) and fully refunds
	// its reservation.
	StuckPendingTimeoutSeconds int `yaml:"stuck_pending_timeout_seconds"`
	// ClusterUnreachableAfterSeconds bounds how stale a cluster-agent's heartbeat may be before
	// the cluster is treated as Unreachable: its live-reported capacity (see GetLiveCPUCapacity)
	// ages out of admission (freezing new jobs to it) and it shows Disconnected in the UI.
	// Jobs already RUNNING there are left alone until it reconnects — avoids duplicate dispatch
	// if it comes back. ~15 missed polls at the ~2s desired-state cadence.
	ClusterUnreachableAfterSeconds int `yaml:"cluster_unreachable_after_seconds"`
	// ClusterStatusSilenceCeilingSeconds bounds how long a cluster may push no job-status
	// snapshot at all before the jobs it holds are evicted as cluster_unreachable and their
	// reservations released. Deliberately far longer than ClusterUnreachableAfterSeconds, which
	// only ages a cluster's capacity out of admission: this one ends real work, so it must
	// tolerate a control-plane restart, a rolling agent upgrade, and a transient partition
	// without firing. Without any ceiling, a cluster that never comes back strands every
	// reservation on it forever.
	ClusterStatusSilenceCeilingSeconds int `yaml:"cluster_status_silence_ceiling_seconds"`
	// GuaranteedFairnessWindowSeconds quantizes queued_at into age buckets for the guaranteed
	// tier's admission ordering: jobs within the same bucket are ordered by least-used-quota
	// agent first instead of exact submission timestamp, bounding the latency-fairness gap a
	// steady-submitting agent would otherwise get from pure exact-FIFO. Same shape as every
	// other *_seconds field here: 0/unset falls back to the default below, not "disabled".
	GuaranteedFairnessWindowSeconds int `yaml:"guaranteed_fairness_window_seconds"`
	// ResourceDisbalanceTolerance is the multiple of a cluster's per-accelerator CPU/memory/
	// storage share a running job may request before the scheduler evicts it for stranding idle
	// accelerators on its own node (see services/scheduler/loop_disbalance.go). Required and
	// positive, like every other setting here — scheduler.DefaultDisbalanceTolerance is the
	// suggested value. There is no "off": a disabled pass is a second scheduling behaviour to
	// reason about, and a cluster whose accelerators are stranded by one job's shape is broken
	// whether or not an operator remembered to switch the fix on.
	ResourceDisbalanceTolerance float64 `yaml:"resource_disbalance_tolerance"`
	// DefaultTerminationGracePeriodSeconds is used when a job doesn't request its own.
	DefaultTerminationGracePeriodSeconds int `yaml:"default_termination_grace_period_seconds"`
	// MaxTerminationGracePeriodSeconds caps whatever a job requests for itself.
	MaxTerminationGracePeriodSeconds int `yaml:"max_termination_grace_period_seconds"`
	// MaxLogTailLineChars bounds one line of a job's reported log tail (see
	// runtime/shared/agentloop.splitLongLines): large enough to keep a real compiler error or
	// stack frame intact, small enough that one pathological line can't blow up a status push.
	// Lines over this are split, never truncated/dropped.
	MaxLogTailLineChars int `yaml:"max_log_tail_line_chars"`
}

// StagesConfig holds the platform-wide default elimination ladder.
type StagesConfig struct {
	// Default is the ladder applied to a platform experiment created without its own.
	// Validated by domain.ValidateStages at load.
	Default []domain.Stage `yaml:"default"`
}

type ServicesConfig struct {
	// APIPort serves the whole agent- and UI-facing API — quota, scheduler and registry
	// operations on one router, so there is a single base URL, a single /openapi.json and a
	// single /explore to discover all of it.
	APIPort int `yaml:"api_port"`
	// MetricControllerPort serves the internal controller only; nothing agent-facing.
	MetricControllerPort int    `yaml:"metric_controller_port"`
	MetricsDBURL         string `yaml:"metrics_db_url"`
}

// DataStoreConfig addresses the S3-compatible object store jobs write checkpoints and datasets
// to. The control plane hands out the address and the credentials and never sits in the data
// path: it lists prefixes and measures them, nothing more. Expiring a closed platform
// experiment's prefix is a lifecycle rule configured on the bucket itself, not a sweeper here.
type DataStoreConfig struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	// MaxBytesPerAgent caps how many bytes one agent may hold within one platform experiment,
	// checked at admission against what the store reports right now. Deliberately not enforced
	// mid-write: that would need a gateway in the data path, and a job killed as it saves loses
	// the run it was about to preserve. 0 means unlimited.
	MaxBytesPerAgent int64 `yaml:"max_bytes_per_agent"`
	// SessionDurationSeconds is how long a job's scoped credentials stay valid. A job outliving
	// its session loses write access mid-run and cannot save what it produced, so this must
	// comfortably exceed the longest job the stage ladder allows — it is not a security dial to
	// turn down. Bounded by what the store accepts (objectstore.STSMinDuration/STSMaxDuration).
	SessionDurationSeconds int `yaml:"session_duration_seconds"`
}

// Config is the top-level parsed config from hypothesisloop.yaml.
type Config struct {
	AcceleratorTypes []AcceleratorTypeConfig `yaml:"accelerator_types"`
	DataStore        DataStoreConfig         `yaml:"data_store"`
	Quota            QuotaConfig             `yaml:"quota"`
	Scheduler        SchedulerConfig         `yaml:"scheduler"`
	Stages           StagesConfig            `yaml:"stages"`
	Services         ServicesConfig          `yaml:"services"`
	// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate are the flat per-unit quota rates for
	// the non-Accelerator resource dimensions (default 1.0 — see domain.SetCPUCoreHourRate etc).
	CPUCoreHourRate   float64 `yaml:"cpu_core_hour_rate"`
	RAMGBHourRate     float64 `yaml:"ram_gb_hour_rate"`
	StorageGBHourRate float64 `yaml:"storage_gb_hour_rate"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName map[string]float64 // accelerator name → AccH rate
}
