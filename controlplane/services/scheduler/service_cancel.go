package scheduler

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
