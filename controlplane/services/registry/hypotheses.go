package registry

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// RegisterHypothesis registers a new hypothesis within a platform experiment, or returns the
// existing one (with alreadyExisted=true) if an equivalent one (by normalized text) was
// already registered *in that same platform experiment*. Agents should call this — and
// retrieve the returned ID — before submitting an experiment, since ExperimentMeta.HypothesisID
// is required and must belong to the same platform experiment as the job being submitted.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID, platformExperimentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error) {
	h, alreadyExisted, err = s.store.FindOrCreateHypothesis(ctx, agentID, platformExperimentID, text)
	if err != nil {
		return nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID),
		zap.String("platform_experiment_id", platformExperimentID), zap.Bool("already_existed", alreadyExisted))
	return h, alreadyExisted, nil
}

// GetHypothesis returns a single hypothesis by ID.
func (s *Service) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	h, err := s.store.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetHypothesis: %w", err)
	}
	return h, nil
}

// ListHypotheses returns every hypothesis registered within a platform experiment, most
// recent first — the shared idea pool agents draw from and add to for that platform experiment.
func (s *Service) ListHypotheses(ctx context.Context, platformExperimentID string) ([]*domain.Hypothesis, error) {
	hs, err := s.store.ListHypotheses(ctx, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	return hs, nil
}

// ListHypothesisFindings returns every finding filed against a hypothesis, oldest first.
func (s *Service) ListHypothesisFindings(ctx context.Context, hypothesisID string) ([]*domain.HypothesisFinding, error) {
	fs, err := s.store.ListFindingsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	return fs, nil
}
