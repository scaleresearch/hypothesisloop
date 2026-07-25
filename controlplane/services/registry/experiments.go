package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Get returns a single experiment by ID.
func (s *Service) Get(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.Get: %w", err)
	}
	return exp, nil
}

// List returns experiments matching the filter.
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
