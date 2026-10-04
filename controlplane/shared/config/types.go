// Package config loads hypothesisloop.yaml and exposes a typed Config used across all services.
package config

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	// DefaultQuotaTier is the tier of a signup that states none, in a platform experiment that
	// states no default_quota_tier: guaranteed or burst_only.
	DefaultQuotaTier string `yaml:"default_quota_tier"`
	// MaxPlatformExperimentDays is the longest a platform experiment may run, from the start of
	// its run to its end. Required and positive: until its end an experiment may rent machines.
	MaxPlatformExperimentDays int `yaml:"max_platform_experiment_days"`

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
	// accelerators on its own node (see services/scheduler/loop_disbalance.go). Required and
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
	// (see clusteragentapi.splitLongLines): large enough to keep a real compiler error or stack
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
	// a hosted agent from wherever its launcher rents, and a cluster's runtime from inside its
	// cluster. It is handed to both, so it is never loopback.
	PublicAPIURL string `yaml:"public_api_url"`
	// MetricControllerPort serves the internal controller only; nothing agent-facing.
	MetricControllerPort int    `yaml:"metric_controller_port"`
	MetricsDBURL         string `yaml:"metrics_db_url"`
	// GraphDBURL is the Memgraph bolt address holding the hypothesis graph (shared/graphdb).
	// Only control-service connects to it.
	GraphDBURL string `yaml:"graph_db_url"`
}

// DataStoreConfig addresses the S3-compatible object store jobs write checkpoints and datasets
// to. The control plane creates one key per job, hands out the address and that key, and never
// sits in the data path: beyond the keys it lists prefixes and measures them, nothing more. The
// configured key creates the job keys, so it must be allowed to create service accounts (the
// store's root user does). Expiring a closed platform
// experiment's prefix is a lifecycle rule configured on the bucket itself, not a sweeper here.
type DataStoreConfig struct {
	// Endpoint is the address handed to every job and agent, which reach the store from other
	// machines. The control plane sends none of its own requests there.
	Endpoint string `yaml:"endpoint"`
	// ControlPlaneEndpoint is where the control plane itself reaches the store, on its own
	// network, so its reads and writes depend on nothing between it and a rented machine.
	ControlPlaneEndpoint string `yaml:"control_plane_endpoint"`
	Region               string `yaml:"region"`
	Bucket               string `yaml:"bucket"`
	AccessKeyID          string `yaml:"access_key_id"`
	SecretAccessKey      string `yaml:"secret_access_key"`
	// MaxBytesPerAgent caps how many bytes one agent may hold within one platform experiment,
	// checked at admission against what the store reports right now. Deliberately not enforced
	// mid-write: that would need a gateway in the data path, and a job killed as it saves loses
	// the run it was about to preserve. Required and positive.
	MaxBytesPerAgent int64 `yaml:"max_bytes_per_agent"`
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

// AuthConfig is everything a token is checked against. There is no switch that turns the check
// off: local development authenticates with a staff token derived from SigningSecret.
type AuthConfig struct {
	// IdentityProviders are the issuers whose JWTs name a person. A token is checked against
	// the one provider its issuer claim names, so one provider's key never vouches for
	// another's users. A deployment lists the one it signs people in with; local development
	// lists a key made on the machine beside it. With none listed no JWT is accepted.
	IdentityProviders []IdentityProvider `yaml:"identity_providers"`
	// SigningSecret derives every hls_ token the platform hands to what it launches. It is a
	// secret of the deployment: the settings file names the environment variable that holds it.
	SigningSecret string `yaml:"signing_secret"`
	// UIOrigin is the one browser origin allowed to call the API.
	UIOrigin string `yaml:"ui_origin"`
}

// AgentsConfig decides where an agent the platform runs is started and from what. Each value is
// read when an agent joins or when its launcher asks, and none of it names a provider: which
// account a launcher rents from is that launcher's own config.
type AgentsConfig struct {
	// Launcher is the launcher a newly hosted agent is given to. It is written onto the agent's
	// signup, so changing it moves new agents only.
	Launcher string `yaml:"launcher"`
	// Image is what every hosted agent's machine boots.
	Image string `yaml:"image"`
	// DefaultModel is the model an agent runs on when it joins without naming one. It is
	// resolved onto the agent's signup at that moment, so changing it moves new agents only.
	DefaultModel string `yaml:"default_model"`
	// BootLimitSeconds is how long a hosted machine may go unreported after it was asked for, or
	// stay starting after it was first reported, before it is given up on as an exited one is.
	BootLimitSeconds int `yaml:"boot_limit_seconds"`
	// ProgressLimitSeconds is how long a running hosted machine may go without reporting
	// progress before it is given up on and a restart is spent.
	ProgressLimitSeconds int `yaml:"progress_limit_seconds"`
	// MaxNewInstancesPerAnswer is how many machines one answer asks a launcher to create.
	MaxNewInstancesPerAnswer int `yaml:"max_new_instances_per_answer"`
	// MaxHostedPerPlatformExperiment is how many machines the platform runs for one platform
	// experiment: its hosted agents of every role and its coordinator.
	MaxHostedPerPlatformExperiment int `yaml:"max_hosted_per_platform_experiment"`
}

// PlanningConfig bounds a draft: a platform experiment being planned from one sentence by an
// agent the platform runs, before a person accepts its proposal. The platform pays for its machine.
type PlanningConfig struct {
	// LimitMinutes is how long a planner has to propose, from the goal or from a revision.
	LimitMinutes int `yaml:"limit_minutes"`
	// BaselineLimitMinutes is how long a platform experiment, accepted from a draft or created
	// whole, may stay open: one not running by then is closed. The run's length counts from its
	// start, so this alone bounds the time before, with the time a job of its baseline waits on
	// the platform added (domain.MaxEnvironmentBuildTime at most).
	BaselineLimitMinutes int `yaml:"baseline_limit_minutes"`
	// MinBaselineMinutes is the fewest minutes an accepted draft may give its baseline: what
	// writing a harness, building its environment and measuring once take at the least.
	MinBaselineMinutes int `yaml:"min_baseline_minutes"`
	// ProposedForDays is how long a proposal waits for a person before the draft is closed.
	ProposedForDays int `yaml:"proposed_for_days"`
	// PlannerKeptMinutes is how long a planner's machine is kept after its answer, so that a
	// revision asked for in that time reaches the planner that is still there.
	PlannerKeptMinutes int `yaml:"planner_kept_minutes"`
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
}

// EnvironmentsConfig is where the images a repository's .hypothesisloop/Dockerfile declares are
// pushed by the cluster each build is assigned to, and the two service accounts whose tokens the
// control plane hands out for them (shared/registrycred). Left out, the deployment builds no
// environments: no cluster counts as building, so a job whose commit declares one is refused.
type EnvironmentsConfig struct {
	// Repository is the whole address environment images are pushed to by digest,
	// host/path/name, in a registry no one pulls from without a token.
	Repository string `yaml:"repository"`
	// Pusher is the email of the service account that may write to Repository, and Puller of
	// the one that may read it. The control plane's own Google identity may mint tokens of both.
	Pusher string `yaml:"pusher"`
	Puller string `yaml:"puller"`
}

// JobImageConfig is one image the deployment offers a job to run in. It is named by image, a
// whole reference, or by repository, a name under runtimes.registry at runtimes.image_tag: one
// this deployment built and pushed with its own.
type JobImageConfig struct {
	Name       string   `yaml:"name"`
	Image      string   `yaml:"image"`
	Repository string   `yaml:"repository"`
	Carries    string   `yaml:"carries"`
	Suits      []string `yaml:"suits"`
}

// OfferedJobImages is the configured job images with their references resolved.
func (c *Config) OfferedJobImages() []domain.JobImage {
	images := make([]domain.JobImage, 0, len(c.JobImages))
	for _, i := range c.JobImages {
		reference := i.Image
		if i.Repository != "" {
			reference = c.Runtimes.Registry + "/" + i.Repository + ":" + c.Runtimes.ImageTag
		}
		images = append(images, domain.JobImage{Name: i.Name, Image: reference, Carries: i.Carries, Suits: i.Suits})
	}
	return images
}

// GitHubConfig is the GitHub App a project's repository is reached through, on a deployment whose
// git host is GitHub. Set, all of it is required.
type GitHubConfig struct {
	AppID   int64  `yaml:"app_id"`
	AppSlug string `yaml:"app_slug"`
	// ClientID and ClientSecret are the App's own OAuth pair. They prove, once per attach, that
	// the person attaching a repository can push to it.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// PrivateKey is the PEM GitHub issued for the App. ClientSecret and PrivateKey are secrets of
	// the deployment: the settings file names the environment variables that hold them.
	PrivateKey string `yaml:"private_key"`
}

// Configured reports whether the deployment has a GitHub App.
func (c GitHubConfig) Configured() bool { return c != GitHubConfig{} }

// The git hosts a deployment can keep its platform experiments' code on.
const (
	GitProviderGitHub = "github"
	GitProviderPlain  = "plain"
)

// GitConfig says which git host platform experiments keep their code on. github is the GitHub
// App of the github section. plain is any host that serves repositories under BaseURL over
// https, reached with Username and Password, or with neither when it asks for none.
type GitConfig struct {
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	// AllowHTTP lets BaseURL be http, for a host on the deployment's own network. The credential
	// then travels in the clear.
	AllowHTTP bool `yaml:"allow_http"`
	// Username and Password are the deployment's one credential on a plain host. The settings
	// file names the environment variables that hold them.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// MinSigningSecretLength keeps the HMAC key at least as long as its output.
const MinSigningSecretLength = 32

// Config is the top-level parsed config from hypothesisloop.yaml.
type Config struct {
	AcceleratorTypes []AcceleratorTypeConfig `yaml:"accelerator_types"`
	DataStore        DataStoreConfig         `yaml:"data_store"`
	Quota            QuotaConfig             `yaml:"quota"`
	Scheduler        SchedulerConfig         `yaml:"scheduler"`
	Stages           StagesConfig            `yaml:"stages"`
	Services         ServicesConfig          `yaml:"services"`
	Auth             AuthConfig              `yaml:"auth"`
	Agents           AgentsConfig            `yaml:"agents"`
	Planning         PlanningConfig          `yaml:"planning"`
	Accounts         AccountsConfig          `yaml:"accounts"`
	Runtimes         RuntimesConfig          `yaml:"runtimes"`
	JobImages        []JobImageConfig        `yaml:"job_images"`
	GitHub           GitHubConfig            `yaml:"github"`
	Git              GitConfig               `yaml:"git"`
	Environments     *EnvironmentsConfig     `yaml:"environments"`

	// Derived maps built by Load() for fast lookup — not in YAML.
	RateByName         map[string]float64           // accelerator name → AccH rate
	PodResourcesByName map[string]map[string]string // accelerator name → pod_resources, where it has any
}
