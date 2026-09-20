package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Load reads the YAML file at path and returns a validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	data, err = ExpandEnv(data)
	if err != nil {
		return nil, fmt.Errorf("config: expand %s: %w", path, err)
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

var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces every ${NAME} inside a YAML value with that environment variable, so a
// config file kept in plain sight can name a secret without holding it. It works on the parsed
// values and writes the document back out, which keeps a secret holding YAML syntax (a colon, a
// hash, a quote) one string. An expanded value is always a string. A reference to a variable
// that is not set is an error.
func ExpandEnv(data []byte) ([]byte, error) {
	if !envReference.Match(data) {
		return data, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if err := expandNode(&document); err != nil {
		return nil, err
	}
	return yaml.Marshal(&document)
}

func expandNode(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 1; i < len(node.Content); i += 2 {
			if err := expandNode(node.Content[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		for _, child := range node.Content {
			if err := expandNode(child); err != nil {
				return err
			}
		}
		return nil
	}
	if !envReference.MatchString(node.Value) {
		return nil
	}
	var missing string
	node.Value = envReference.ReplaceAllStringFunc(node.Value, func(reference string) string {
		name := envReference.FindStringSubmatch(reference)[1]
		value, ok := os.LookupEnv(name)
		if !ok && missing == "" {
			missing = name
		}
		return value
	})
	if missing != "" {
		return fmt.Errorf("line %d: environment variable %s is not set", node.Line, missing)
	}
	node.SetString(node.Value)
	return nil
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
	if c.Quota.DefaultMaxConcurrentAccelerators <= 0 {
		return fmt.Errorf("quota default_max_concurrent_accelerators must be positive")
	}
	s := c.Scheduler
	if s.LoopHeartbeatSeconds <= 0 || s.JobPollIntervalSeconds <= 0 ||
		s.StuckPendingTimeoutSeconds <= 0 || s.StartTimeoutSeconds <= 0 || s.ClusterUnreachableAfterSeconds <= 0 ||
		s.ClusterStatusSilenceCeilingSeconds <= 0 || s.GuaranteedFairnessWindowSeconds <= 0 ||
		s.ReconcileIntervalSeconds <= 0 ||
		s.DefaultTerminationGracePeriodSeconds <= 0 || s.MaxTerminationGracePeriodSeconds <= 0 || s.DefaultReportIntervalSeconds <= 0 ||
		s.SilenceMultiplier <= 0 || s.MinSilenceWindowSeconds <= 0 || s.MaxLogTailLineChars <= 0 ||
		s.MaxInfrastructureRequeues <= 0 || s.MaxCheckpointGraceSeconds <= 0 ||
		s.ResourceDisbalanceTolerance <= 0 || s.ScaleUpTimeoutSeconds <= 0 || s.TriedClusterTTLSeconds <= 0 {
		return fmt.Errorf("all scheduler timing, retry, window, and multiplier settings must be positive")
	}
	if s.ScaleUpTimeoutSeconds >= MaxScaleUpTimeoutSeconds {
		return fmt.Errorf("scheduler scale_up_timeout_seconds must be less than %d (PyTorch's default rendezvous timeout)", MaxScaleUpTimeoutSeconds)
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
		return fmt.Errorf("data_store endpoint %q is loopback — job pods run in their own network namespace and cannot reach it. Use an address job pods reach (host.containers.internal under podman machine, else the host's LAN address)", c.DataStore.Endpoint)
	}
	if err := domain.ValidateStages(c.Stages.Default); err != nil {
		return fmt.Errorf("stages.default: %w", err)
	}
	services := c.Services
	if services.APIPort <= 0 || services.APIPort > 65535 ||
		services.MetricControllerPort <= 0 || services.MetricControllerPort > 65535 ||
		services.MetricsDBURL == "" || services.GraphDBURL == "" {
		return fmt.Errorf("service ports must be in 1-65535 and metrics_db_url and graph_db_url are required")
	}
	return nil
}
