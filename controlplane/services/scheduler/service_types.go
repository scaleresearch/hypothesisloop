package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
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
	GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	// GetAgentSignup is the agent's place in the platform experiment, nil when it has none: its
	// role says whether its job is the baseline's.
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	// IsAgentCut reports whether the agent was cut at a stage boundary (see
	// controller/stages.go) — a cut is terminal, and cut agents may not submit new jobs.
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// IsAgentFinished reports whether the agent said it has nothing left to do, which is final.
	IsAgentFinished(ctx context.Context, platformExpID, agentID string) (bool, error)
	// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment
	// without a summary in the given platform experiment.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	// CreateHypothesisFinding records the agent's post-run write-up, attached to the
	// hypothesis the completed job tested (see domain.HypothesisFinding).
	CreateHypothesisFinding(ctx context.Context, hypothesisID, platformExperimentID, experimentID, agentID string, actor domain.Actor, summary string) (*domain.HypothesisFinding, error)
	// NameActors sets on each actor the name it carries now.
	NameActors(ctx context.Context, actors []*domain.Actor) error
	// DescribeJobs sets on each job its agent's name and whether its summary is due.
	DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error
	// FindingSummary returns the summary filed for a job, empty when none was.
	FindingSummary(ctx context.Context, experimentID string) (string, error)
	ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (domain.Termination, error)
	MarkQuotaSettled(ctx context.Context, id string) error
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (Accelerator-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	// GetAgentQuota looks up (agentID, platformExpID)'s quota row — used by computePriority to
	// compute a dimensionless cost-efficiency term (see domain.AgentQuota.DominantCostFraction).
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	AdmitExperiment(ctx context.Context, exp *domain.Experiment, rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (db.AdmitDecision, error)
}

// NoveltyDetector computes the novelty of a hypothesis relative to existing experiments.
type NoveltyDetector interface {
	// ComputeNovelty returns a value in [0,1] where 1.0 is completely novel. hypothesisID
	// identifies a hypothesis registered via the hypotheses registry (services/registry); real
	// duplicate *rejection* happens there (a graph uniqueness constraint), so this score is purely
	// advisory, based on how many active experiments share the same hypothesis_id.
	ComputeNovelty(ctx context.Context, hypothesisID string, existingExperiments []*domain.Experiment) (float64, error)
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
}

// CodeHost is the deployment's git host as admission asks it (githost.Provider): whether an
// address is that of a repository it knows by name.
type CodeHost interface {
	Serves(repository, protocol, host, path string) bool
}

// Service is the Scheduler Service that gates, scores, and queues experiments.
type Service struct {
	store        Store
	quota        QuotaService
	novelty      NoveltyDetector
	hypotheses   HypothesisStore
	codeHost     CodeHost
	weights      domain.SchedulingWeights
	credits      domain.QuotaConfig
	loop         Triggerable
	settler      QuotaSettler
	metricsDBURL string
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
	novelty NoveltyDetector,
	hypotheses HypothesisStore,
	codeHost CodeHost,
	settler QuotaSettler,
	metricsDBURL string,
	logger *zap.Logger,
) *Service {
	if settler == nil {
		panic("scheduler: NewService requires a quota settler — it is the only path terminal usage is written by")
	}
	if codeHost == nil || metricsDBURL == "" || logger == nil {
		panic("scheduler: NewService requires the git host, the metrics store URL and a logger")
	}
	return &Service{
		store:        store,
		quota:        quota,
		novelty:      novelty,
		hypotheses:   hypotheses,
		codeHost:     codeHost,
		settler:      settler,
		metricsDBURL: metricsDBURL,
		logger:       logger,
		weights:      domain.DefaultSchedulingWeights(),
	}
}

// WithQuotaConfig sets the quota constants (rates, limits) used by the scheduler.
func (s *Service) WithQuotaConfig(cfg domain.QuotaConfig) *Service {
	s.credits = cfg
	return s
}

// WithDataStore wires the store job keys are created in and the durable-data ceiling. A ceiling
// of 0 is unlimited, like every other per-job cap, and then the store is never measured.
func (s *Service) WithDataStore(client *objectstore.Client, maxBytesPerAgent int64) *Service {
	s.dataStore = client
	s.maxDataBytesPerAgent = maxBytesPerAgent
	return s
}

// WithLoop wires the scheduler loop so Submit() triggers it after queuing.
func (s *Service) WithLoop(l Triggerable) *Service {
	s.loop = l
	return s
}
