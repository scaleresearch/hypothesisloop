package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var (
	// ErrNotResearcher is a merge, retirement or unlink asked by anyone but a researcher of the
	// hypothesis's platform experiment.
	ErrNotResearcher = errors.New("only a researcher of this platform experiment merges, retires and unlinks hypotheses: comment on the hypothesis to point one at it")
	// ErrHypothesisInFlight is a merge or retirement of a hypothesis a job is queued or running on.
	ErrHypothesisInFlight = errors.New("a job is queued or running on this hypothesis")
	// ErrReasonRequired is a retirement that says nothing of why.
	ErrReasonRequired = errors.New("reason is required: say why the idea is no longer worth a job")
)

// curated reads hypothesis id for a curation by agentID, refusing unless it exists and agentID is
// a researcher of its platform experiment.
func (s *Service) curated(ctx context.Context, id, agentID string) (*domain.Hypothesis, error) {
	h, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrHypothesisNotFound, id)
	}
	curates, err := s.Curates(ctx, h.PlatformExperimentID, agentID)
	if err != nil {
		return nil, err
	}
	if !curates {
		return nil, ErrNotResearcher
	}
	return h, nil
}

// idle refuses with ErrHypothesisInFlight while a job is queued or running on h. A running job is
// never stopped for a curation: its finding still lands on h. It is asked inside
// AroundHypothesis, so no job is submitted on h between it and the curation.
func (s *Service) idle(ctx context.Context, h *domain.Hypothesis) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{h.ID})
	if err != nil {
		return err
	}
	var live []string
	for _, job := range jobs {
		if !job.Status.Ended() {
			live = append(live, fmt.Sprintf("%s (%s)", job.ID, job.Status))
		}
	}
	if len(live) > 0 {
		return fmt.Errorf("%w: %s; curate it once they end", ErrHypothesisInFlight, named("job", live))
	}
	return nil
}

// MergeHypothesis records, for researcher agentID, that hypothesis id states the claim of into:
// the links to id move to into and id is tested under into from now on. Refused while a job is
// queued or running on id. Merging again into the same hypothesis changes nothing.
func (s *Service) MergeHypothesis(ctx context.Context, id, into, agentID string, actor domain.Actor, reason string) (*domain.Hypothesis, error) {
	current, err := s.curated(ctx, id, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry.MergeHypothesis: %w", err)
	}
	var h *domain.Hypothesis
	err = s.changes.AroundHypothesis(ctx, current.PlatformExperimentID, id, func(a domain.Announcer) error {
		if err := s.idle(ctx, current); err != nil {
			return err
		}
		var changed bool
		var err error
		if h, changed, err = s.graph.Merge(ctx, id, into, actor, strings.TrimSpace(reason)); err != nil {
			return err
		}
		return announce(ctx, a, changed, domain.Change{Kind: platformapi.EventHypothesisStatus, Subject: h.ID, Value: string(h.Status), Detail: h.MergedInto,
			PlatformExperimentID: h.PlatformExperimentID, AgentID: agentID})
	})
	if err != nil {
		return nil, fmt.Errorf("registry.MergeHypothesis: %w", err)
	}
	s.logger.Info("hypothesis merged", zap.String("id", id), zap.String("into", into), zap.String("agent", agentID))
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return nil, fmt.Errorf("registry.MergeHypothesis: %w", err)
	}
	return h, nil
}

// RetireHypothesis takes the open hypothesis id off the frontier for researcher agentID, with the
// reason it is no longer worth a job. Refused while a job is queued or running on it.
func (s *Service) RetireHypothesis(ctx context.Context, id, agentID string, actor domain.Actor, reason string) (*domain.Hypothesis, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("registry.RetireHypothesis: %w", ErrReasonRequired)
	}
	current, err := s.curated(ctx, id, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry.RetireHypothesis: %w", err)
	}
	var h *domain.Hypothesis
	err = s.changes.AroundHypothesis(ctx, current.PlatformExperimentID, id, func(a domain.Announcer) error {
		if err := s.idle(ctx, current); err != nil {
			return err
		}
		var changed bool
		var err error
		if h, changed, err = s.graph.Retire(ctx, id, actor, reason); err != nil {
			return err
		}
		return announce(ctx, a, changed, domain.Change{Kind: platformapi.EventHypothesisStatus, Subject: h.ID, Value: string(h.Status), Detail: h.StatusReason,
			PlatformExperimentID: h.PlatformExperimentID, AgentID: agentID})
	})
	if err != nil {
		return nil, fmt.Errorf("registry.RetireHypothesis: %w", err)
	}
	s.logger.Info("hypothesis retired", zap.String("id", id), zap.String("agent", agentID))
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return nil, fmt.Errorf("registry.RetireHypothesis: %w", err)
	}
	return h, nil
}

// UnlinkHypothesis removes one outgoing link of hypothesis id for researcher agentID and returns
// its outgoing links after. Removing an absent link changes nothing and announces nothing.
func (s *Service) UnlinkHypothesis(ctx context.Context, id, agentID string, spec domain.LinkSpec) ([]domain.Link, error) {
	current, err := s.curated(ctx, id, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry.UnlinkHypothesis: %w", err)
	}
	var out []domain.Link
	err = s.changes.Around(ctx, current.PlatformExperimentID, func(a domain.Announcer) error {
		removed, links, err := s.graph.Unlink(ctx, id, spec)
		if err != nil {
			return err
		}
		out = links
		if removed {
			a.Change(domain.Change{Kind: platformapi.EventHypothesisUnlink, Subject: id, Value: string(spec.Type), Detail: spec.To,
				PlatformExperimentID: current.PlatformExperimentID, AgentID: agentID})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry.UnlinkHypothesis: %w", err)
	}
	s.logger.Info("hypothesis link removed", zap.String("from", id), zap.String("type", string(spec.Type)), zap.String("to", spec.To), zap.String("agent", agentID))
	if err := s.store.NameActors(ctx, linkActors(out)); err != nil {
		return nil, fmt.Errorf("registry.UnlinkHypothesis: %w", err)
	}
	return out, nil
}
