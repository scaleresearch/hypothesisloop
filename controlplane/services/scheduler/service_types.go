package scheduler

import (
	"context"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// submission rate-limit window.
const submissionRateLimitWindow = time.Hour

// ObservedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan for a job's
// first sample — not a trusted clock, just a search-window ceiling no real job's runtime could
// exceed, so the range query stays cheap. Exported so cmd/*/main.go can pass the same bound to
// services/settlement.New, keeping every observed-usage query in this deployment in agreement.
const ObservedMaxLookback = 14 * 24 * time.Hour

// Store is the persistence interface required by the Scheduler.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	CreateExperiment(ctx context.Context, exp *domain.Experiment) error
	// DeleteExperiment removes an experiment row. Used only to unwind the reconcilable anchor
	// created at the start of Submit when the subsequent quota debit fails — the job never
	// existed, so its row must not linger.
	DeleteExperiment(ctx context.Context, id string) error
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateExperimentPriority(ctx context.Context, id string, score float64) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	MarkQueued(ctx context.Context, id string) error
	GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	// IsAgentHeld reports whether the agent is under a phase-2 budget hold for this platform
	// experiment (see controller/phase2.go's TriggerPhase2) — held agents may not submit new jobs.
	IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error)
	// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment
	// without a summary in the given platform experiment.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
	// CreateHypothesisFinding records the agent's post-run write-up, attached to the
	// hypothesis the completed job tested (see domain.HypothesisFinding).
	CreateHypothesisFinding(ctx context.Context, hypothesisID, experimentID, agentID, summary string) (*domain.HypothesisFinding, error)
	// CountRecentSubmissions counts experiments created by the agent in the platform
	// experiment since the given time. Used to enforce hourly submission rate limits.
	CountRecentSubmissions(ctx context.Context, agentID, platformExpID string, since time.Time) (int, error)
	// TransitionStatus atomically updates status only when the current status matches
	// from. Returns true if the row was updated, false if already changed by a concurrent
	// request.
	TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error)
	// MarkSubmitted transitions id to SUBMITTED and persists clusterName atomically with that
	// transition (see LoopStore.MarkSubmitted). Used by the operator admit endpoint so a manual
	// override goes through the same capacity-claiming transition as normal admission.
	MarkSubmitted(ctx context.Context, id, clusterName string) error
	// UpsertPendingReservation/DeletePendingReservation manage a durable pending-capacity claim
	// (see pending_capacity_reservations' schema comment) — used by the operator admit endpoint
	// so a manual override claims/releases capacity the same way normal admission does.
	UpsertPendingReservation(ctx context.Context, experimentID, clusterName string, fp domain.Footprint) error
	DeletePendingReservation(ctx context.Context, id string) error
	// ListPendingReservationsByCluster sums every outstanding reservation per cluster — used by
	// the operator admit endpoint to compute a target cluster's in-flight footprint the same way
	// tick() does (see loop_tick.go step 2b). GetFlavorCapacity already reflects every
	// SUBMITTED/ADMITTED/RUNNING pod that actually exists on a node, so pending reservations
	// (which by construction only cover the not-yet-scheduled gap) are the only additional
	// subtraction needed — subtracting occupied-status experiments on top of that would
	// double-count the ones whose pod already exists.
	ListPendingReservationsByCluster(ctx context.Context) (map[string]domain.Footprint, error)
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (Accelerator-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	// GetAgentQuota looks up (agentID, platformExpID)'s quota row — used by computePriority to
	// compute a dimensionless cost-efficiency term (see domain.AgentQuota.DominantCostFraction),
	// comparable across CPU/Accelerator/RAM/storage jobs instead of raw, dimensionally-incompatible hours.
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	CheckAndDebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
	// RefundQuota overwrites experimentID's own usage with amount (its observed cost, an
	// absolute set — see metricsdb.UsageTracker.SetObserved), not a delta to subtract.
	RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
	// CorrectReservation overwrites experimentID's own not-yet-final reservation with a new
	// absolute amount (see metricsdb.UsageTracker.SetReservation) — used on preemption requeue
	// to replace a stale original-estimate reservation with the revised, shortened one.
	CorrectReservation(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
}

// WorkloadClient manages backend-executed workloads, potentially across multiple target
// clusters. There is deliberately no DeleteWorkload: a job's desired existence is
// derived entirely from the experiment's own status, so "deleting" a workload just means
// transitioning status away from SUBMITTED/ADMITTED/RUNNING (see workload.Backend doc).
type WorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	// ClusterNames returns the configured target cluster names (stable order). Used by
	// admission to pick a cluster for a newly-admitted experiment. Single-cluster
	// deployments return exactly one name ("default").
	ClusterNames() []string
	// GetFlavorCapacity returns available physical capacity as a canonical domain.Footprint per
	// cluster, guaranteed[cluster] and burst[cluster] (see LoopWorkloadClient.GetFlavorCapacity).
	// Used by the operator admit endpoint to verify the requested target cluster actually has
	// room, jointly across every dimension the job requests, before admitting onto it.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error)
}

// NoveltyDetector computes the novelty of a hypothesis relative to existing experiments.
type NoveltyDetector interface {
	// ComputeNovelty returns a value in [0,1] where 1.0 is completely novel. hypothesisID
	// identifies a row registered via the hypotheses registry (services/registry); real
	// duplicate *rejection* happens there (a DB unique constraint on normalized text) — this
	// score is purely advisory, based on how many active experiments already share the same
	// hypothesis_id. See services/dedup.
	ComputeNovelty(ctx context.Context, hypothesisID string, existingExperiments []*domain.Experiment) (float64, error)
}

// Triggerable is anything that can wake the scheduler loop.
type Triggerable interface {
	Trigger()
}

// HypothesisStore resolves a hypothesis_id to the hypothesis it names. Submissions must
// reference an already-registered hypothesis (see services/registry.RegisterHypothesis) —
// the scheduler itself never creates hypotheses, only validates the reference.
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

	// metricsDBURL, observedGapCap, observedStep configure Cancel()'s observed-elapsed query
	// (see metricsdb.ObservedElapsedHours) — the same GreptimeDB-backed source of truth
	// Controller uses for every other termination path, not a separate wall-clock calc.
	// GreptimeDB is a required dependency of this deployment: a query error propagates rather
	// than falling back to a wall-clock guess.
	metricsDBURL   string
	observedGapCap time.Duration
	observedStep   time.Duration
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

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step parameters Cancel() uses to
// compute observed-elapsed time — see metricsdb.ObservedElapsedHours. Pass the same values the
// Controller in this deployment uses (silenceMultiplier × defaultReportInterval for the cap, and
// defaultReportInterval for the step) so a job cancelled by a user and a job evicted
// automatically are held to the same definition of "how long did this really run".
func (s *Service) WithObservedTimeConfig(metricsDBURL string, gapCap, step time.Duration) *Service {
	s.metricsDBURL = metricsDBURL
	s.observedGapCap = gapCap
	s.observedStep = step
	return s
}

// WithLoop wires the scheduler loop so Submit() triggers it after queuing.
func (s *Service) WithLoop(l Triggerable) *Service {
	s.loop = l
	return s
}
