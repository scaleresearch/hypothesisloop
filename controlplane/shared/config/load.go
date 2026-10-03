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
	"k8s.io/apimachinery/pkg/api/resource"

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

// cpuCoreAccHRate is what one core of domain.CPUType costs an hour. At 1 the budget of an
// experiment on cores reads as CPU core-hours.
const cpuCoreAccHRate = 1.0

func (c *Config) build() error {
	if len(c.AcceleratorTypes) == 0 {
		return fmt.Errorf("accelerator_types must not be empty")
	}
	for _, g := range c.AcceleratorTypes {
		if domain.CPUType.Is(domain.AcceleratorType(g.Name)) {
			return fmt.Errorf("accelerator_types lists %s, which is built in at %g and priced nowhere else", domain.CPUType, cpuCoreAccHRate)
		}
	}
	c.AcceleratorTypes = append([]AcceleratorTypeConfig{{Name: string(domain.CPUType), AccHRate: cpuCoreAccHRate}}, c.AcceleratorTypes...)
	c.RateByName = make(map[string]float64, len(c.AcceleratorTypes))
	c.PodResourcesByName = map[string]map[string]string{}

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
		for name, quantity := range g.PodResources {
			if q, err := resource.ParseQuantity(quantity); name == "" || err != nil || q.Sign() <= 0 {
				return fmt.Errorf("accelerator_type %q pod_resources[%q] must be a positive quantity, got %q", g.Name, name, quantity)
			}
		}
		if len(g.PodResources) > 0 {
			c.PodResourcesByName[g.Name] = g.PodResources
		}
	}
	if c.Quota.BurstFraction <= 0 {
		return fmt.Errorf("quota burst_fraction must be positive")
	}
	// A negative bonus silently shrinks a winning agent's allocation.
	if c.Quota.Top3BonusFraction < 0 {
		return fmt.Errorf("quota top3_bonus_fraction must not be negative")
	}
	// Every limit is stated: a key left out would otherwise lift its limit with no word said.
	for _, limit := range []struct {
		key   string
		value float64
	}{
		{"max_submissions_per_hour", float64(c.Quota.MaxSubmissionsPerHour)},
		{"max_accelerator_count_per_job", float64(c.Quota.MaxAcceleratorCountPerJob)},
		{"max_cpu_cores_per_job", c.Quota.MaxCPUCoresPerJob},
		{"max_ram_gb_per_job", c.Quota.MaxRAMGBPerJob},
		{"max_storage_gb_per_job", c.Quota.MaxStorageGBPerJob},
	} {
		if limit.value <= 0 {
			return fmt.Errorf("quota %s is required and must be positive", limit.key)
		}
	}
	if c.Quota.DefaultMaxConcurrentAccelerators <= 0 {
		return fmt.Errorf("quota default_max_concurrent_accelerators must be positive")
	}
	if tier := domain.QuotaTier(c.Quota.DefaultQuotaTier); tier != domain.QuotaTierGuaranteed && tier != domain.QuotaTierBurstOnly {
		return fmt.Errorf("quota default_quota_tier must be guaranteed or burst_only")
	}
	if c.Quota.MaxPlatformExperimentDays <= 0 {
		return fmt.Errorf("quota max_platform_experiment_days must be positive")
	}
	s := c.Scheduler
	if s.LoopHeartbeatSeconds <= 0 || s.JobPollIntervalSeconds <= 0 ||
		s.StuckPendingTimeoutSeconds <= 0 || s.StartTimeoutSeconds <= 0 || s.ClusterUnreachableAfterSeconds <= 0 ||
		s.ClusterStatusSilenceCeilingSeconds <= 0 || s.GuaranteedFairnessWindowSeconds <= 0 ||
		s.HeldJobNoticeSeconds <= 0 || s.ReconcileIntervalSeconds <= 0 ||
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
	// Neither address stands in for the other: the control plane's is often one no job reaches,
	// and a job's is often one that is down while the control plane's own store is up.
	if c.DataStore.ControlPlaneEndpoint == "" {
		return fmt.Errorf("data_store control_plane_endpoint is required: the address the control plane itself reaches the object store at, on its own network. endpoint is only what jobs and agents are handed")
	}
	if own, err := url.Parse(c.DataStore.ControlPlaneEndpoint); err != nil || own.Host == "" || (own.Scheme != "http" && own.Scheme != "https") {
		return fmt.Errorf("data_store control_plane_endpoint %q must be an http(s) URL with a host", c.DataStore.ControlPlaneEndpoint)
	}
	if c.DataStore.MaxBytesPerAgent <= 0 {
		return fmt.Errorf("data_store max_bytes_per_agent is required and must be positive")
	}
	// A job pod resolves endpoint from inside its own network namespace, which is not the
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
	if c.Planning.LimitMinutes <= 0 {
		return fmt.Errorf("planning limit_minutes is required and positive: how long a draft's planner has to propose")
	}
	if c.Planning.BaselineLimitMinutes <= 0 {
		return fmt.Errorf("planning baseline_limit_minutes is required and positive: how long a platform experiment may stay open without a declared baseline")
	}
	if c.Planning.ProposedForDays <= 0 {
		return fmt.Errorf("planning proposed_for_days is required and positive: how long a proposal waits for a person")
	}
	if c.Accounts.MaxOrgsPerUser <= 0 {
		return fmt.Errorf("accounts max_orgs_per_user is required and positive: how many orgs a person may own when they create one")
	}
	if c.Accounts.MaxProjectsPerOrg <= 0 {
		return fmt.Errorf("accounts max_projects_per_org is required and positive: how many projects an org may hold")
	}
	for key, address := range map[string]string{"services public_api_url": c.Services.PublicAPIURL, "runtimes metrics_url": c.Runtimes.MetricsURL} {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s %q must be the http(s) address the platform is reached at from outside it", key, address)
		}
		if unreachableFromElsewhere(parsed.Hostname()) {
			return fmt.Errorf("%s %q is loopback or unspecified: a hosted agent or a runtime inside a cluster cannot reach it", key, address)
		}
	}
	registry, err := url.Parse("//" + c.Runtimes.Registry)
	if err != nil || registry.Host == "" || strings.Contains(c.Runtimes.Registry, "://") || strings.ContainsAny(c.Runtimes.Registry, " \t") {
		return fmt.Errorf("runtimes registry %q must be the registry a cluster's nodes pull the runtime's images from, as host[:port][/path]", c.Runtimes.Registry)
	}
	if unreachableFromElsewhere(registry.Hostname()) {
		return fmt.Errorf("runtimes registry %q is loopback or unspecified: a node of a cluster cannot pull from it", c.Runtimes.Registry)
	}
	if c.Runtimes.ImageTag == "" || strings.ContainsAny(c.Runtimes.ImageTag, " \t/:") {
		return fmt.Errorf("runtimes image_tag %q must be the tag the runtime's images were pushed under", c.Runtimes.ImageTag)
	}
	offered := map[string]bool{}
	for _, i := range c.JobImages {
		if i.Name == "" || offered[i.Name] {
			return fmt.Errorf("job_images: name %q is empty or listed twice", i.Name)
		}
		offered[i.Name] = true
		if (i.Image == "") == (i.Repository == "") || strings.ContainsAny(i.Image+i.Repository, " \t") {
			return fmt.Errorf("job_images %s: state image, a whole reference, or repository, a name under runtimes.registry, and one of them only", i.Name)
		}
		if i.Carries == "" {
			return fmt.Errorf("job_images %s: carries is required, since what is installed is what an agent chooses the image by", i.Name)
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
	switch g := c.Git; g.Provider {
	case "":
		return fmt.Errorf("git provider is required, the git host platform experiments keep their code on: %s or %s", GitProviderGitHub, GitProviderPlain)
	case GitProviderGitHub:
		if !c.GitHub.Configured() || g != (GitConfig{Provider: GitProviderGitHub}) {
			return fmt.Errorf("git provider github is the GitHub App of the github section: that section is required, and base_url, allow_http, username and password are for provider plain")
		}
	case GitProviderPlain:
		if c.GitHub.Configured() {
			return fmt.Errorf("git provider plain and a github section are set together: a deployment keeps its code on one git host")
		}
		if !strings.HasPrefix(g.BaseURL, "https://") && !(g.AllowHTTP && strings.HasPrefix(g.BaseURL, "http://")) {
			return fmt.Errorf("git base_url %q must be the https root the repositories of the deployment are cloned from, or an http one with allow_http: true", g.BaseURL)
		}
		if (g.Username == "") != (g.Password == "") {
			return fmt.Errorf("git username and password are set together or not at all")
		}
	default:
		return fmt.Errorf("git provider %q is neither %s nor %s", g.Provider, GitProviderGitHub, GitProviderPlain)
	}
	return nil
}
