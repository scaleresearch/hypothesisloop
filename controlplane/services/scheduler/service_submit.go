package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

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

	cut, err := s.store.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: check stage cut: %w", err)
	}
	if cut {
		return &AdmissionError{
			Reason:  "agent_held",
			Message: "agent was cut at a stage boundary and cannot submit new jobs for the rest of this platform experiment — check GET /platform-experiments/{id}/stages",
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
	// The hypothesis must belong to the same platform experiment this job is submitted under.
	// Without this check an agent could point a job at a hypothesis registered under a different
	// program, contaminating cross-program scope/ranking (invariant #7).
	if hyp.PlatformExperimentID != exp.PlatformExperimentID {
		return &AdmissionError{
			Reason:  ReasonMalformed,
			Message: fmt.Sprintf("hypothesis %s belongs to platform experiment %s, not %s", exp.HypothesisID, hyp.PlatformExperimentID, exp.PlatformExperimentID),
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

	// 4. Compute estimated cost if not already set. Accelerator-hours is the primary/always-populated
	// dimension; CPU-hours is only estimated (and therefore only debited/capped) when this
	// platform experiment actually tracks that dimension (non-zero budget — most platform
	// experiments are accelerator-only, and their agents' CPU quota pool is correctly 0/0, so debiting
	// anything against it would always fail). 0 correctly means "not tracked" for that
	// submission. CPU/Memory/Storage are always set on JobSpec now (see ValidateExperiment's
	// "explicit resource requests" cross-cutting fix), so there is no more "left unset,
	// resolved later by a cluster-side default" case to work around here.
	//
	// RAM/storage are hard physical-fit-checked
	// at admission (see domain.Experiment.Footprint()/domain.Fits, wired into loop_tick.go) but
	// deliberately never estimated/debited as hours here — EstimatedRAMGBHours/
	// EstimatedStorageGBHours are left at 0 for every new submission from this point on. This
	// is an intentional migration decision, not an oversight: PlatformExperiment.BudgetRAMGBHours/
	// BudgetStorageGBHours, AgentQuota's RAM/storage guaranteed/burst columns, and
	// domain.ResourceRAMGBHours/ResourceStorageGBHours all remain defined (deprecated, not
	// deleted — see their own doc comments) so existing rows/history are never silently
	// dropped or rewritten. A platform experiment created before this change with a non-zero
	// RAM/storage budget keeps that number in the DB, but it is no longer read by anything: no
	// new debit ever happens against it, so its guaranteed/burst pools simply stop moving.
	// Historical ActualRAMGBHours/ActualStorageGBHours on already-terminal experiments are
	// untouched (this is a forward-only behavior change, not a backfill/rewrite).
	if exp.EstimatedCostAccH == 0 && exp.AcceleratorCount > 0 {
		exp.EstimatedCostAccH = estimatedAcceleratorCost(exp)
	}
	if exp.EstimatedCPUCoreHours == 0 && pe.BudgetCPUCoreHours > 0 {
		if cores, err := workload.ParseCPUCores(exp.Job.CPU); err == nil && cores > 0 {
			exp.EstimatedCPUCoreHours = cores * float64(exp.Job.Nodes()) * exp.EstimatedDurationHours * domain.CPUCoreHourRate()
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

	// Atomically validate aggregate desired estimates plus observed settled usage and insert the
	// PostgreSQL desired-state row under the per-agent admission lock.
	now := time.Now().UTC()
	exp.CreatedAt = now
	exp.UpdatedAt = now
	exp.Status = domain.StatusQueued
	exp.PriorityScore = priorityScore
	// ClusterName is intentionally left unset here: the admission loop assigns it, capacity-aware,
	// at the moment this job is actually admitted onto a specific cluster (see loop_tick.go).
	if err := s.quota.AdmitExperiment(ctx, exp); err != nil {
		var insufficient interface{ InsufficientQuota() bool }
		if !errors.As(err, &insufficient) {
			return fmt.Errorf("scheduler: atomically admit experiment: %w", err)
		}
		return &AdmissionError{
			Reason:  ReasonInsufficientCredits,
			Message: err.Error(),
		}
	}

	// 8. Wake the scheduler loop.
	if s.loop != nil {
		s.loop.Trigger()
	}
	return nil
}

func estimatedAcceleratorCost(exp *domain.Experiment) float64 {
	if exp.AcceleratorCount == 0 {
		return 0
	}
	return exp.AcceleratorType.Cost() * float64(exp.AcceleratorCount) * exp.EstimatedDurationHours
}
