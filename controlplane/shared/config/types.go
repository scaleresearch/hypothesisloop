// Package config loads openresearch.yaml and exposes a typed Config used across all services.
package config

// AcceleratorTypeConfig defines one accelerator tier. Vendor-specific execution-engine plumbing
// (ResourceName/TaintKey/NodeLabelKey/NodeLabelValue) lives per-entry, not as a single
// cluster-wide default, so a cluster can mix vendors (e.g. NVIDIA H100 nodes and AMD MI300X
// nodes) in one catalog — each type carries its own device-plugin resource name, taint, and
// node-label scheme. Note: Kubernetes Dynamic Resource Allocation (resource.k8s.io, GA-track
// since 1.31/1.32) is the eventual native replacement for this extended-resource/taint/label
// approach; these per-vendor fields are a deliberate stopgap until DRA is adopted.
type AcceleratorTypeConfig struct {
	Name        string  `yaml:"name"`
	AccHRate     float64 `yaml:"acch_rate"`
	Flavor      string  `yaml:"flavor"`
	ClusterAccelerators int     `yaml:"cluster_accelerators"`
	// NodeLabelValue is the node label value real nodes of this accelerator type carry (set by the
	// vendor's device-plugin/feature-discovery add-on), e.g. "NVIDIA-H100-80GB-HBM3" or
	// "AMD-Instinct-MI300X". Required for every type agents can request (see BuildJob's
	// node affinity) — a plain accelerator_type with no acceptable_accelerator_types still must land on
	// hardware of exactly that model.
	NodeLabelValue string `yaml:"node_label_value"`
	// NodeLabelKey is the node label KEY carrying NodeLabelValue above — defaults to
	// "nvidia.com/gpu.product" (NVIDIA GPU Feature Discovery) if unset. AMD nodes commonly
	// use a different key (e.g. via the AMD GPU Operator's own labeling), so this must be
	// set per-entry for non-NVIDIA types.
	NodeLabelKey string `yaml:"node_label_key"`
	// ResourceName is the k8s extended resource name requested per accelerator of this type
	// (quantity = JobSpec.AcceleratorCount) — defaults to "nvidia.com/gpu" if unset. AMD's device
	// plugin advertises "amd.com/gpu" instead.
	ResourceName string `yaml:"resource_name"`
	// TaintKey is the taint key nodes of this accelerator type carry (so only accelerator-requesting pods
	// land there) — defaults to "nvidia.com/gpu" if unset, matching ResourceName's default.
	TaintKey string `yaml:"taint_key"`

	// AllocationMode selects how the backend hands this accelerator type to a pod:
	//   "resource" (default) — the classic device-plugin model: an extended resource request
	//                           (ResourceName, quantity = AcceleratorCount) plus a nodeAffinity
	//                           on NodeLabelKey/NodeLabelValue and a toleration for TaintKey.
	//   "dra"                — Kubernetes Dynamic Resource Allocation (resource.k8s.io): the
	//                           backend creates a ResourceClaimTemplate requesting DeviceClassName
	//                           (quantity = AcceleratorCount) and attaches it to the pod via
	//                           PodResourceClaims/Container.Resources.Claims instead of a plain
	//                           extended resource. No NodeLabelValue/ResourceName/TaintKey needed —
	//                           the DRA scheduler plugin and the vendor's kubelet plugin (e.g.
	//                           Tenstorrent's tt-dra-driver) handle device selection and node
	//                           placement natively. This is the mode a Tenstorrent (or any other
	//                           DRA-native vendor) accelerator type should use.
	// Any other vendor/mechanism (JobSet-managed multi-host DRA, a future AMD DRA driver, ...)
	// is meant to slot in as a third mode here rather than a new code path elsewhere — see
	// BuildJob's allocationModeFor branch, the single place this field is read.
	AllocationMode string `yaml:"allocation_mode"`
	// DeviceClassName is the resource.k8s.io DeviceClass this type's ResourceClaimTemplate
	// requests — required when AllocationMode is "dra" (e.g. "tenstorrent.com"), ignored
	// otherwise. Set by whatever installed the DRA driver (see tenstorrent/README.md).
	DeviceClassName string `yaml:"device_class_name"`
}

const (
	AllocationModeResource = "resource"
	AllocationModeDRA      = "dra"
)

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
	MaxAcceleratorCountPerJob  int     `yaml:"max_accelerator_count_per_job"`
	MaxCPUCoresPerJob  float64 `yaml:"max_cpu_cores_per_job"`
	MaxRAMGBPerJob     float64 `yaml:"max_ram_gb_per_job"`
	MaxStorageGBPerJob float64 `yaml:"max_storage_gb_per_job"`
}

// SchedulerConfig holds timing and eviction tuning constants.
type SchedulerConfig struct {
	LoopHeartbeatSeconds         int     `yaml:"loop_heartbeat_seconds"`
	PreemptTimeoutSeconds        int     `yaml:"preempt_timeout_seconds"`
	JobPollIntervalSeconds       int     `yaml:"job_poll_interval_seconds"`
	AdmittedScanIntervalSeconds  int     `yaml:"admitted_scan_interval_seconds"`
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
	// JobBackoffLimit is how many times the backend retries a failing pod (RestartPolicyOnFailure)
	// before marking the Job Failed. Tolerates transient flakes; a persistently crashing job
	// exhausts the retries and is failed natively by the backend.
	JobBackoffLimit int `yaml:"job_backoff_limit"`
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
	// recent cluster_job_reports row — an orphaned desired-state entry that survived an
	// extended cluster-agent outage combined with a control-plane bug/crash.
	StaleDesiredStateSweepIntervalSeconds int `yaml:"stale_desired_state_sweep_interval_seconds"`
	// StaleDesiredStateThresholdSeconds is how old a missing/stale report must be before an
	// experiment is flagged — a large multiple of the ~3s status-report cadence so transient
	// gaps never false-positive.
	StaleDesiredStateThresholdSeconds int `yaml:"stale_desired_state_threshold_seconds"`
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

// Config is the top-level parsed config from openresearch.yaml.
type Config struct {
	AcceleratorTypes              []AcceleratorTypeConfig `yaml:"accelerator_types"`
	InterchangeableGroups [][]string      `yaml:"interchangeable_groups"`
	Quota                 QuotaConfig     `yaml:"quota"`
	Scheduler             SchedulerConfig `yaml:"scheduler"`
	Phase2                Phase2Config    `yaml:"phase2"`
	// AcceleratorResourceName is the k8s extended resource name the backend requests per accelerator
	// (quantity = JobSpec.AcceleratorCount) when compiling a job down to a native k8s Job —
	// entirely an execution-engine concern, never seen by agents. Defaults to
	// "nvidia.com/gpu"; set to "cpu" as a temporary substitution for a cluster with no accelerator
	// nodes/device plugin yet.
	AcceleratorResourceName string `yaml:"accelerator_resource_name"`
	// AcceleratorTaintKey is the taint key real accelerator nodes carry (the NVIDIA GPU Operator applies
	// this by default so only accelerator-requesting pods land there); the backend automatically
	// tolerates it on any pod that requests accelerators. Defaults to "nvidia.com/gpu". Also purely
	// an execution-engine concern — agents never declare tolerations in JobSpec.
	AcceleratorTaintKey string `yaml:"accelerator_taint_key"`

	// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate are the flat per-unit quota rates for
	// the non-Accelerator resource dimensions (default 1.0 — see domain.SetCPUCoreHourRate etc).
	CPUCoreHourRate   float64 `yaml:"cpu_core_hour_rate"`
	RAMGBHourRate     float64 `yaml:"ram_gb_hour_rate"`
	StorageGBHourRate float64 `yaml:"storage_gb_hour_rate"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName         map[string]float64 // accelerator name → AccH rate
	FlavorByName       map[string]string  // accelerator name → flavor name
	NameByFlavor       map[string]string  // flavor name → accelerator name
	AcceleratorsByFlavor       map[string]int     // flavor name → cluster accelerator count
	NodeLabelByType    map[string]string  // accelerator name → node label value
	NodeLabelKeyByType map[string]string  // accelerator name → node label key (defaulted per-type)
	ResourceNameByType map[string]string  // accelerator name → k8s extended resource name (defaulted per-type)
	TaintKeyByType     map[string]string  // accelerator name → node taint key (defaulted per-type)
	AllocationModeByType  map[string]string // accelerator name → "resource" | "dra" (defaulted per-type)
	DeviceClassNameByType map[string]string // accelerator name → resource.k8s.io DeviceClass (dra types only)
}
