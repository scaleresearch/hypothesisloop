package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
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
		s.MaxInfrastructureRequeues <= 0 ||
		s.ResourceDisbalanceTolerance <= 0 {
		return fmt.Errorf("all scheduler timing, retry, window, and multiplier settings must be positive")
	}
	// Required, not optional: every job is handed a data address, so a deployment without a
	// store would hand out an address that resolves to nothing.
	if c.DataStore.Endpoint == "" || c.DataStore.Region == "" || c.DataStore.Bucket == "" ||
		c.DataStore.AccessKeyID == "" || c.DataStore.SecretAccessKey == "" {
		return fmt.Errorf("data_store endpoint, region, bucket, access_key_id and secret_access_key are all required")
	}
	if c.DataStore.MaxBytesPerAgent < 0 {
		return fmt.Errorf("data_store max_bytes_per_agent must not be negative (0 means unlimited)")
	}
	sessionDuration := time.Duration(c.DataStore.SessionDurationSeconds) * time.Second
	if sessionDuration < objectstore.STSMinDuration || sessionDuration > objectstore.STSMaxDuration {
		return fmt.Errorf("data_store session_duration_seconds must be between %d and %d",
			int(objectstore.STSMinDuration.Seconds()), int(objectstore.STSMaxDuration.Seconds()))
	}
	// A job pod resolves this endpoint from inside its own network namespace, which is not the
	// control plane's. A loopback address works from every container that shares the host's
	// network and from nothing else, so it fails only once a real job tries to save — hours in,
	// with nothing to show for the run. Same failure the shared code repo's URL has (see
	// agents/coordinator/setup.md step 3), caught here instead of in a job's logs.
	endpointHost, err := url.Parse(c.DataStore.Endpoint)
	if err != nil {
		return fmt.Errorf("data_store endpoint %q is not a URL: %w", c.DataStore.Endpoint, err)
	}
	if host := endpointHost.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("data_store endpoint %q is loopback — job pods run in their own network namespace and cannot reach it. Use the host's LAN address (ip -4 addr show)", c.DataStore.Endpoint)
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
