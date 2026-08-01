package scheduler

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// submission rate-limit window.
const submissionRateLimitWindow = time.Hour

// ObservedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan for a job's
// first sample — a search-window ceiling no real job's runtime could exceed, keeping the range
// query cheap. Exported so cmd/*/main.go can pass the same bound to services/settlement.New.
const ObservedMaxLookback = 14 * 24 * time.Hour

// Store is the persistence interface required by the Scheduler.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateExperimentPriority(ctx context.Context, id string, score float64) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	MarkQueued(ctx context.Context, id, reason string) error
	GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	// IsAgentCut reports whether the agent was cut at a stage boundary (see
	// controller/stages.go) — a cut is terminal, and cut agents may not submit new jobs.
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment
	// without a summary in the given platform experiment.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	// CreateHypothesisFinding records the agent's post-run write-up, attached to the
	// hypothesis the completed job tested (see domain.HypothesisFinding).
	CreateHypothesisFinding(ctx context.Context, hypothesisID, experimentID, agentID, summary string) (*domain.HypothesisFinding, error)
	// CountRecentSubmissions enforces hourly submission rate limits.
	CountRecentSubmissions(ctx context.Context, agentID, platformExpID string, since time.Time) (int, error)
	// TransitionStatus atomically updates status only when current status matches from.
	// Returns true if updated, false if already changed by a concurrent request.
	TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error)
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	MarkQuotaSettled(ctx context.Context, id string) error
	ClaimSubmitted(ctx context.Context, id, clusterName string, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error)
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (Accelerator-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	// GetAgentQuota looks up (agentID, platformExpID)'s quota row — used by computePriority to
	// compute a dimensionless cost-efficiency term (see domain.AgentQuota.DominantCostFraction).
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	AdmitExperiment(ctx context.Context, exp *domain.Experiment) error
	ReserveAdmittedFlavor(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64) error
}

// WorkloadClient manages backend-executed workloads, potentially across multiple target
// clusters. No DeleteWorkload: a job's desired existence derives from the experiment's own
// status, so "deleting" just means transitioning away from SUBMITTED/ADMITTED/RUNNING.
type WorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	// GetFlavorCapacity returns available physical capacity per cluster (see
	// LoopWorkloadClient.GetFlavorCapacity). Used by the operator admit endpoint to verify the
	// target cluster has room, across every dimension, before admitting onto it.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error)
	GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)
}

// NoveltyDetector computes the novelty of a hypothesis relative to existing experiments.
type NoveltyDetector interface {
	// ComputeNovelty returns a value in [0,1] where 1.0 is completely novel. hypothesisID
	// identifies a row registered via the hypotheses registry (services/registry); real
	// duplicate *rejection* happens there (a DB unique constraint), so this score is purely
	// advisory, based on how many active experiments share the same hypothesis_id.
	ComputeNovelty(ctx context.Context, hypothesisID string, existingExperiments []*domain.Experiment) (float64, error)
}

// Triggerable is anything that can wake the scheduler loop.
type Triggerable interface {
	Trigger()
}

// HypothesisStore resolves a hypothesis_id to the hypothesis it names. Submissions must
// reference an already-registered hypothesis; the scheduler never creates hypotheses, only
// validates the reference.
type HypothesisStore interface {
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
}

// Service is the Scheduler Service that gates, scores, and queues experiments.
type Service struct {
	store      Store
	quota      QuotaService
	workload   WorkloadClient
	novelty    NoveltyDetector
	hypotheses HypothesisStore
	weights    domain.SchedulingWeights
	credits    domain.CreditConfig
	loop       Triggerable
	settler    QuotaSettler
}

// NewService constructs a Scheduler Service with the provided dependencies and
// default weights/credit config.
func NewService(
	store Store,
	quota QuotaService,
	workload WorkloadClient,
	novelty NoveltyDetector,
	hypotheses HypothesisStore,
) *Service {
	return &Service{
		store:      store,
		quota:      quota,
		workload:   workload,
		novelty:    novelty,
		hypotheses: hypotheses,
		weights:    domain.DefaultSchedulingWeights(),
		credits:    domain.DefaultCreditConfig(),
	}
}

// WithQuotaConfig overrides the quota constants (rates, limits) used by the scheduler.
func (s *Service) WithQuotaConfig(cfg domain.QuotaConfig) *Service {
	s.credits = cfg
	return s
}

// WithQuotaSettler wires the one canonical metrics-based terminal accounting path.
func (s *Service) WithQuotaSettler(settler QuotaSettler) *Service {
	s.settler = settler
	return s
}

// WithLoop wires the scheduler loop so Submit() triggers it after queuing.
func (s *Service) WithLoop(l Triggerable) *Service {
	s.loop = l
	return s
}
