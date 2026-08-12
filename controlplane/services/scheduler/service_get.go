package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetExperiment returns one experiment with the runtime's latest phase detail (why a non-running
// job's container hasn't started) merged in live from the metrics store — never persisted to
// PostgreSQL, so a stale explanation can never outlive what actually happened. Returns nil when
// no such experiment exists.
func (s *Service) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if exp == nil || s.metricsDBURL == "" {
		return exp, nil
	}
	reason, message, restartCount, found, err := metricsdb.GetLatestPhaseDetail(ctx, s.metricsDBURL, id)
	if err != nil {
		// Phase detail is diagnostic, not authoritative: a metrics-store hiccup must not turn a
		// perfectly good experiment read into a 500.
		if s.logger != nil {
			s.logger.Warn("scheduler.GetExperiment: phase detail unavailable", zap.String("id", id), zap.Error(err))
		}
	} else if found {
		exp.PhaseDetail = &domain.PhaseDetail{Reason: reason, Message: message, RestartCount: restartCount}
	}
	return exp, nil
}
