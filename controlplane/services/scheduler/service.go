package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// submission rate-limit window.
const submissionRateLimitWindow = time.Hour

// observedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan for a job's
// first sample — not a trusted clock, just a search-window ceiling no real job's runtime could
// exceed, so the range query stays cheap.
const observedMaxLookback = 14 * 24 * time.Hour

// Store is the persistence interface required by the Scheduler.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	CreateExperiment(ctx context.Context, exp *domain.Experiment) error
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateExperimentPriority(ctx context.Context, id string, score float64) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	MarkQueued(ctx context.Context, id string) error
	GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error)
	// Platform experiment lookups for admission validation.
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
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
}

// QuotaService handles experiment-scoped quota checks and debits, across every resource
// dimension (GPU-hours, CPU-core-hours, RAM-GB-hours, storage-GB-hours).
type QuotaService interface {
	CheckAndDebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
	// RefundQuota overwrites experimentID's own usage with amount (its observed cost, an
	// absolute set — see metricsdb.UsageTracker.SetObserved), not a delta to subtract.
	RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
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

// Submit runs the admission gate for an experiment. On success the experiment is
// transitioned to QUEUED. On rejection an *AdmissionError is returned.
func (s *Service) Submit(ctx context.Context, exp *domain.Experiment) error {
	// 1. Structural validation.
	if err := ValidateExperiment(exp, s.credits); err != nil {
		return err
	}

	// Default capacity tier so quota debit and DB insert agree.
	if exp.CapacityTier == "" {
		exp.CapacityTier = domain.CapacityGuaranteed
	}

	// 2. Validate platform experiment reference.
	if exp.PlatformExperimentID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "platform_experiment_id is required"}
	}
	pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("scheduler: get platform experiment: %w", err)
	}
	if pe == nil {
		return &AdmissionError{
			Reason:  "experiment_not_found",
			Message: fmt.Sprintf("platform experiment %s not found", exp.PlatformExperimentID),
		}
	}
	if pe.Status != domain.PlatformExpRunning {
		return &AdmissionError{
			Reason:  "experiment_not_running",
			Message: fmt.Sprintf("platform experiment is %s, not running", pe.Status),
		}
	}
	signedUp, err := s.store.IsSignedUp(ctx, exp.PlatformExperimentID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: check signup: %w", err)
	}
	if !signedUp {
		return &AdmissionError{
			Reason:  "not_signed_up",
			Message: "agent has not signed up for this platform experiment",
		}
	}

	// 3a. Summary gate: block submissions when the agent has COMPLETED experiments without
	// a summary. Only successful runs are gated — FAILED/EVICTED are excluded because
	// documenting infrastructure failures adds little signal and unfairly penalises noisy
	// environments. Agents unblock themselves via POST /experiments/{id}/summary.
	unsummarized, err := s.store.HasUnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("scheduler: check unsummarized terminal: %w", err)
	}
	if unsummarized {
		return &AdmissionError{
			Reason:  ReasonSummaryRequired,
			Message: "agent has completed experiments without summaries — write summaries via POST /experiments/{id}/summary before submitting new jobs",
		}
	}

	// 3b. Rate limit: cap submissions per hour to prevent queue flooding.
	if s.credits.MaxSubmissionsPerHour > 0 {
		since := time.Now().UTC().Add(-submissionRateLimitWindow)
		count, err := s.store.CountRecentSubmissions(ctx, exp.AgentID, exp.PlatformExperimentID, since)
		if err != nil {
			return fmt.Errorf("scheduler: count recent submissions: %w", err)
		}
		if count >= s.credits.MaxSubmissionsPerHour {
			return &AdmissionError{
				Reason:  ReasonRateLimited,
				Message: fmt.Sprintf("agent has submitted %d experiments in the last hour (limit: %d)", count, s.credits.MaxSubmissionsPerHour),
			}
		}
	}

	// 2b. Validate hypothesis reference: every experiment must test a specific,
	// previously-registered hypothesis (POST /registry/hypotheses) rather than restating
	// free text ad hoc. Denormalize its text onto the experiment for cheap reads.
	if exp.HypothesisID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "hypothesis_id is required — register or retrieve one via POST /registry/hypotheses"}
	}
	hyp, err := s.hypotheses.GetHypothesis(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("scheduler: get hypothesis: %w", err)
	}
	if hyp == nil {
		return &AdmissionError{
			Reason:  ReasonMalformed,
			Message: fmt.Sprintf("hypothesis %s not found — register it first via POST /registry/hypotheses", exp.HypothesisID),
		}
	}
	exp.Hypothesis = hyp.Text

	// 3. Duplicate check — must happen before any side effects (quota debit).
	// Agents may pre-register via the registry service, so the row may already exist.
	// Only QUEUED experiments may be re-submitted (to refresh priority); all other
	// states are either active or terminal and cannot be rewound without stopping the
	// underlying backend workload.
	existing, err := s.store.GetExperiment(ctx, exp.ID)
	if err != nil {
		return fmt.Errorf("scheduler: get experiment: %w", err)
	}
	if existing != nil && existing.Status != domain.StatusQueued {
		return &AdmissionError{
			Reason:  ReasonDuplicate,
			Message: fmt.Sprintf("experiment %s already exists with status %s", exp.ID, existing.Status),
		}
	}

	// 4. Compute estimated cost if not already set. GPU-hours is the primary/always-populated
	// dimension. CPU/RAM/storage are only estimated (and therefore only debited/capped) when
	// BOTH (a) this platform experiment actually tracks that dimension (non-zero budget —
	// most platform experiments are GPU-only, and their agents' CPU/RAM/storage quota pools
	// are correctly 0/0, so debiting anything against them would always fail) and (b) the
	// agent explicitly set JobSpec.CPU/Memory/Storage — if left unset, the actual value used
	// is a per-cluster default resolved later by the execution engine (see
	// workload.JobDefaults), invisible to this control-plane layer, so it can't be billed or
	// capped here either. 0 correctly means "not tracked" for that submission.
	if exp.EstimatedCostT4H == 0 {
		exp.EstimatedCostT4H = exp.GPUType.Cost() * float64(exp.GPUCount) * exp.EstimatedDurationHours
	}
	if exp.EstimatedCPUCoreHours == 0 && pe.BudgetCPUCoreHours > 0 {
		if cores, err := workload.ParseCPUCores(exp.Job.CPU); err == nil && cores > 0 {
			exp.EstimatedCPUCoreHours = cores * float64(exp.Job.Nodes()) * exp.EstimatedDurationHours * domain.CPUCoreHourRate()
		}
	}
	if exp.EstimatedRAMGBHours == 0 && pe.BudgetRAMGBHours > 0 {
		if gb, err := workload.ParseMemoryGB(exp.Job.Memory); err == nil && gb > 0 {
			exp.EstimatedRAMGBHours = gb * float64(exp.Job.Nodes()) * exp.EstimatedDurationHours * domain.RAMGBHourRate()
		}
	}
	if exp.EstimatedStorageGBHours == 0 && pe.BudgetStorageGBHours > 0 {
		if gb, err := workload.ParseStorageGB(exp.Job.Storage); err == nil && gb > 0 {
			exp.EstimatedStorageGBHours = gb * float64(exp.Job.Nodes()) * exp.EstimatedDurationHours * domain.StorageGBHourRate()
		}
	}

	// 5. Novelty detection: compare against running and queued experiments.
	// This is advisory — low novelty is NOT a hard rejection. The score is stored on the
	// experiment so agents can see it and decide whether to refine. Agents should proactively
	// check GET /experiments?status=QUEUED to spot duplicates before submitting.
	activeExps, err := s.store.GetRunningAndQueued(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: get running and queued: %w", err)
	}
	noveltyScore, err := s.novelty.ComputeNovelty(ctx, exp.HypothesisID, activeExps)
	if err != nil {
		return fmt.Errorf("scheduler: compute novelty: %w", err)
	}
	exp.NoveltyScore = noveltyScore

	// 6. Priority scoring.
	priorityScore, err := s.computePriority(ctx, exp, noveltyScore)
	if err != nil {
		return fmt.Errorf("scheduler: compute priority: %w", err)
	}
	exp.PriorityScore = priorityScore

	// 7. Persist: create if new, or refresh priority if already QUEUED.
	// QUEUED re-submission only updates priority — no quota debit (the original debit
	// still stands). All other statuses were rejected above.
	if existing != nil {
		// Already QUEUED: refresh priority score only.
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, priorityScore); err != nil {
			return fmt.Errorf("scheduler: update priority: %w", err)
		}
		exp.Status = domain.StatusQueued
		if s.loop != nil {
			s.loop.Trigger()
		}
		return nil
	}

	// New experiment: debit quota (every resource dimension the submission uses) then create.
	if err := s.debitAllResources(ctx, exp); err != nil {
		return &AdmissionError{
			Reason:  ReasonInsufficientCredits,
			Message: err.Error(),
		}
	}
	now := time.Now().UTC()
	exp.CreatedAt = now
	exp.UpdatedAt = now
	exp.Status = domain.StatusQueued
	exp.PriorityScore = priorityScore
	if exp.ClusterName == "" {
		exp.ClusterName = s.pickClusterName()
	}
	if err := s.store.CreateExperiment(ctx, exp); err != nil {
		// Undo the quota debit so the agent can retry — the job never actually existed.
		s.refundAllResources(ctx, exp, 0, 0)
		return fmt.Errorf("scheduler: create experiment: %w", err)
	}
	exp.Status = domain.StatusQueued

	// 8. Wake the scheduler loop.
	if s.loop != nil {
		s.loop.Trigger()
	}
	return nil
}

// RePrioritize re-evaluates and persists priority scores for all QUEUED experiments.
// It is intended to be called periodically (e.g. every minute).
func (s *Service) RePrioritize(ctx context.Context) error {
	queued, err := s.store.ListExperiments(ctx, domain.ExperimentFilter{Status: domain.StatusQueued})
	if err != nil {
		return fmt.Errorf("scheduler: list queued for reprioritize: %w", err)
	}

	activeExps, err := s.store.GetRunningAndQueued(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: get active experiments: %w", err)
	}

	for _, exp := range queued {
		noveltyScore, err := s.novelty.ComputeNovelty(ctx, exp.HypothesisID, activeExps)
		if err != nil {
			return fmt.Errorf("scheduler: compute novelty for %s: %w", exp.ID, err)
		}
		score, err := s.computePriority(ctx, exp, noveltyScore)
		if err != nil {
			return fmt.Errorf("scheduler: compute priority for %s: %w", exp.ID, err)
		}
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, score); err != nil {
			return fmt.Errorf("scheduler: update priority for %s: %w", exp.ID, err)
		}
	}
	return nil
}

// resourceCosts returns (resourceType, estimatedAmount) pairs for every dimension exp actually
// uses — GPU is always included (even 0-cost, checked elsewhere); CPU/RAM/storage only appear
// when non-zero (the submission set them and they were estimated in Submit).
func resourceCosts(exp *domain.Experiment) []struct {
	resourceType domain.ResourceType
	amount       float64
} {
	return []struct {
		resourceType domain.ResourceType
		amount       float64
	}{
		{domain.ResourceGPUHours, exp.EstimatedCostT4H},
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours},
	}
}

// debitAllResources debits every non-zero resource dimension exp uses. If a later dimension's
// debit fails, the earlier ones already committed for this call are rolled back to 0 — the
// reservation is being abandoned entirely, so the job's own series should read "never happened",
// not "refunded" — so a rejected submission never leaves a partial debit behind.
func (s *Service) debitAllResources(ctx context.Context, exp *domain.Experiment) error {
	costs := resourceCosts(exp)
	for i, c := range costs {
		if c.amount <= 0 {
			continue
		}
		if err := s.quota.CheckAndDebitQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, c.resourceType, exp.CapacityTier, c.amount); err != nil {
			for _, prior := range costs[:i] {
				if prior.amount > 0 {
					_ = s.quota.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, prior.resourceType, exp.CapacityTier, 0)
				}
			}
			return err
		}
	}
	return nil
}

// refundAllResources overwrites each non-zero estimated dimension exp uses with its true
// observed cost, not an amount to refund. GPU-hours uses gpuCost directly — an absolute amount
// already computed per accelerator type actually used (see metricsdb.ObservedGPUCost), since a
// single flat rate over observedFraction would mischarge any job that ran on more than one
// accelerator type. CPU/RAM/storage have no per-type rate tiers, so they still use
// observedFraction × estimate. observedFraction=0 (and gpuCost=0) for a job that never ran
// (cancel-while-queued, a failed create).
func (s *Service) refundAllResources(ctx context.Context, exp *domain.Experiment, observedFraction, gpuCost float64) {
	for _, c := range resourceCosts(exp) {
		amount := observedFraction * c.amount
		if c.resourceType == domain.ResourceGPUHours {
			amount = gpuCost
		}
		if amount <= 0 && c.amount <= 0 {
			continue
		}
		_ = s.quota.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, c.resourceType, exp.CapacityTier, amount)
	}
}

// observedFraction returns exp's confirmed-alive time (see metricsdb.ObservedElapsedHours) as a
// fraction of its estimate, and its true GPU cost billed per accelerator type actually used (see
// metricsdb.ObservedGPUCost) — the two figures refundAllResources needs. This is the same
// GreptimeDB query Controller uses for every automatic eviction path — a user cancelling a job
// and the controller evicting it agree on what "how long did this run" and "what did it cost"
// mean, because both ask the same source of truth the same way. No fallback: GreptimeDB is a
// required dependency of this deployment, not an optional one, so a query error is returned to
// the caller rather than papered over.
func (s *Service) observedFraction(ctx context.Context, exp *domain.Experiment) (fraction, gpuCost float64, err error) {
	if exp.EstimatedDurationHours <= 0 {
		return 0, 0, nil
	}
	hours, err := metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, time.Now().UTC(), observedMaxLookback, s.observedGapCap, s.observedStep)
	if err != nil {
		return 0, 0, fmt.Errorf("scheduler: observed elapsed hours: %w", err)
	}
	gpuCost, err = metricsdb.ObservedGPUCost(ctx, s.metricsDBURL, exp.ID, exp.GPUCount, time.Now().UTC(), observedMaxLookback, s.observedGapCap, s.observedStep)
	if err != nil {
		return 0, 0, fmt.Errorf("scheduler: observed GPU cost: %w", err)
	}
	return hours / exp.EstimatedDurationHours, gpuCost, nil
}

// pickClusterName chooses the target cluster for a newly-admitted experiment.
// Policy: first configured cluster, in stable order — with exactly one cluster (the
// common case) this always returns that cluster's name ("default" for single-cluster
// deployments), so single-cluster behavior is unchanged.
func (s *Service) pickClusterName() string {
	if s.workload == nil {
		return "default"
	}
	names := s.workload.ClusterNames()
	if len(names) == 0 {
		return "default"
	}
	return names[0]
}

// computePriority calculates the weighted priority score for an experiment.
//
// Priority = w1*novelty + w3*costEfficiency
//
// Components:
//   - novelty:        provided by the caller (already computed against active experiments)
//   - costEfficiency: 1 / (1 + estimatedCost), favours cheaper experiments
//
// Note: SchedulingWeights has no W2/abuse-penalty field today — abuse is handled entirely by
// the controller's eviction guards (crash-loop, silence), not by suppressing admission
// priority. (This doc comment previously described a 3-term w1/w2/w3 formula that no longer
// matches the code — corrected to the actual 2-term one.)
func (s *Service) computePriority(_ context.Context, exp *domain.Experiment, novelty float64) (float64, error) {
	w := s.weights

	// Cost efficiency: cheap experiments get higher scores.
	cost := exp.EstimatedCostT4H
	if cost < 0 {
		cost = 0
	}
	costEfficiency := 1.0 / (1.0 + cost)

	score := w.W1Novelty*novelty +
		w.W3CostEfficiency*costEfficiency

	return score, nil
}

// CancelExperiment cancels a QUEUED or RUNNING/ADMITTED experiment.
//   - QUEUED: marks as REJECTED and issues a full credit refund.
//   - ADMITTED/RUNNING: marks as EVICTED (which is itself the deletion signal — the
//     cluster-agent reconciles its Jobs to the set of SUBMITTED/ADMITTED/RUNNING
//     experiments, so moving out of that set is what makes the Job go away) and issues
//     a partial refund for unused GPU-hours.
func (s *Service) CancelExperiment(ctx context.Context, id string) error {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return fmt.Errorf("cancel: get experiment: %w", err)
	}
	if exp == nil {
		return &AdmissionError{Reason: "not_found", Message: "experiment not found"}
	}

	switch exp.Status {
	case domain.StatusQueued, domain.StatusSubmitted:
		// Use a conditional update to guard against concurrent cancellation issuing a
		// double refund. If the row was already transitioned by a concurrent request,
		// treat it as a no-op rather than returning an error.
		updated, err := s.store.TransitionStatus(ctx, id, exp.Status, domain.StatusRejected)
		if err != nil {
			return fmt.Errorf("cancel: update status: %w", err)
		}
		if !updated {
			return nil // concurrent cancellation already handled it
		}
		_ = s.store.UpdateEvictionReason(ctx, id, string(domain.EvictionCancelled))
		// Never started running: 0 observed cost.
		s.refundAllResources(ctx, exp, 0, 0)
		return nil

	case domain.StatusAdmitted, domain.StatusRunning:
		if err := s.store.UpdateExperimentStatus(ctx, id, domain.StatusEvicted); err != nil {
			return fmt.Errorf("cancel: update status: %w", err)
		}
		_ = s.store.UpdateEvictionReason(ctx, id, string(domain.EvictionCancelled))
		fraction, gpuCost, err := s.observedFraction(ctx, exp)
		if err != nil {
			return fmt.Errorf("cancel: %w", err)
		}
		s.refundAllResources(ctx, exp, fraction, gpuCost)
		if s.loop != nil {
			s.loop.Trigger()
		}
		return nil

	default:
		return &AdmissionError{
			Reason:  "invalid_state",
			Message: fmt.Sprintf("cannot cancel experiment in status %s", exp.Status),
		}
	}
}

// WriteExperimentSummary files the agent's post-run write-up on a terminal experiment,
// attached to the hypothesis that job tested (not the job itself — see
// domain.HypothesisFinding) so the write-up joins the shared, accumulated evidence trail
// other agents see when deciding whether to test the same hypothesis again. Only allowed on
// COMPLETED, FAILED, EVICTED, or REJECTED experiments so that agents summarise what they
// learned — findings are visible to other agents via GET /registry/hypotheses/{id}.
func (s *Service) WriteExperimentSummary(ctx context.Context, id, summary string) error {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return fmt.Errorf("summary: get experiment: %w", err)
	}
	if exp == nil {
		return &AdmissionError{Reason: "not_found", Message: "experiment not found"}
	}
	switch exp.Status {
	case domain.StatusCompleted, domain.StatusFailed, domain.StatusEvicted, domain.StatusRejected:
		// ok
	default:
		return &AdmissionError{
			Reason:  "invalid_state",
			Message: fmt.Sprintf("summary can only be written on terminal experiments (got %s)", exp.Status),
		}
	}
	if _, err := s.store.CreateHypothesisFinding(ctx, exp.HypothesisID, id, exp.AgentID, summary); err != nil {
		return fmt.Errorf("summary: create finding: %w", err)
	}
	return nil
}
