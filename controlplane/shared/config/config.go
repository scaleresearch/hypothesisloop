// Package config loads openresearch.yaml and exposes a typed Config used across all services.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// GPUTypeConfig defines one GPU tier. Vendor-specific execution-engine plumbing
// (ResourceName/TaintKey/NodeLabelKey/NodeLabelValue) lives per-entry, not as a single
// cluster-wide default, so a cluster can mix vendors (e.g. NVIDIA H100 nodes and AMD MI300X
// nodes) in one catalog — each type carries its own device-plugin resource name, taint, and
// node-label scheme. Note: Kubernetes Dynamic Resource Allocation (resource.k8s.io, GA-track
// since 1.31/1.32) is the eventual native replacement for this extended-resource/taint/label
// approach; these per-vendor fields are a deliberate stopgap until DRA is adopted.
type GPUTypeConfig struct {
	Name        string  `yaml:"name"`
	T4HRate     float64 `yaml:"t4h_rate"`
	Flavor      string  `yaml:"flavor"`
	ClusterGPUs int     `yaml:"cluster_gpus"`
	// NodeLabelValue is the node label value real nodes of this GPU type carry (set by the
	// vendor's device-plugin/feature-discovery add-on), e.g. "NVIDIA-H100-80GB-HBM3" or
	// "AMD-Instinct-MI300X". Required for every type agents can request (see BuildJob's
	// node affinity) — a plain gpu_type with no acceptable_gpu_types still must land on
	// hardware of exactly that model.
	NodeLabelValue string `yaml:"node_label_value"`
	// NodeLabelKey is the node label KEY carrying NodeLabelValue above — defaults to
	// "nvidia.com/gpu.product" (NVIDIA GPU Feature Discovery) if unset. AMD nodes commonly
	// use a different key (e.g. via the AMD GPU Operator's own labeling), so this must be
	// set per-entry for non-NVIDIA types.
	NodeLabelKey string `yaml:"node_label_key"`
	// ResourceName is the k8s extended resource name requested per GPU of this type
	// (quantity = JobSpec.GPUCount) — defaults to "nvidia.com/gpu" if unset. AMD's device
	// plugin advertises "amd.com/gpu" instead.
	ResourceName string `yaml:"resource_name"`
	// TaintKey is the taint key nodes of this GPU type carry (so only GPU-requesting pods
	// land there) — defaults to "nvidia.com/gpu" if unset, matching ResourceName's default.
	TaintKey string `yaml:"taint_key"`
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

	// MaxGPUCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap how much of
	// each resource dimension a single job may request (per node × num_nodes total), on top
	// of the guaranteed/burst quota check — this is what actually prevents one absurd
	// submission from blowing through an entire budget in one debit. 0 means unlimited.
	// Operators must size these sanely relative to burst-pool sizing: a cap alone doesn't
	// replace correct budget sizing, it only bounds a single job's blast radius.
	MaxGPUCountPerJob  int     `yaml:"max_gpu_count_per_job"`
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
	MinSilenceWindowSeconds int `yaml:"min_silence_window_seconds"`
	OverrunMultiplier            float64 `yaml:"overrun_multiplier"`
	MetricWindowSize             int     `yaml:"metric_window_size"`
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
	GPUTypes              []GPUTypeConfig `yaml:"gpu_types"`
	InterchangeableGroups [][]string      `yaml:"interchangeable_groups"`
	Quota                 QuotaConfig     `yaml:"quota"`
	Scheduler             SchedulerConfig `yaml:"scheduler"`
	Phase2                Phase2Config    `yaml:"phase2"`
	// GPUResourceName is the k8s extended resource name the backend requests per GPU
	// (quantity = JobSpec.GPUCount) when compiling a job down to a native k8s Job —
	// entirely an execution-engine concern, never seen by agents. Defaults to
	// "nvidia.com/gpu"; set to "cpu" as a temporary substitution for a cluster with no GPU
	// nodes/device plugin yet.
	GPUResourceName string `yaml:"gpu_resource_name"`
	// GPUTaintKey is the taint key real GPU nodes carry (the NVIDIA GPU Operator applies
	// this by default so only GPU-requesting pods land there); the backend automatically
	// tolerates it on any pod that requests GPUs. Defaults to "nvidia.com/gpu". Also purely
	// an execution-engine concern — agents never declare tolerations in JobSpec.
	GPUTaintKey string `yaml:"gpu_taint_key"`

	// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate are the flat per-unit quota rates for
	// the non-GPU resource dimensions (default 1.0 — see domain.SetCPUCoreHourRate etc).
	CPUCoreHourRate   float64 `yaml:"cpu_core_hour_rate"`
	RAMGBHourRate     float64 `yaml:"ram_gb_hour_rate"`
	StorageGBHourRate float64 `yaml:"storage_gb_hour_rate"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName         map[string]float64 // gpu name → T4h rate
	FlavorByName       map[string]string  // gpu name → flavor name
	NameByFlavor       map[string]string  // flavor name → gpu name
	GPUsByFlavor       map[string]int     // flavor name → cluster gpu count
	NodeLabelByType    map[string]string  // gpu name → node label value
	NodeLabelKeyByType map[string]string  // gpu name → node label key (defaulted per-type)
	ResourceNameByType map[string]string  // gpu name → k8s extended resource name (defaulted per-type)
	TaintKeyByType     map[string]string  // gpu name → node taint key (defaulted per-type)
}

// Load reads the YAML file at path and returns a validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.build(); err != nil {
		return nil, fmt.Errorf("config: validate: %w", err)
	}
	return &cfg, nil
}

// MustLoad calls Load and panics on error. Suitable for use in main().
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}
	return cfg
}

func (c *Config) build() error {
	if len(c.GPUTypes) == 0 {
		return fmt.Errorf("gpu_types must not be empty")
	}
	c.RateByName = make(map[string]float64, len(c.GPUTypes))
	c.FlavorByName = make(map[string]string, len(c.GPUTypes))
	c.NameByFlavor = make(map[string]string, len(c.GPUTypes))
	c.GPUsByFlavor = make(map[string]int, len(c.GPUTypes))
	c.NodeLabelByType = make(map[string]string, len(c.GPUTypes))
	c.NodeLabelKeyByType = make(map[string]string, len(c.GPUTypes))
	c.ResourceNameByType = make(map[string]string, len(c.GPUTypes))
	c.TaintKeyByType = make(map[string]string, len(c.GPUTypes))

	defaultResourceName := c.GPUResourceName
	if defaultResourceName == "" {
		defaultResourceName = "nvidia.com/gpu"
	}
	defaultTaintKey := c.GPUTaintKey
	if defaultTaintKey == "" {
		defaultTaintKey = "nvidia.com/gpu"
	}

	for _, g := range c.GPUTypes {
		if g.Name == "" || g.Flavor == "" {
			return fmt.Errorf("each gpu_type must have name and flavor")
		}
		c.RateByName[g.Name] = g.T4HRate
		c.FlavorByName[g.Name] = g.Flavor
		c.NameByFlavor[g.Flavor] = g.Name
		c.GPUsByFlavor[g.Flavor] = g.ClusterGPUs
		if g.NodeLabelValue != "" {
			c.NodeLabelByType[g.Name] = g.NodeLabelValue
		}
		nodeLabelKey := g.NodeLabelKey
		if nodeLabelKey == "" {
			nodeLabelKey = "nvidia.com/gpu.product"
		}
		c.NodeLabelKeyByType[g.Name] = nodeLabelKey
		resourceName := g.ResourceName
		if resourceName == "" {
			resourceName = defaultResourceName
		}
		c.ResourceNameByType[g.Name] = resourceName
		taintKey := g.TaintKey
		if taintKey == "" {
			taintKey = defaultTaintKey
		}
		c.TaintKeyByType[g.Name] = taintKey
	}
	if c.Quota.BurstFraction == 0 {
		c.Quota.BurstFraction = 2.0
	}
	if c.Quota.MaxSubmissionsPerHour == 0 {
		c.Quota.MaxSubmissionsPerHour = 100
	}
	if c.Quota.MetricDeclineFraction == 0 {
		c.Quota.MetricDeclineFraction = 0.3
	}
	if c.Scheduler.LoopHeartbeatSeconds == 0 {
		c.Scheduler.LoopHeartbeatSeconds = 10
	}
	if c.Scheduler.PreemptTimeoutSeconds == 0 {
		c.Scheduler.PreemptTimeoutSeconds = 30
	}
	if c.Scheduler.JobPollIntervalSeconds == 0 {
		c.Scheduler.JobPollIntervalSeconds = 5
	}
	if c.Scheduler.AdmittedScanIntervalSeconds == 0 {
		c.Scheduler.AdmittedScanIntervalSeconds = 10
	}
	if c.Scheduler.StuckPendingTimeoutSeconds == 0 {
		c.Scheduler.StuckPendingTimeoutSeconds = 300
	}
	if c.Scheduler.ClusterUnreachableAfterSeconds == 0 {
		c.Scheduler.ClusterUnreachableAfterSeconds = 30
	}
	if c.Scheduler.GuaranteedFairnessWindowSeconds == 0 {
		c.Scheduler.GuaranteedFairnessWindowSeconds = 60
	}
	if c.Scheduler.StaleDesiredStateSweepIntervalSeconds == 0 {
		c.Scheduler.StaleDesiredStateSweepIntervalSeconds = 600
	}
	if c.Scheduler.StaleDesiredStateThresholdSeconds == 0 {
		c.Scheduler.StaleDesiredStateThresholdSeconds = 300
	}
	if c.Scheduler.ReconcileIntervalSeconds == 0 {
		c.Scheduler.ReconcileIntervalSeconds = 30
	}
	if c.Scheduler.DefaultReportIntervalSeconds == 0 {
		c.Scheduler.DefaultReportIntervalSeconds = 30
	}
	if c.Scheduler.SilenceMultiplier == 0 {
		c.Scheduler.SilenceMultiplier = 3.0
	}
	if c.Scheduler.OverrunMultiplier == 0 {
		c.Scheduler.OverrunMultiplier = 1.5
	}
	if c.Scheduler.MetricWindowSize == 0 {
		c.Scheduler.MetricWindowSize = 20
	}
	if c.Scheduler.JobBackoffLimit == 0 {
		c.Scheduler.JobBackoffLimit = 3
	}
	if c.Scheduler.JobDeadlineMultiplier == 0 {
		c.Scheduler.JobDeadlineMultiplier = 1.5
	}
	if c.Scheduler.MinJobDeadlineSeconds == 0 {
		c.Scheduler.MinJobDeadlineSeconds = 300
	}
	if c.Phase2.BoundaryFraction == 0 {
		c.Phase2.BoundaryFraction = 0.40
	}
	if c.Phase2.AdmissionPercentile == 0 {
		c.Phase2.AdmissionPercentile = 0.75
	}
	if c.GPUResourceName == "" {
		c.GPUResourceName = "nvidia.com/gpu"
	}
	if c.GPUTaintKey == "" {
		c.GPUTaintKey = "nvidia.com/gpu"
	}
	return nil
}

// ClusterEntry describes one target cluster the control plane can admit
// experiments onto.
type ClusterEntry struct {
	Name           string `yaml:"name"`
	KubeconfigPath string `yaml:"kubeconfig_path"`
	Context        string `yaml:"context"`
}

// ClustersConfig is the top-level shape of a clusters.yaml file.
type ClustersConfig struct {
	Clusters []ClusterEntry `yaml:"clusters"`
}

// LoadClusters reads a clusters.yaml-shaped file and returns its cluster entries.
// If path is empty or the file does not exist, it returns (nil, nil) so callers can
// fall back to single-cluster behavior built from env vars (preserves today's behavior
// exactly).
func LoadClusters(path string) ([]ClusterEntry, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cc ClustersConfig
	if err := yaml.Unmarshal(data, &cc); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	for i, c := range cc.Clusters {
		if c.Name == "" {
			return nil, fmt.Errorf("config: %s: cluster entry %d missing name", path, i)
		}
	}
	return cc.Clusters, nil
}

// RateFor returns the T4h rate for the named GPU type (e.g. "H100").
// Returns 1.0 (T4 base rate) for unknown types.
func (c *Config) RateFor(gpuName string) float64 {
	if r, ok := c.RateByName[gpuName]; ok {
		return r
	}
	return 1.0
}

// GPUNameForFlavor returns the GPU type name for a flavor name.
// Returns "" if not found.
func (c *Config) GPUNameForFlavor(flavor string) string {
	return c.NameByFlavor[flavor]
}

// FlavorOrder returns flavor names in definition order.
func (c *Config) FlavorOrder() []string {
	out := make([]string, len(c.GPUTypes))
	for i, g := range c.GPUTypes {
		out[i] = g.Flavor
	}
	return out
}
