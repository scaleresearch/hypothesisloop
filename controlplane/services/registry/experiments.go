package registry

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Register assigns a new UUIDv7 ID and persists the experiment.
func (s *Service) Register(ctx context.Context, exp *domain.Experiment) error {
	if exp.PlatformExperimentID == "" {
		return fmt.Errorf("registry.Register: platform_experiment_id is required")
	}
	if exp.HypothesisID == "" {
		return fmt.Errorf("registry.Register: hypothesis_id is required")
	}
	hyp, err := s.store.GetHypothesis(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("registry.Register: get hypothesis: %w", err)
	}
	if hyp == nil {
		return fmt.Errorf("registry.Register: hypothesis %q not found", exp.HypothesisID)
	}
	if hyp.PlatformExperimentID != exp.PlatformExperimentID {
		return fmt.Errorf("registry.Register: hypothesis %q belongs to platform experiment %q, not %q — hypotheses cannot be tested outside the platform experiment they were registered under",
			exp.HypothesisID, hyp.PlatformExperimentID, exp.PlatformExperimentID)
	}

	id, err := newUUIDv7()
	if err != nil {
		return fmt.Errorf("registry.Register: generate id: %w", err)
	}
	exp.ID = id
	now := time.Now().UTC()
	exp.CreatedAt = now
	exp.UpdatedAt = now
	if exp.Status == "" {
		exp.Status = domain.StatusQueued
	}
	if err := s.store.CreateExperiment(ctx, exp); err != nil {
		return fmt.Errorf("registry.Register: store: %w", err)
	}
	s.logger.Info("experiment registered", zap.String("id", exp.ID), zap.String("agent", exp.AgentID))
	return nil
}

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
