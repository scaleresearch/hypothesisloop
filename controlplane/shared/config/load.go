package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Load reads the YAML file at path and returns a validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
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

	seenNames := map[string]bool{}
	for _, g := range c.AcceleratorTypes {
		if g.Name == "" {
			return fmt.Errorf("each accelerator_type must have name")
		}
		if g.AccHRate <= 0 {
			return fmt.Errorf("accelerator_type %q must have a positive acch_rate", g.Name)
		}
		if seenNames[g.Name] {
			return fmt.Errorf("accelerator_type names must be unique: %q", g.Name)
		}
		seenNames[g.Name] = true
		c.RateByName[g.Name] = g.AccHRate
	}
	if c.CPUCoreHourRate <= 0 || c.RAMGBHourRate <= 0 || c.StorageGBHourRate <= 0 {
		return fmt.Errorf("all resource-hour rates must be positive")
	}
	if c.Quota.BurstFraction <= 0 {
		return fmt.Errorf("quota burst_fraction must be positive")
	}
	// A negative bonus silently *shrinks* a winning agent's allocation, and a negative per-job or
	// per-hour limit reads downstream as "unlimited" — both are misconfigurations that produce no
	// error and no log line, only wrong quota.
	if c.Quota.Top3BonusFraction < 0 {
		return fmt.Errorf("quota top3_bonus_fraction must not be negative")
	}
	if c.Quota.MaxSubmissionsPerHour < 0 || c.Quota.MaxAcceleratorCountPerJob < 0 ||
		c.Quota.MaxCPUCoresPerJob < 0 || c.Quota.MaxRAMGBPerJob < 0 || c.Quota.MaxStorageGBPerJob < 0 {
		return fmt.Errorf("quota per-job and per-hour limits must not be negative (0 means unlimited)")
	}
	s := c.Scheduler
	if s.LoopHeartbeatSeconds <= 0 || s.JobPollIntervalSeconds <= 0 ||
		s.StuckPendingTimeoutSeconds <= 0 || s.ClusterUnreachableAfterSeconds <= 0 ||
		s.ClusterStatusSilenceCeilingSeconds <= 0 || s.GuaranteedFairnessWindowSeconds <= 0 ||
		s.ReconcileIntervalSeconds <= 0 ||
		s.DefaultTerminationGracePeriodSeconds <= 0 || s.MaxTerminationGracePeriodSeconds <= 0 || s.DefaultReportIntervalSeconds <= 0 ||
		s.SilenceMultiplier <= 0 || s.MinSilenceWindowSeconds <= 0 || s.MaxLogTailLineChars <= 0 ||
		s.ResourceDisbalanceTolerance <= 0 {
		return fmt.Errorf("all scheduler timing, retry, window, and multiplier settings must be positive")
	}
	if err := domain.ValidateStages(c.Stages.Default); err != nil {
		return fmt.Errorf("stages.default: %w", err)
	}
	services := c.Services
	if services.APIPort <= 0 || services.APIPort > 65535 ||
		services.MetricControllerPort <= 0 || services.MetricControllerPort > 65535 ||
		services.MetricsDBURL == "" {
		return fmt.Errorf("service ports must be in 1-65535 and metrics_db_url is required")
	}
	return nil
}
