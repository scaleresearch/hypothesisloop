package registry

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Get returns a single experiment by ID, with its latest runtime-reported phase detail (why a
// non-running job's container hasn't started) merged in live from the metrics store — never
// persisted to PostgreSQL, so a stale explanation can never outlive what actually happened.
func (s *Service) Get(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.Get: %w", err)
	}
	if exp == nil {
		return nil, nil
	}
	reason, message, restartCount, found, err := metricsdb.GetLatestPhaseDetail(ctx, s.metricsDBURL, id)
	if err != nil {
		// Phase detail is diagnostic, not authoritative: a metrics-store hiccup must not turn a
		// perfectly good experiment read into a 500.
		s.logger.Warn("registry.Get: phase detail unavailable", zap.String("id", id), zap.Error(err))
	} else if found {
		exp.PhaseDetail = &domain.PhaseDetail{Reason: reason, Message: message, RestartCount: restartCount}
	}
	return exp, nil
}

// List returns experiments matching the filter — used internally (e.g. a hypothesis's jobs), not
// exposed as its own list-experiments endpoint (that's GET /experiments on the scheduler service).
func (s *Service) List(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	exps, err := s.store.ListExperiments(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("registry.List: %w", err)
	}
	return exps, nil
}

// GetLineage returns the ancestor chain of an experiment (oldest first).
func (s *Service) GetLineage(ctx context.Context, id string) ([]*domain.Experiment, error) {
	lineage, err := s.store.GetLineage(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetLineage: %w", err)
	}
	return lineage, nil
}
