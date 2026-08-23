package registry

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrHypothesisNotFound and ErrHypothesisNotOwner are the two ways SetHypothesisStatus can fail
// on ownership, distinguished so the HTTP layer can return 404 vs 403 rather than one status for
// both.
var (
	ErrHypothesisNotFound = fmt.Errorf("hypothesis not found")
	ErrHypothesisNotOwner = db.ErrNotOwner
)

// RegisterHypothesis registers a new hypothesis within a platform experiment, or returns the
// existing one (with alreadyExisted=true) if an equivalent one (by normalized text) was
// already registered *in that same platform experiment*. Agents should call this — and
// retrieve the returned ID — before submitting an experiment, since ExperimentMeta.HypothesisID
// is required and must belong to the same platform experiment as the job being submitted.
//
// A human submitting from the UI passes author instead of agentID. This is the one place the
// exactly-one-of rule is applied (domain.ClassifyHypothesisOrigin): an invalid pair is an error
// here, never a value coerced into a row for some later read to interpret.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID, author, platformExperimentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error) {
	source, err := domain.ClassifyHypothesisOrigin(agentID, author)
	if err != nil {
		return nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	h, alreadyExisted, err = s.store.FindOrCreateHypothesis(ctx, source, agentID, author, platformExperimentID, text)
	if err != nil {
		return nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID), zap.String("author", author),
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

// ListHypotheses returns hypotheses registered within a platform experiment, most recent
// first, each carrying finding/comment counts — the shared idea pool agents draw from and add
// to. agentID restricts to one agent's own hypotheses when non-empty; status restricts to one
// status (open/confirmed/inconclusive) when non-empty; limit is bounded and defaulted by the
// store — see db.HypothesesStore.ListHypotheses.
func (s *Service) ListHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus, limit, offset int) ([]*db.HypothesisListItem, error) {
	if status != "" && !domain.ValidHypothesisStatus(status) {
		return nil, fmt.Errorf("registry.ListHypotheses: invalid status %q", status)
	}
	hs, err := s.store.ListHypotheses(ctx, platformExperimentID, agentID, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	return hs, nil
}

// CountHypotheses returns the total matching the same filters, ignoring limit/offset.
func (s *Service) CountHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus) (int, error) {
	total, err := s.store.CountHypotheses(ctx, platformExperimentID, agentID, status)
	if err != nil {
		return 0, fmt.Errorf("registry.CountHypotheses: %w", err)
	}
	return total, nil
}

// SetHypothesisStatus sets a hypothesis's status on behalf of callerAgentID — the owning agent's
// own verdict on its own claim, or any agent's verdict on an unowned human row (see
// domain.HypothesisStatus and db.HypothesesStore.UpdateHypothesisStatus). Ownership is enforced by the store
// in the same statement as the write; this just translates its two failure modes into errors the
// HTTP layer can map to 404/403 (see ErrHypothesisNotFound/ErrHypothesisNotOwner).
func (s *Service) SetHypothesisStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus) (*domain.Hypothesis, error) {
	if !domain.ValidHypothesisStatus(status) {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: invalid status %q", status)
	}
	h, err := s.store.UpdateHypothesisStatus(ctx, id, callerAgentID, status)
	if err != nil {
		if errors.Is(err, db.ErrNotOwner) {
			return nil, ErrHypothesisNotOwner
		}
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	if h == nil {
		return nil, ErrHypothesisNotFound
	}
	s.logger.Info("hypothesis status updated",
		zap.String("id", id), zap.String("agent", callerAgentID), zap.String("status", string(status)))
	return h, nil
}

// ListHypothesisFindings returns one page of the findings filed against a hypothesis, oldest
// first, alongside the full count the page was taken from.
func (s *Service) ListHypothesisFindings(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, int, error) {
	fs, err := s.store.ListFindingsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	total, err := s.store.CountFindingsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	return fs, total, nil
}

// AddHypothesisComment records a freeform, job-independent note against a hypothesis — see
// domain.HypothesisComment for how this differs from a finding.
// A human commenting from the UI passes author instead of agentID, under the same exactly-one-of
// rule registration applies.
func (s *Service) AddHypothesisComment(ctx context.Context, hypothesisID, agentID, author, text string) (*domain.HypothesisComment, error) {
	source, err := domain.ClassifyHypothesisOrigin(agentID, author)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	c, err := s.store.CreateHypothesisComment(ctx, hypothesisID, source, agentID, author, text)
	if err != nil {
		if errors.Is(err, db.ErrUnknownAgent) {
			return nil, fmt.Errorf("registry.AddHypothesisComment: %w", db.ErrUnknownAgent)
		}
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	s.logger.Info("hypothesis comment added",
		zap.String("hypothesis_id", hypothesisID), zap.String("agent", agentID), zap.String("author", author))
	return c, nil
}

// ListHypothesisComments returns one page of the comments filed against a hypothesis, oldest
// first, alongside the full count the page was taken from.
func (s *Service) ListHypothesisComments(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, int, error) {
	cs, err := s.store.ListCommentsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	total, err := s.store.CountCommentsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	return cs, total, nil
}
