package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// List returns experiments matching the filter — used internally (e.g. a hypothesis's jobs), not
// exposed as its own list-experiments endpoint (that's GET /experiments on the scheduler service).
func (s *Service) List(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	exps, err := s.store.ListExperiments(ctx, filter)
	if err != nil {
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
	return lineage, nil
}
