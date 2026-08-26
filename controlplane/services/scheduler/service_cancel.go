package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// CancelExperiment cancels a QUEUED or RUNNING/ADMITTED experiment.
//   - QUEUED/SUBMITTED: marks as REJECTED.
//   - ADMITTED/RUNNING: marks as EVICTED (which is itself the deletion signal — the
//     cluster-agent reconciles its Jobs to the set of SUBMITTED/ADMITTED/RUNNING
//     experiments, so moving out of that set is what makes the Job go away).
//
// Every path then uses the canonical settlement service; there is no cancellation-specific
// accounting implementation.

// cancelMaxAttempts bounds the re-read-and-retry loop below. A lost CAS almost always means
// admission progressed the status by exactly one step (QUEUED->SUBMITTED->ADMITTED->RUNNING);
// a handful of attempts comfortably outruns that without risking a livelock against a truly
// concurrent cancel (which converges immediately: the second cancel's CAS is a no-op because
// the first one already moved the row to terminal).
const cancelMaxAttempts = 5

func (s *Service) CancelExperiment(ctx context.Context, id string) error {
	for attempt := 0; attempt < cancelMaxAttempts; attempt++ {
		exp, err := s.store.GetExperiment(ctx, id)
		if err != nil {
			return fmt.Errorf("cancel: get experiment: %w", err)
		}
		if exp == nil {
			return &AdmissionError{Reason: "not_found", Message: "experiment not found"}
		}

		switch exp.Status {
		case domain.StatusQueued, domain.StatusSubmitted:
			outcome, err := s.store.ResolveTermination(ctx, id, exp.Status, domain.StatusRejected, string(domain.EvictionCancelled), "")
			if err != nil {
				return fmt.Errorf("cancel: update status: %w", err)
			}
			if outcome != domain.TerminationWritten {
				// Lost the CAS: either a concurrent cancel already terminalized it, or admission
				// moved it forward (e.g. QUEUED->SUBMITTED) between our read and this write.
				// Re-read and retry rather than assuming the former — otherwise a cancel racing
				// admission can report success while the job goes on to run to completion.
				continue
			}
			exp.Status = domain.StatusRejected
			s.settle(ctx, "cancel", exp)
			return nil

		case domain.StatusAdmitted, domain.StatusRunning:
			outcome, err := s.store.ResolveTermination(ctx, id, exp.Status, domain.StatusEvicted, string(domain.EvictionCancelled), "")
			if err != nil {
				return fmt.Errorf("cancel: update status: %w", err)
			}
			if outcome != domain.TerminationWritten {
				continue
			}
			exp.Status = domain.StatusEvicted
			s.loop.Trigger()
			s.settle(ctx, "cancel", exp)
			return nil

		default:
			return &AdmissionError{
				Reason:  "invalid_state",
				Message: fmt.Sprintf("cannot cancel experiment in status %s", exp.Status),
			}
		}
	}
	return fmt.Errorf("cancel: experiment %s status kept changing underneath the cancel; give up after %d attempts", id, cancelMaxAttempts)
}

// EvictExperiment terminates a RUNNING or ADMITTED experiment through the same canonical path
// CancelExperiment uses — EVICTED is itself the deletion signal, and settlement writes the real
// observed usage. Implements ExperimentEvictor for the scheduler loop's disbalance evictor.
// A job that has already left RUNNING/ADMITTED (finished or was cancelled concurrently) is a
// no-op, not an error: the outcome the caller wanted has happened either way.
func (s *Service) EvictExperiment(ctx context.Context, id string, reason domain.EvictionReason) error {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return fmt.Errorf("evict: get experiment: %w", err)
	}
	if exp == nil {
		return &AdmissionError{Reason: "not_found", Message: "experiment not found"}
	}
	switch exp.Status {
	case domain.StatusAdmitted, domain.StatusRunning:
	default:
		return nil
	}
	outcome, err := s.store.ResolveTermination(ctx, id, exp.Status, domain.StatusEvicted, string(reason), "")
	if err != nil {
		return fmt.Errorf("evict: update status: %w", err)
	}
	if outcome != domain.TerminationWritten {
		return nil
	}
	exp.Status = domain.StatusEvicted
	exp.EvictionReason = string(reason)
	s.settle(ctx, "evict", exp)
	return nil
}

// settle runs the one canonical terminal-accounting step for an experiment this service just
// moved to a terminal status. There is deliberately no per-caller accounting implementation.
//
// Best-effort: the terminal transition above already committed, so a settlement failure here
// must not be reported as if the cancel/evict itself failed — callers like the disbalance
// evictor read a returned error as "the transition never happened" and retry against a fresh
// victim, condemning extra jobs for the same shortage while this one sits evicted-but-unsettled.
// The settlement reconciler (see settlement.go) sweeps and retries unsettled terminal
// experiments, so logging and moving on here is safe.
func (s *Service) settle(ctx context.Context, op string, exp *domain.Experiment) {
	if err := s.settler.Settle(ctx, exp); err != nil {
		s.logger.Warn("scheduler: settle observed usage", zap.String("op", op), zap.String("exp", exp.ID), zap.Error(err))
		return
	}
	if err := s.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		s.logger.Error("scheduler: mark quota settled", zap.String("op", op), zap.String("exp", exp.ID), zap.Error(err))
	}
}

// WriteExperimentSummary files the agent's post-run write-up on a terminal experiment,
// attached to the hypothesis that job tested (not the job itself — see
// domain.HypothesisFinding) so the write-up joins the shared, accumulated evidence trail
// other agents see when deciding whether to test the same hypothesis again. Only allowed on
// COMPLETED, FAILED, EVICTED, or REJECTED experiments so that agents summarise what they
// learned — findings are visible to other agents via GET /hypotheses/{id}.
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
