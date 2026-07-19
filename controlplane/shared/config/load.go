package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

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
	if len(c.AcceleratorTypes) == 0 {
		return fmt.Errorf("accelerator_types must not be empty")
	}
	c.RateByName = make(map[string]float64, len(c.AcceleratorTypes))
	c.FlavorByName = make(map[string]string, len(c.AcceleratorTypes))
	c.NameByFlavor = make(map[string]string, len(c.AcceleratorTypes))
	c.AcceleratorsByFlavor = make(map[string]int, len(c.AcceleratorTypes))
	c.NodeLabelByType = make(map[string]string, len(c.AcceleratorTypes))
	c.NodeLabelKeyByType = make(map[string]string, len(c.AcceleratorTypes))
	c.ResourceNameByType = make(map[string]string, len(c.AcceleratorTypes))
	c.TaintKeyByType = make(map[string]string, len(c.AcceleratorTypes))
	c.AllocationModeByType = make(map[string]string, len(c.AcceleratorTypes))
	c.DeviceClassNameByType = make(map[string]string, len(c.AcceleratorTypes))

	defaultResourceName := c.AcceleratorResourceName
	if defaultResourceName == "" {
		defaultResourceName = "nvidia.com/gpu"
	}
	defaultTaintKey := c.AcceleratorTaintKey
	if defaultTaintKey == "" {
		defaultTaintKey = "nvidia.com/gpu"
	}

	for _, g := range c.AcceleratorTypes {
		if g.Name == "" || g.Flavor == "" {
			return fmt.Errorf("each accelerator_type must have name and flavor")
		}
		c.RateByName[g.Name] = g.AccHRate
		c.FlavorByName[g.Name] = g.Flavor
		c.NameByFlavor[g.Flavor] = g.Name
		c.AcceleratorsByFlavor[g.Flavor] = g.ClusterAccelerators

		allocationMode := g.AllocationMode
		if allocationMode == "" {
			allocationMode = AllocationModeResource
		}
		if allocationMode != AllocationModeResource && allocationMode != AllocationModeDRA {
			return fmt.Errorf("accelerator_type %q: allocation_mode must be %q or %q, got %q", g.Name, AllocationModeResource, AllocationModeDRA, allocationMode)
		}
		c.AllocationModeByType[g.Name] = allocationMode

		if allocationMode == AllocationModeDRA {
			// DRA devices are selected by the DRA scheduler plugin/kubelet plugin, not by
			// node label/extended resource/taint — those fields are meaningless here and
			// deliberately left unpopulated so a misconfigured mix (e.g. a stray taint_key
			// on a dra-mode entry) can't silently do something. See AllocationMode's doc.
			if g.DeviceClassName == "" {
				return fmt.Errorf("accelerator_type %q: device_class_name is required when allocation_mode is %q", g.Name, AllocationModeDRA)
			}
			c.DeviceClassNameByType[g.Name] = g.DeviceClassName
			continue
		}

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
	if c.AcceleratorResourceName == "" {
		c.AcceleratorResourceName = "nvidia.com/gpu"
	}
	if c.AcceleratorTaintKey == "" {
		c.AcceleratorTaintKey = "nvidia.com/gpu"
	}
	return nil
}
