package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// List returns the experiments matching the filter, each with its metrics: a
// hypothesis's jobs. GET /experiments is the scheduler service's.
func (s *Service) List(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	exps, err := s.store.ListExperiments(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("registry.List: %w", err)
	}
	if err := metricsdb.PopulateJobMetrics(ctx, s.metricsDBURL, s.store.GetPlatformExperimentMetrics, exps); err != nil {
		return nil, fmt.Errorf("registry.List: %w", err)
	}
	return exps, nil
}

// Count returns how many experiments match the filter, ignoring its Limit/Offset — the full
// size of a set List returned one page of.
func (s *Service) Count(ctx context.Context, filter domain.ExperimentFilter) (int, error) {
	n, err := s.store.CountExperiments(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("registry.Count: %w", err)
	}
	return n, nil
}

// GetLineage returns the ancestor chain of an experiment (oldest first).
func (s *Service) GetLineage(ctx context.Context, id string) ([]*domain.Experiment, error) {
	lineage, err := s.store.GetLineage(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetLineage: %w", err)
	}
	// The walk starts at the experiment itself, so an empty chain means there is no such row —
	// a caller's typo, not a server fault.
	if len(lineage) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrExperimentNotFound, id)
	}
	return lineage, nil
}
