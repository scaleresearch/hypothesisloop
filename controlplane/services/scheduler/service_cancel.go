package scheduler

import (
	"context"
	"fmt"

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
		updated, err := s.store.TransitionTerminal(ctx, id, exp.Status, domain.StatusRejected, string(domain.EvictionCancelled))
		if err != nil {
			return fmt.Errorf("cancel: update status: %w", err)
		}
		if !updated {
			return nil // concurrent cancellation already handled it
		}
		exp.Status = domain.StatusRejected

	case domain.StatusAdmitted, domain.StatusRunning:
		updated, err := s.store.TransitionTerminal(ctx, id, exp.Status, domain.StatusEvicted, string(domain.EvictionCancelled))
		if err != nil {
			return fmt.Errorf("cancel: update status: %w", err)
		}
		if !updated {
			return nil
		}
		exp.Status = domain.StatusEvicted
		if s.loop != nil {
			s.loop.Trigger()
		}

	default:
		return &AdmissionError{
			Reason:  "invalid_state",
			Message: fmt.Sprintf("cannot cancel experiment in status %s", exp.Status),
		}
	}
	if s.settler == nil {
		return fmt.Errorf("cancel: quota settler is required")
	}
	if err := s.settler.Settle(ctx, exp); err != nil {
		return fmt.Errorf("cancel: settle observed usage: %w", err)
	}
	if err := s.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		return fmt.Errorf("cancel: mark quota settled: %w", err)
	}
	return nil
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
