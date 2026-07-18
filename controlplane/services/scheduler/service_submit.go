package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// occupiedFootprint returns every experiment currently holding physical capacity (SUBMITTED,
// ADMITTED, or RUNNING) — the same occupancy set tick() subtracts from live availability before
// admitting (see loop_tick.go step 2). Used by AdmitExperiment to check a requested cluster's
// real remaining room before an operator override claims it.
func (s *Service) occupiedFootprint(ctx context.Context) ([]*domain.Experiment, error) {
	submitted, err := s.store.ListSubmittedExperiments(ctx)
	if err != nil {
		return nil, err
	}
	admitted, err := s.store.ListAdmittedExperiments(ctx)
	if err != nil {
		return nil, err
	}
	running, err := s.store.ListRunningExperiments(ctx)
	if err != nil {
		return nil, err
	}
	occupied := make([]*domain.Experiment, 0, len(submitted)+len(admitted)+len(running))
	occupied = append(occupied, submitted...)
	occupied = append(occupied, admitted...)
	occupied = append(occupied, running...)
	return occupied, nil
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
	// ClusterName is intentionally left unset here: the admission loop assigns it, capacity-aware,
	// at the moment this job is actually admitted onto a specific cluster (see loop_tick.go).
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
