package scheduler

import (
	"context"
	"errors"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// ReasonSummaryExists is the reason of the 409 a second summary on one job is answered with.
const ReasonSummaryExists = "summary_exists"

// ChangeRefusal is the refusal a cancel or a summary answers an AdmissionError with, nil for
// any other error.
func ChangeRefusal(err error) *refusal.Refusal {
	var admErr *AdmissionError
	if !errors.As(err, &admErr) {
		return nil
	}
	status := http.StatusUnprocessableEntity
	switch admErr.Reason {
	case "not_found":
		status = http.StatusNotFound
	case ReasonInvalidTransition, ReasonSummaryExists:
		status = http.StatusConflict
	}
	return refusal.Refuse(status, admErr.Reason, admErr.Message)
}

// AdmissionStatus is the HTTP status an admission refusal of reason is answered with.
func AdmissionStatus(reason string) int {
	switch reason {
	case ReasonInsufficientCredits:
		return http.StatusPaymentRequired
	case ReasonDuplicate:
		return http.StatusConflict
	case ReasonMalformed:
		return http.StatusBadRequest
	case ReasonRateLimited:
		return http.StatusTooManyRequests
	case ReasonImageUnresolved:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}

// List is a page of the jobs filter matches, each with what is read beside its row, and how many
// match in all.
func (s *Service) List(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, int, error) {
	exps, err := s.store.ListExperiments(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(s.logger, "list experiments", err)
	}
	if err := s.NameJobs(ctx, exps...); err != nil {
		return nil, 0, auth.Internal(s.logger, "list experiments", err)
	}
	total, err := s.store.CountExperiments(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(s.logger, "list experiments", err)
	}
	for _, merge := range []func(context.Context, []*domain.Experiment) error{s.mergePhaseDetails, s.mergeRanOn, s.mergeBuildProgress, s.mergeMetrics} {
		if err := merge(ctx, exps); err != nil {
			return nil, 0, auth.Internal(s.logger, "list experiments", err)
		}
	}
	return exps, total, nil
}

// Stats counts the jobs filter matches, grouped by groupBy when it names a group.
func (s *Service) Stats(ctx context.Context, filter domain.ExperimentFilter, groupBy string) (*domain.ExperimentStats, error) {
	stats, err := s.store.ExperimentStats(ctx, filter, groupBy)
	if err != nil {
		return nil, auth.Internal(s.logger, "experiment stats", err)
	}
	return stats, nil
}
