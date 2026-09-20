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
	MarkQueued(ctx context.Context, id, reason string) error
	GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	// GetAgent resolves the submitting agent's Kind, to gate submission against
	// PlatformExperiment.JobSubmitPolicy (see domain.SubmitterPolicy).
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// IsAgentCut reports whether the agent was cut at a stage boundary (see
	// controller/stages.go) — a cut is terminal, and cut agents may not submit new jobs.
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment
	// without a summary in the given platform experiment.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	// CreateHypothesisFinding records the agent's post-run write-up, attached to the
	// hypothesis the completed job tested (see domain.HypothesisFinding).
	CreateHypothesisFinding(ctx context.Context, hypothesisID, platformExperimentID, experimentID, agentID, summary string) (*domain.HypothesisFinding, error)
	// FindingSummary returns the summary filed for a job, empty when none was.
	FindingSummary(ctx context.Context, experimentID string) (string, error)
	ResolveTermination(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string, triedClusterID string) (domain.Termination, error)
	MarkQuotaSettled(ctx context.Context, id string) error
	ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error)
	// GetClusterSettings/PutClusterSettings back PUT /clusters/{id}/settings — see
	// db.ClusterSettingsStore.
	GetClusterSettings(ctx context.Context, clusterID string) (*domain.ClusterSettings, error)
	PutClusterSettings(ctx context.Context, cs *domain.ClusterSettings) error
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (Accelerator-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	// GetAgentQuota looks up (agentID, platformExpID)'s quota row — used by computePriority to
	// compute a dimensionless cost-efficiency term (see domain.AgentQuota.DominantCostFraction).
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	AdmitExperiment(ctx context.Context, exp *domain.Experiment, rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (db.AdmitDecision, error)
}

// WorkloadClient manages backend-executed workloads, potentially across multiple target
// clusters. No DeleteWorkload: a job's desired existence derives from the experiment's own
// status, so "deleting" just means transitioning away from SUBMITTED/RUNNING.
type WorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	// GetFlavorCapacity returns available physical capacity per cluster (see
	// LoopWorkloadClient.GetFlavorCapacity). Used by the operator admit endpoint to verify the
	// target cluster has room, across every dimension, before admitting onto it.
	GetFlavorCapacity(ctx context.Context) (available, desiredFree map[string]domain.Footprint, err error)
	GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error)
	// GetNodeResourceCapacity reports free CPU/memory/storage per node — admission must prove a
	// job fits one node in every dimension, not just in accelerators.
	GetNodeResourceCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)
	// GetClusterIDs maps each connected cluster's name to the id it reports.
	GetClusterIDs(ctx context.Context) (map[string]string, error)
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

// Service is the Scheduler Service that gates, scores, and queues experiments.
type Service struct {
	store        Store
	quota        QuotaService
	workload     WorkloadClient
	novelty      NoveltyDetector
	hypotheses   HypothesisStore
	weights      domain.SchedulingWeights
	credits      domain.QuotaConfig
	loop         Triggerable
	settler      QuotaSettler
	grace        domain.GraceLimits
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
	workload WorkloadClient,
	novelty NoveltyDetector,
	hypotheses HypothesisStore,
	settler QuotaSettler,
	grace domain.GraceLimits,
	metricsDBURL string,
	logger *zap.Logger,
) *Service {
	if settler == nil {
		panic("scheduler: NewService requires a quota settler — it is the only path terminal usage is written by")
	}
	if metricsDBURL == "" || logger == nil {
		panic("scheduler: NewService requires the metrics store URL and a logger")
	}
	return &Service{
		store:        store,
		quota:        quota,
		workload:     workload,
		novelty:      novelty,
		hypotheses:   hypotheses,
		settler:      settler,
		grace:        grace,
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
