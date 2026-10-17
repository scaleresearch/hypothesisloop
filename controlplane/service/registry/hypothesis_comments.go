package registry

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AddHypothesisComment records a freeform, job-independent note against a hypothesis, or against
// the objective when hypothesisID is a platform experiment's id — see domain.HypothesisComment for
// how this differs from a finding. agentID is the agent whose note it is, empty for one no agent
// owns, and actor the credential writing it.
func (s *Service) AddHypothesisComment(ctx context.Context, hypothesisID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	platformExperimentID, err := s.CommentTarget(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	c, err := s.store.CreateHypothesisComment(ctx, hypothesisID, platformExperimentID, agentID, actor, text)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	if err := s.store.NameActors(ctx, []*domain.Actor{&c.Actor}); err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	s.logger.Info("hypothesis comment added",
		zap.String("hypothesis_id", hypothesisID), zap.String("agent", agentID),
		zap.String("actor_kind", string(actor.Kind)), zap.String("actor_id", actor.ID))
	return c, nil
}

// CommentTarget is the platform experiment a comment on id lands in: the hypothesis's, or id
// itself when it names a platform experiment, whose objective is the root of its graph and holds
// comments like any hypothesis, under its id.
func (s *Service) CommentTarget(ctx context.Context, id string) (string, error) {
	h, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return "", err
	}
	if h != nil {
		return h.PlatformExperimentID, nil
	}
	found, err := s.store.PlatformExperimentExists(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrHypothesisNotFound, id)
	}
	return id, nil
}

// ListHypothesisComments returns one page of the comments filed against a hypothesis, or against
// the objective under its platform experiment's id, oldest first, alongside the full count the
// page was taken from.
func (s *Service) ListHypothesisComments(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, int, error) {
	cs, err := s.store.ListCommentsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	actors := make([]*domain.Actor, len(cs))
	for i, c := range cs {
		actors[i] = &c.Actor
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	total, err := s.store.CountCommentsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	return cs, total, nil
}
