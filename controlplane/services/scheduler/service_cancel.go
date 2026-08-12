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
	return s.settle(ctx, "cancel", exp)
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
	updated, err := s.store.TransitionTerminal(ctx, id, exp.Status, domain.StatusEvicted, string(reason))
	if err != nil {
		return fmt.Errorf("evict: update status: %w", err)
	}
	if !updated {
		return nil
	}
	exp.Status = domain.StatusEvicted
	exp.EvictionReason = string(reason)
	return s.settle(ctx, "evict", exp)
}

// settle runs the one canonical terminal-accounting step for an experiment this service just
// moved to a terminal status. There is deliberately no per-caller accounting implementation.
func (s *Service) settle(ctx context.Context, op string, exp *domain.Experiment) error {
	if s.settler == nil {
		return fmt.Errorf("%s: quota settler is required", op)
	}
	if err := s.settler.Settle(ctx, exp); err != nil {
		return fmt.Errorf("%s: settle observed usage: %w", op, err)
	}
	if err := s.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		return fmt.Errorf("%s: mark quota settled: %w", op, err)
	}
	return nil
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
