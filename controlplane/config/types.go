package config

import (
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AcceleratorTypeConfig defines one priced type: its identity, its exchange rate and what every
// node of a job on it needs beside the device.
type AcceleratorTypeConfig struct {
	Name     string  `yaml:"name"`
	AccHRate float64 `yaml:"acch_rate"`
	// PodResources are resource requests every node of a job on this type gets, by the name the
	// runtime knows them (hugepages-1Gi: 4Gi). No job states them.
	PodResources map[string]string `yaml:"pod_resources"`
}

// AcceleratorTypeNames returns every accelerator type the catalog prices — the driver-published
// "key=value" strings, which are also the only types capacity reports and admission accepts.
func (c *Settings) AcceleratorTypeNames() []string {
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
	// within a single platform experiment. Required and positive.
	MaxSubmissionsPerHour int `yaml:"max_submissions_per_hour"`

	// MaxAcceleratorCountPerJob/MaxCPUCoresPerJob/MaxRAMGBPerJob/MaxStorageGBPerJob cap how much of
	// each resource dimension a single job may request (per node × num_nodes total), on top
	// of the guaranteed/burst quota check — this is what actually prevents one absurd
	// submission from blowing through an entire budget in one debit. Each is required and positive.
	// Operators must size these sanely relative to burst-pool sizing: a cap alone doesn't
	// replace correct budget sizing, it only bounds a single job's blast radius.
	// DefaultMaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING) per
	// platform experiment when it does not set its own max_concurrent_accelerators. Required
	// and positive: speculative submission (see scheduler.ScaleUpTimeoutSeconds) makes capacity
	// appear on demand, removing the live-capacity ceiling that otherwise bounded concurrency —
	// "unlimited" is not offered, this default is what "I don't care" means.
	DefaultMaxConcurrentAccelerators int `yaml:"default_max_concurrent_accelerators"`
	// DefaultModelBudgetUSD is the model budget of a platform experiment that states none, and
	// MaxModelBudgetUSD the most one may state. Required, 0 < default <= max.
	DefaultModelBudgetUSD float64 `yaml:"default_model_budget_usd"`
	MaxModelBudgetUSD     float64 `yaml:"max_model_budget_usd"`
	// DefaultQuotaTier is the tier of a signup that states none, in a platform experiment that
	// states no default_quota_tier: guaranteed or burst_only.
	DefaultQuotaTier string `yaml:"default_quota_tier"`
	// MaxPlatformExperimentDays is the longest a platform experiment may run, from the start of
	// its run to its end. Required and positive: until its end an experiment may rent machines.
	MaxPlatformExperimentDays int `yaml:"max_platform_experiment_days"`
	// DefaultOrgSharedAccHPerHour and DefaultProjectSharedAccHPerHour are the platform compute
	// limit (domain.ComputeLimit) an org is created with: the accelerator hours per hour of the
	// platform's clusters its placed jobs, and each project's, may reserve. Required, 0 < project <= org.
	DefaultOrgSharedAccHPerHour     float64 `yaml:"default_org_shared_acch_per_hour"`
	DefaultProjectSharedAccHPerHour float64 `yaml:"default_project_shared_acch_per_hour"`

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
	// StuckPendingTimeoutSeconds bounds how long a job may stay SUBMITTED without ever
	// reporting RUNNING before job_watcher evicts it (reason stuck_pending) and fully refunds
	// its reservation.
	StuckPendingTimeoutSeconds int `yaml:"stuck_pending_timeout_seconds"`
	// StartTimeoutSeconds bounds how long a placed job the runtime reports as pending may take to
	// start, image pull included, before job_watcher evicts it (reason stuck_pending).
	StartTimeoutSeconds int `yaml:"start_timeout_seconds"`
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
	// HeldJobNoticeSeconds is the least a job waits in the queue before its owner is told, once,
	// that it is held and why (job.held). A job is told when it has waited longer than this and
	// longer than its own estimated duration.
	HeldJobNoticeSeconds int `yaml:"held_job_notice_seconds"`
	// ResourceDisbalanceTolerance is the multiple of a cluster's per-accelerator CPU/memory/
	// storage share a running job may request before the scheduler evicts it for stranding idle
	// accelerators on its own node (see service/scheduler/loop_disbalance.go). Required and
	// positive, like every other setting here. There is no "off": a disabled pass is a second scheduling behaviour to
	// reason about, and a cluster whose accelerators are stranded by one job's shape is broken
	// whether or not an operator remembered to switch the fix on.
	ResourceDisbalanceTolerance float64 `yaml:"resource_disbalance_tolerance"`
	// DefaultTerminationGracePeriodSeconds is used when a job doesn't request its own.
	DefaultTerminationGracePeriodSeconds int `yaml:"default_termination_grace_period_seconds"`
	// MaxTerminationGracePeriodSeconds caps whatever a job requests for itself.
	MaxTerminationGracePeriodSeconds int `yaml:"max_termination_grace_period_seconds"`
	// MaxCheckpointGraceSeconds caps job.checkpoint_grace_seconds — the window a job gets, after
	// being told a policy-class termination is coming, to write a checkpoint it can resume from.
	// Deliberately NOT the same knob as MaxTerminationGracePeriodSeconds: that one bounds an
	// ordinary container shutdown and is measured in seconds, while this one has to be long
	// enough to serialise a real model, and letting one number serve both would either make
	// every kill wait minutes or leave a checkpoint half-written. Required and positive; it is
	// what keeps the promise honest, since a job must not be able to hold contended accelerators
	// indefinitely by claiming it is still saving.
	MaxCheckpointGraceSeconds int `yaml:"max_checkpoint_grace_seconds"`
	// MaxInfrastructureRequeues bounds how many times one experiment may be returned to the
	// queue for free after an infrastructure-class eviction reason (see domain.FaultClass) —
	// the environment failed it, so it costs no max_retries attempt and no accelerator-hours.
	// Required and positive, like every other setting here. There is no "unbounded": a job that
	// keeps landing on broken hardware must eventually stop and say so with the reason that
	// keeps ending it, rather than loop silently forever.
	MaxInfrastructureRequeues int `yaml:"max_infrastructure_requeues"`
	// MaxLogTailLineChars bounds one stored line of a job's reported log tail, applied on ingest
	// (see clusterexchange.splitLongLines): large enough to keep a real compiler error or stack
	// frame intact, small enough that one pathological line can't blow up a stored tail.
	// Lines over this are split, never truncated/dropped.
	MaxLogTailLineChars int `yaml:"max_log_tail_line_chars"`
	// ScaleUpTimeoutSeconds is the global default deadline a SUBMITTED/RUNNING job with an
	// incomplete gang (scheduled_nodes < Nodes()) is given on an autoscaler-enabled cluster
	// before job_watcher evicts it with reason scale_up_timeout and fails it over to the next
	// candidate cluster (see domain.ClusterSettings for the per-cluster override). Must be < 1800:
	// PyTorch's default rendezvous store timeout is 30 minutes, and a scale-up wait past that
	// makes landed ranks time out and fail the job for real, charging a retry for what was only
	// a scheduling failure.
	ScaleUpTimeoutSeconds int `yaml:"scale_up_timeout_seconds"`
	// TriedClusterTTLSeconds is how long a cluster stays out of a job's placement after the
	// environment failed that job on it (see experiments.tried_clusters), unless no other
	// cluster could run the job. Past the TTL the cluster is eligible again. It is a delay: what
	// bounds a job's attempts is its infrastructure-requeue allowance.
	TriedClusterTTLSeconds int `yaml:"tried_cluster_ttl_seconds"`
	// PassTimeoutSeconds bounds one pass of every background loop, so a pass stuck on a store
	// that stopped answering ends and the next one starts.
	PassTimeoutSeconds int `yaml:"pass_timeout_seconds"`
}

// PassTimeout is SchedulerConfig.PassTimeoutSeconds.
func (s SchedulerConfig) PassTimeout() time.Duration {
	return time.Duration(s.PassTimeoutSeconds) * time.Second
}

// DatabaseConfig is how every control-plane process holds its sessions of the relational
// database, whose address is DatabaseURL.
type DatabaseConfig struct {
	StatementTimeoutSeconds                int `yaml:"statement_timeout_seconds"`
	LockTimeoutSeconds                     int `yaml:"lock_timeout_seconds"`
	IdleInTransactionSessionTimeoutSeconds int `yaml:"idle_in_transaction_session_timeout_seconds"`
}

// MaxScaleUpTimeoutSeconds is the hard ceiling both the global default and any per-cluster
// scale_up_timeout_seconds override must stay under. See SchedulerConfig.ScaleUpTimeoutSeconds.
const MaxScaleUpTimeoutSeconds = 1800

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
	// PublicAPIURL is the one address everything outside the control plane reaches this API at:
	// a sandboxed agent from wherever its sandbox runs, and a cluster's runtime from inside its
	// cluster. It is handed to both, so it is never loopback.
	PublicAPIURL string `yaml:"public_api_url"`
	// MetricControllerPort serves the internal controller only; nothing agent-facing.
	MetricControllerPort int `yaml:"metric_controller_port"`
}

// GitProxyURL is the address agents and jobs reach the control plane's git proxy at: the API's
// /git. They push and fetch through it with their own token and never hold the host's.
func (c *Settings) GitProxyURL() string {
	return strings.TrimSuffix(c.Services.PublicAPIURL, "/") + "/git"
}

// ObjectStoreConfig addresses the S3-compatible object store jobs write checkpoints and datasets
// to. The control plane creates one key per job, hands out the address and that key, and never
// sits in the data path: beyond the keys it lists prefixes and measures them, nothing more. The
// key of the section Provider names, with that provider's secret from the environment, creates the
// job keys, so it must be allowed to.
// Expiring a closed platform experiment's prefix is a lifecycle rule configured on the bucket
// itself, not a sweeper here.
type ObjectStoreConfig struct {
	// Provider names the store, which decides how a job's key is created, and the section of that
	// name, the only one set: ObjectStoreProviderMinIO.
	Provider string       `yaml:"provider"`
	MinIO    *MinIOConfig `yaml:"minio"`
	// Endpoint is the address handed to every job and agent, which reach the store from other
	// machines. The control plane sends none of its own requests there.
	Endpoint string `yaml:"endpoint"`
	// ControlPlaneEndpoint is where the control plane itself reaches the store, on its own
	// network, so its reads and writes depend on nothing between it and a rented machine.
	ControlPlaneEndpoint string `yaml:"control_plane_endpoint"`
	Region               string `yaml:"region"`
	Bucket               string `yaml:"bucket"`
	// MaxBytesPerAgent caps how many bytes one agent may hold within one platform experiment,
	// checked at admission against what the store reports right now. Deliberately not enforced
	// mid-write: that would need a gateway in the data path, and a job killed as it saves loses
	// the run it was about to preserve. Required and positive.
	MaxBytesPerAgent int64 `yaml:"max_bytes_per_agent"`
}

// MinIOConfig is the key of a store that speaks the MinIO admin API, whose secret comes from the
// environment.
type MinIOConfig struct {
	AccessKeyID string `yaml:"access_key_id"`
}

// JWTKey is one public key of the identity provider, as its JWKS document lists it. ES256 on
// P-256 and RS256 are accepted.
type JWTKey struct {
	Kid string `yaml:"kid"`
	Kty string `yaml:"kty"`
	Alg string `yaml:"alg"`
	Crv string `yaml:"crv"`
	X   string `yaml:"x"`
	Y   string `yaml:"y"`
	N   string `yaml:"n"`
	E   string `yaml:"e"`
}

// IdentityProvider is one issuer of people's JWTs: who issues them, the audience it issues them
// for this API under, and the public keys that verify its signature. The keys are listed here and
// never fetched, so rotating one is a config change and a restart.
type IdentityProvider struct {
	Issuer   string   `yaml:"issuer"`
	Audience string   `yaml:"audience"`
	JWTKeys  []JWTKey `yaml:"jwt_keys"`
}

// AuthConfig is everything a token is checked against beside the signing secret, which comes from
// the environment. There is no switch that turns the check off: local development authenticates
// with a staff token derived from the signing secret, which expires.
type AuthConfig struct {
	// LocalDevelopment marks a deployment that is one developer's machine, the one place the
	// staff token is accepted. Anywhere else an operator signs in as a staff user.
	LocalDevelopment bool `yaml:"local_development"`
	// IdentityProviders are the issuers whose JWTs name a person. A token is checked against
	// the one provider its issuer claim names, so one provider's key never vouches for
	// another's users. A deployment lists the one it signs people in with; local development
	// lists a key made on the machine beside it. With none listed no JWT is accepted.
	IdentityProviders []IdentityProvider `yaml:"identity_providers"`
	// UIOrigin is the one browser origin allowed to call the API.
	UIOrigin string `yaml:"ui_origin"`
}

// AgentsConfig decides what an agent the platform runs is started from and how it is watched.
// Which account its machine is rented on is the sandbox section's.
type AgentsConfig struct {
	// Image is what every sandboxed agent's machine boots.
	Image string `yaml:"image"`
	// DefaultModel is the model reference an agent runs on when it joins without naming one. It
	// is resolved onto the agent's signup at that moment, so changing it moves new agents only.
	DefaultModel string `yaml:"default_model"`
	// CoordinatorModel is the model reference a sandboxed coordinator runs on when the create
	// request names none. It is written onto the platform experiment when it is created.
	CoordinatorModel string `yaml:"coordinator_model"`
	// BootLimitSeconds is how long an agent sandbox may go unreported after it was asked for, or
	// stay starting after it was first reported, before it is given up on as an exited one is.
	BootLimitSeconds int `yaml:"boot_limit_seconds"`
	// ProgressLimitSeconds is how long a running agent sandbox may go without reporting
	// progress before it is given up on and a restart is spent.
	ProgressLimitSeconds int `yaml:"progress_limit_seconds"`
	// MaxNewInstancesPerAnswer is how many machines one pass over the sandbox provider creates.
	MaxNewInstancesPerAnswer int `yaml:"max_new_instances_per_answer"`
	// MaxAgentSandboxesPerPlatformExperiment is how many machines the platform runs for one platform
	// experiment: its sandboxed agents of every role and its coordinator.
	MaxAgentSandboxesPerPlatformExperiment int `yaml:"max_agentsandboxes_per_platform_experiment"`
	// IdleNoticeMinutes is how long an executor of a running platform experiment may go with no
	// job of its own on the platform before it and the coordinator are told (agent.idle), and how
	// long a run's capacity may hold places beyond its queued jobs before the coordinator is told
	// (capacity.idle).
	IdleNoticeMinutes int `yaml:"idle_notice_minutes"`
}

// ModelsConfig is whose models sandboxed agents run on. The control plane holds each provider's
// credential, from the environment, and makes every model call of a sandboxed agent for it
// (httpapi/modelproxy), so no agent's machine holds the credential. A provider is served while its
// credential is set, and a model of any other is refused.
type ModelsConfig struct {
	Anthropic AnthropicModelsConfig `yaml:"anthropic"`
	OpenAI    OpenAIModelsConfig    `yaml:"openai"`
	// MaxCallsInFlightPerOrg is how many model calls the platform makes for one org at once.
	MaxCallsInFlightPerOrg int `yaml:"max_calls_in_flight_per_org"`
}

// AnthropicModelsConfig is Claude, which the platform calls with a Claude subscription token.
type AnthropicModelsConfig struct {
	// Catalog is every model a call to Anthropic may name, with its price. A call naming another
	// is refused, and each call is priced at it.
	Catalog domain.ModelCatalog `yaml:"catalog"`
}

// OpenAIModelsConfig is OpenAI's models, which the platform calls with a ChatGPT subscription.
type OpenAIModelsConfig struct {
	// Catalog is every model a call to OpenAI may name, with its price, required while the
	// provider's credential is set. A call naming another is refused, and each call is priced at it.
	Catalog domain.ModelCatalog `yaml:"catalog"`
}

// PlanningConfig bounds a draft: a platform experiment being planned from one sentence by an
// agent the platform runs, before a person accepts its proposal. The platform pays for its machine.
type PlanningConfig struct {
	// LimitMinutes is how long a planner has to propose, from the goal and again from each change of it.
	LimitMinutes int `yaml:"limit_minutes"`
	// BaselineLimitMinutes is how long a platform experiment, accepted from a draft or created
	// whole, may stay open: one not running by then is closed. The run's length counts from its
	// start, so this alone bounds the time before, with the time a job of its baseline waits on
	// the platform added (domain.MaxBaselineHold at most).
	BaselineLimitMinutes int `yaml:"baseline_limit_minutes"`
	// MinBaselineMinutes is the fewest minutes an accepted draft may give its baseline: what
	// writing a harness, building its environment and measuring once take at the least.
	MinBaselineMinutes int `yaml:"min_baseline_minutes"`
	// ProposedForDays is how long a proposal waits for a person before the draft is closed.
	ProposedForDays int `yaml:"proposed_for_days"`
	// PlannerKeptMinutes is how long a planner's machine is kept after its answer, so that a goal
	// changed in that time reaches the planner that is still there.
	PlannerKeptMinutes int `yaml:"planner_kept_minutes"`
	// AutoAcceptSeconds is how long a proposal waits before the platform accepts it as proposed,
	// unless a person accepts it sooner, holds it to edit it, changes the goal or closes it. 0 leaves
	// every proposal to a person.
	AutoAcceptSeconds int `yaml:"auto_accept_seconds"`
}

// AccountsConfig bounds what one account creates. Staff are held to neither bound.
type AccountsConfig struct {
	// MaxOrgsPerUser is how many orgs a person may own when they create one.
	MaxOrgsPerUser int `yaml:"max_orgs_per_user"`
	// MaxProjectsPerOrg is how many projects an org may hold.
	MaxProjectsPerOrg int `yaml:"max_projects_per_org"`
}

// RuntimesConfig is what a cluster's runtime is installed with beside services.public_api_url:
// where it sends metrics and which images it runs. All of it is handed out when a cluster is
// registered.
type RuntimesConfig struct {
	MetricsURL string `yaml:"metrics_url"`
	Registry   string `yaml:"registry"`
	ImageTag   string `yaml:"image_tag"`
	// RegistryPlainHTTP says Registry serves plain HTTP with no TLS, as the local dev registry
	// does: the attach command then pulls the chart over HTTP and has the chart configure every
	// node's containerd to pull from it over HTTP.
	RegistryPlainHTTP bool `yaml:"registry_plain_http"`
}

// ContainerRegistryConfig is the registry the images of experiments are pushed to by their
// agents and pulled from by their jobs, and the provider that hands out the logins to it
// (clients/registrycred). Each org has its own repository there. Left out, an experiment records
// only images pulled without a login.
type ContainerRegistryConfig struct {
	// Provider names the registry, and the section of that name, the only one set:
	// RegistryProviderGCP, Google Artifact Registry.
	Provider string             `yaml:"provider"`
	GCP      *GCPRegistryConfig `yaml:"gcp"`
}

// ObjectStoreProviderMinIO is a store that speaks the MinIO admin API (MinIO, Silo, AIStor), which
// creates a job's key as a service account of the configured key.
const ObjectStoreProviderMinIO = "minio"

// RegistryProviderGCP is Google Artifact Registry, logged in to with service account tokens.
const RegistryProviderGCP = "gcp"

// GCPRegistryConfig is the Google Cloud project and location the Artifact Registry repositories
// of orgs are created in, with the service accounts that read and write each.
type GCPRegistryConfig struct {
	Project  string `yaml:"project"`
	Location string `yaml:"location"`
}

// PlainHTTPRegistries is the registries, by host[:port], that serve plain HTTP: runtimes.registry
// when registry_plain_http says so, and none otherwise.
func (c *Settings) PlainHTTPRegistries() []string {
	if !c.Runtimes.RegistryPlainHTTP {
		return nil
	}
	host, _, _ := strings.Cut(c.Runtimes.Registry, "/")
	return []string{host}
}

// The git hosts a deployment can keep its platform experiments' code on.
const (
	GitHostProviderGitHub = "github"
	GitHostProviderPlain  = "plain"
)

// GitHostConfig is the one git host platform experiments keep their code on: Provider names it,
// and the section of that name, the only one set, configures it. ExperimentTemplateRepository is
// the repository on that host every repository the platform creates starts from.
type GitHostConfig struct {
	Provider                     string          `yaml:"provider"`
	ExperimentTemplateRepository string          `yaml:"experiment_template_repository"`
	GitHub                       *GitHubConfig   `yaml:"github"`
	Plain                        *PlainGitConfig `yaml:"plain"`
}

// GitHubConfig is the GitHub App a project's repository is reached through, whose private key
// and client secret come from the environment. All of it is required. ClientID is the App's OAuth
// client, with which an install proves the person finishing it may reach the installation.
type GitHubConfig struct {
	AppID    int64  `yaml:"app_id"`
	AppSlug  string `yaml:"app_slug"`
	ClientID string `yaml:"client_id"`
}

// PlainGitConfig is any host that serves repositories under BaseURL over https, reached with
// Username and Password, or with neither when it asks for none.
type PlainGitConfig struct {
	BaseURL string `yaml:"base_url"`
	// AllowHTTP lets BaseURL be http, for a host on the deployment's own network. The credential
	// then travels in the clear.
	AllowHTTP bool `yaml:"allow_http"`
	// Username is the deployment's one account on a plain host, left out for a host that asks for
	// none. Its password comes from the environment.
	Username string `yaml:"username"`
}

// MinSigningSecretLength keeps the HMAC key at least as long as its output.
const MinSigningSecretLength = 32

// Settings is the top-level parsed config from hypothesisloop.yaml.
type Settings struct {
	AcceleratorTypes  []AcceleratorTypeConfig  `yaml:"accelerator_types"`
	ObjectStore       ObjectStoreConfig        `yaml:"object_store"`
	Quota             QuotaConfig              `yaml:"quota"`
	Scheduler         SchedulerConfig          `yaml:"scheduler"`
	Stages            StagesConfig             `yaml:"stages"`
	Services          ServicesConfig           `yaml:"services"`
	Database          DatabaseConfig           `yaml:"database"`
	Auth              AuthConfig               `yaml:"auth"`
	Agents            AgentsConfig             `yaml:"agents"`
	Models            ModelsConfig             `yaml:"models"`
	Planning          PlanningConfig           `yaml:"planning"`
	Accounts          AccountsConfig           `yaml:"accounts"`
	Runtimes          RuntimesConfig           `yaml:"runtimes"`
	GitHost           GitHostConfig            `yaml:"git_host"`
	ContainerRegistry *ContainerRegistryConfig `yaml:"container_registry"`
	Sandbox           *SandboxConfig           `yaml:"sandbox"`
	CloudCompute      []CloudComputeConfig     `yaml:"cloud_compute"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName         map[string]float64           // accelerator name → AccH rate
	PodResourcesByName map[string]map[string]string // accelerator name → pod_resources, where it has any
}

// NewOrgComputeLimit is the platform compute limit an org is created with.
func (q QuotaConfig) NewOrgComputeLimit() domain.ComputeLimit {
	return domain.ComputeLimit{Org: q.DefaultOrgSharedAccHPerHour, Project: q.DefaultProjectSharedAccHPerHour}
}
