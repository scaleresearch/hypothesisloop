package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// submission rate-limit window.
const submissionRateLimitWindow = time.Hour

// Store is the persistence interface required by the Scheduler.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	// CountExperiments returns the total matching filter, ignoring Limit/Offset/Sort — used to
	// populate list-experiments' X-Total-Count header for pagination.
	CountExperiments(ctx context.Context, filter domain.ExperimentFilter) (int, error)
	// ExperimentStats counts the matching set grouped by status/tier/eviction reason, for
	// callers that need whole-set totals rather than a page of rows.
	ExperimentStats(ctx context.Context, filter domain.ExperimentFilter, groupBy string) (*domain.ExperimentStats, error)
	UpdateExperimentPriority(ctx context.Context, id string, score float64) error
	// GetRunningAndQueued is the jobs of one platform experiment that have not ended.
	GetRunningAndQueued(ctx context.Context, platformExpID string) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	// GetAgentSignup is the agent's place in the platform experiment, nil when it has none: its
	// role says whether its job is the baseline's.
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	// IsAgentCut reports whether the agent was cut at a stage boundary (see
	// controller/stages.go) — a cut is terminal, and cut agents may not submit new jobs.
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// IsAgentFinished reports whether the agent said it has nothing left to do, which is final.
	IsAgentFinished(ctx context.Context, platformExpID, agentID string) (bool, error)
	// CreateHypothesisFinding records the agent's post-run write-up, attached to the
	// hypothesis the completed job tested (see domain.HypothesisFinding).
	CreateHypothesisFinding(ctx context.Context, hypothesisID, platformExperimentID, experimentID, agentID string, actor domain.Actor, summary string) (*domain.HypothesisFinding, error)
	// NameActors sets on each actor the name it carries now.
	NameActors(ctx context.Context, actors []*domain.Actor) error
	// DescribeJobs sets on each job its agent's name.
	DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error
	// FindingSummary returns the summary filed for a job, empty when none was.
	FindingSummary(ctx context.Context, experimentID string) (string, error)
	// GetEnvironment is the environment of key, nil when there is none.
	GetEnvironment(ctx context.Context, key string) (*domain.Environment, error)
	// AwaitEnvironmentBuild has an agent told once the build of key it asked for ends
	// (db.EnvironmentsStore.AwaitEnvironmentBuild).
	AwaitEnvironmentBuild(ctx context.Context, key, peID, agentID, codeRef, image string) error
	// ProjectOrg and RegisteredClusters say which clusters take a job's org's jobs, and which
	// image each runs.
	ProjectOrg(ctx context.Context, projectID string) (string, error)
	RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error)
	ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (domain.Termination, error)
	MarkQuotaSettled(ctx context.Context, id string) error
	// CompletedJobIDs and ListSignupsByRole are what the standings rank: the completed jobs of a
	// platform experiment, and its agents of a role.
	CompletedJobIDs(ctx context.Context, platformExpID string) (map[string]bool, error)
	ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error)
}

// Standings reads each agent's best on a metric from the metrics store
// (metricsdb.BestPerAgentOnMetric), the one definition of who is ahead.
type Standings interface {
	BestPerAgentOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, completed map[string]bool) (map[string]metricsdb.AgentBest, []string, error)
}

type metricsDBStandings struct{ url string }

func (m metricsDBStandings) BestPerAgentOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, completed map[string]bool) (map[string]metricsdb.AgentBest, []string, error) {
	return metricsdb.BestPerAgentOnMetric(ctx, m.url, pe, metric, completed)
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (Accelerator-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	// GetAgentQuota looks up (agentID, platformExpID)'s quota row — used by computePriority to
	// compute a dimensionless cost-efficiency term (see domain.AgentQuota.DominantCostFraction).
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	AdmitExperiment(ctx context.Context, exp *domain.Experiment, rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (db.AdmitDecision, error)
	// Describe sets on an open platform experiment why it is not running yet and what reader does
	// about it (StatusReason): the one wording of it, which a refused submit repeats.
	Describe(ctx context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error
}

// Triggerable is anything that can wake the scheduler loop.
type Triggerable interface {
	Trigger()
}

// HypothesisStore resolves a hypothesis_id to the hypothesis it names, nil when there is none
// (the graph store). Submissions must reference an already-registered hypothesis; the scheduler
// never creates hypotheses, only validates the reference.
type HypothesisStore interface {
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	Links(ctx context.Context, id string) (domain.HypothesisLinks, error)
}

// CodeHost is the deployment's git host as admission asks it (githost.Provider): whether an
// address is that of a repository it knows by name.
type CodeHost interface {
	Serves(repository, protocol, host, path string) bool
}

// Environments finds the environment a commit declares (environments.Service.Declared): nil when
// it declares none, a *domain.EnvironmentFileError for a file the platform does not build and
// domain.ErrNoEnvironmentBuilder while no cluster builds for the org. Find reads it as it is and
// builds nothing (environments.Service.Find).
type Environments interface {
	Declared(ctx context.Context, orgID, projectID, repository, ref, image string) (*domain.Environment, error)
	Find(ctx context.Context, orgID, projectID, repository, ref, image string) (env *domain.Environment, declared bool, err error)
}

// InstalledCapacity is what each connected cluster and each of its nodes has installed, whether
// it scales up and whether it runs a job over several nodes, as the admission loop reads them
// (ClusterCapacity).
type InstalledCapacity interface {
	GetTotalCapacity(ctx context.Context) (map[string]domain.Footprint, error)
	GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetAutoscalerCapability(ctx context.Context) (map[string]bool, error)
	GetMultiNodeCapability(ctx context.Context) (map[string]bool, error)
}

// Service is the Scheduler Service that gates, scores, and queues experiments.
type Service struct {
	store        Store
	quota        QuotaService
	hypotheses   HypothesisStore
	codeHost     CodeHost
	environments Environments
	clusters     InstalledCapacity
	// jobImages is the images the deployment offers a job to run in, which a job names by name.
	jobImages    []domain.JobImage
	weights      domain.SchedulingWeights
	credits      domain.QuotaConfig
	loop         Triggerable
	builds       BuildAssigner
	pulls        RegistryPulls
	settler      QuotaSettler
	metricsDBURL string
	standings    Standings
	logger       *zap.Logger
	// dataStore creates each submitted job's key. With maxDataBytesPerAgent it is also the
	// durable-data ceiling: how much one agent already holds in this platform experiment is asked
	// of the store at admission and nowhere else. Not enforced mid-write — that needs a gateway in
	// the data path, and a job killed as it saves loses the run it was about to preserve.
	dataStore            *objectstore.Client
	maxDataBytesPerAgent int64
}

// NewService constructs a Scheduler Service with the provided dependencies.
func NewService(
	store Store,
	quota QuotaService,
	hypotheses HypothesisStore,
	codeHost CodeHost,
	clusters InstalledCapacity,
	settler QuotaSettler,
	metricsDBURL string,
	logger *zap.Logger,
) *Service {
	if settler == nil {
		panic("scheduler: NewService requires a quota settler — it is the only path terminal usage is written by")
	}
	if codeHost == nil || clusters == nil || metricsDBURL == "" || logger == nil {
		panic("scheduler: NewService requires the git host, the clusters' installed capacity, the metrics store URL and a logger")
	}
	return &Service{
		store:        store,
		quota:        quota,
		hypotheses:   hypotheses,
		codeHost:     codeHost,
		clusters:     clusters,
		settler:      settler,
		metricsDBURL: metricsDBURL,
		standings:    metricsDBStandings{url: metricsDBURL},
		logger:       logger,
		weights:      domain.DefaultSchedulingWeights(),
	}
}

// WithQuotaConfig sets the quota constants (rates, limits) used by the scheduler.
func (s *Service) WithQuotaConfig(cfg domain.QuotaConfig) *Service {
	s.credits = cfg
	return s
}

// WithJobImages sets the images the deployment offers a job to run in.
func (s *Service) WithJobImages(images []domain.JobImage) *Service {
	s.jobImages = images
	return s
}

// WithDataStore wires the store job keys are created in and the durable-data ceiling. A ceiling
// of 0 is unlimited, like every other per-job cap, and then the store is never measured.
func (s *Service) WithDataStore(client *objectstore.Client, maxBytesPerAgent int64) *Service {
	s.dataStore = client
	s.maxDataBytesPerAgent = maxBytesPerAgent
	return s
}

// WithEnvironments wires what finds the environment a job's commit declares.
func (s *Service) WithEnvironments(environments Environments) *Service {
	s.environments = environments
	return s
}

// WithLoop wires the scheduler loop so Submit() triggers it after queuing.
func (s *Service) WithLoop(l Triggerable) *Service {
	s.loop = l
	return s
}
