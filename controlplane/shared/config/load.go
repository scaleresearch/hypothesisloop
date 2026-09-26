package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

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
	if err := refusePlaceholders(data); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
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

// Placeholder marks a value of the committed settings file that a deployment must fill in.
const Placeholder = "REPLACE-WITH-"

// refusePlaceholders fails on any value still holding a placeholder. A placeholder inside an
// address is a well-formed address, so no later check would catch it, and the platform would
// start and hand it out.
func refusePlaceholders(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	var walk func(node *yaml.Node) error
	walk = func(node *yaml.Node) error {
		if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, Placeholder) {
			return fmt.Errorf("line %d: %q is a placeholder the deployment has not filled in (locally: make render-settings)", node.Line, node.Value)
		}
		for _, child := range node.Content {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&document)
}

// unreachableFromElsewhere reports a host that names only the machine a process itself runs on:
// localhost, a loopback address, or the unspecified address a server binds to.
func unreachableFromElsewhere(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip.IsLoopback() || ip.IsUnspecified()
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
	if unreachableFromElsewhere(endpointHost.Hostname()) {
		return fmt.Errorf("data_store endpoint %q is loopback or unspecified — job pods run in their own network namespace and cannot reach it. Use an address job pods reach (host.containers.internal under podman machine, else the host's LAN address)", c.DataStore.Endpoint)
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
	if len(c.Auth.SigningSecret) < MinSigningSecretLength {
		return fmt.Errorf("auth signing_secret is required and must be at least %d characters", MinSigningSecretLength)
	}
	if err := domain.ValidateLauncherName(c.Agents.Launcher); err != nil {
		return fmt.Errorf("agents launcher: %w", err)
	}
	if c.Agents.Image == "" {
		return fmt.Errorf("agents image is required: what a hosted agent's machine boots")
	}
	if apiURL, err := url.Parse(c.Agents.APIURL); err != nil || apiURL.Host == "" || (apiURL.Scheme != "http" && apiURL.Scheme != "https") {
		return fmt.Errorf("agents api_url %q must be the http(s) address hosted agents reach this API at", c.Agents.APIURL)
	}
	if _, _, err := domain.ResolveAgentLaunch("", "", c.Agents.DefaultModel, nil); err != nil {
		return fmt.Errorf("agents default_model is required, the model an agent runs on when it names none: %w", err)
	}
	if c.Agents.BootLimitSeconds <= 0 {
		return fmt.Errorf("agents boot_limit_seconds is required and positive: how long a hosted agent's machine may stay starting")
	}
	if c.Agents.ProgressLimitSeconds < domain.MinAgentProgressLimitSeconds {
		return fmt.Errorf("agents progress_limit_seconds is required and at least %d: an agent prints its progress every %d seconds, and a limit under %d of those ends machines whose output only reached the platform late",
			domain.MinAgentProgressLimitSeconds, domain.AgentProgressIntervalSeconds, domain.MinAgentProgressLimitSeconds/domain.AgentProgressIntervalSeconds)
	}
	if c.Agents.MaxNewInstancesPerAnswer <= 0 {
		return fmt.Errorf("agents max_new_instances_per_answer is required and positive: how many machines one answer asks a launcher to create")
	}
	if c.Agents.MaxHostedPerPlatformExperiment <= 0 {
		return fmt.Errorf("agents max_hosted_per_platform_experiment is required and positive: how many machines the platform runs for one platform experiment, its coordinator included")
	}
	if c.Accounts.MaxOrgsPerUser <= 0 {
		return fmt.Errorf("accounts max_orgs_per_user is required and positive: how many orgs a person may own when they create one")
	}
	if c.Accounts.MaxProjectsPerOrg <= 0 {
		return fmt.Errorf("accounts max_projects_per_org is required and positive: how many projects an org may hold")
	}
	for key, address := range map[string]string{"api_url": c.Runtimes.APIURL, "metrics_url": c.Runtimes.MetricsURL} {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("runtimes %s %q must be the http(s) address a cluster's runtime reaches the platform at", key, address)
		}
		if unreachableFromElsewhere(parsed.Hostname()) {
			return fmt.Errorf("runtimes %s %q is loopback or unspecified: a runtime inside a cluster cannot reach it", key, address)
		}
	}
	if c.Auth.UIOrigin == "" {
		return fmt.Errorf("auth ui_origin is required: the one browser origin allowed to call the API")
	}
	issuers := map[string]bool{}
	for _, provider := range c.Auth.IdentityProviders {
		if provider.Issuer == "" || provider.Audience == "" || len(provider.JWTKeys) == 0 {
			return fmt.Errorf("auth identity_providers: issuer, audience and jwt_keys are all required of each (issuer %q)", provider.Issuer)
		}
		if issuers[provider.Issuer] {
			return fmt.Errorf("auth identity_providers: issuer %q is listed twice", provider.Issuer)
		}
		issuers[provider.Issuer] = true
	}
	if g := c.GitHub; g.Configured() && (g.AppID <= 0 || g.AppSlug == "" || g.ClientID == "" || g.ClientSecret == "" || g.PrivateKey == "") {
		return fmt.Errorf("github app_id, app_slug, client_id, client_secret and private_key are all required once any of them is set")
	}
	return nil
}
