package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Reader is who the hypotheses of one platform experiment are answered to, with what bears on
// what they may do to them: whether they write the platform experiment and own its org, whether
// they are a researcher of it, and whether it closed, after which its graph takes nothing from
// anyone.
type Reader struct {
	Caller   auth.Principal
	Writes   bool
	OrgOwner bool
	Curates  bool
	Closed   bool
}

// Curates says whether agentID is a researcher of platform experiment pe, who may link, merge,
// retire and unlink any of its hypotheses.
func (s *Service) Curates(ctx context.Context, pe, agentID string) (bool, error) {
	researcher, err := s.isResearcher(ctx, pe, agentID)
	if err != nil {
		return false, fmt.Errorf("registry.Curates: %w", err)
	}
	return researcher, nil
}

// PlatformExperimentClosed says whether the platform experiment id is closed.
func (s *Service) PlatformExperimentClosed(ctx context.Context, id string) (bool, error) {
	closed, err := s.store.PlatformExperimentClosed(ctx, id)
	if err != nil {
		return false, fmt.Errorf("registry.PlatformExperimentClosed: %w", err)
	}
	return closed, nil
}

// HypothesisActions is what r may do to h now, by the rules its edit, its removal, a link from it,
// its curation and a comment on it are refused by. Only an idea r may otherwise change is read for
// whether it is in the record. Whether a job is in flight on it is left to the merge or
// retirement itself.
func (s *Service) HypothesisActions(ctx context.Context, h *domain.Hypothesis, r Reader) ([]domain.HypothesisAction, error) {
	actions := []domain.HypothesisAction{}
	if !r.Writes || r.Closed {
		return actions, nil
	}
	changer := Changer{UserID: r.Caller.UserID, OrgOwner: r.OrgOwner}
	edits, removes := ChangeRefusal(h, changer, false) == nil, ChangeRefusal(h, changer, true) == nil
	if edits || removes {
		var inRecord *InRecordError
		switch err := s.untouched(ctx, h, ""); {
		case errors.As(err, &inRecord):
		case err != nil:
			return nil, fmt.Errorf("registry.HypothesisActions: %w", err)
		default:
			if edits {
				actions = append(actions, domain.HypothesisActionEdit)
			}
			if removes {
				actions = append(actions, domain.HypothesisActionRemove)
			}
		}
	}
	if r.Caller.Actor().Kind == "" || !auth.AudienceAgent.Admits(r.Caller, "") {
		return actions, nil
	}
	if h.Curated() {
		return append(actions, domain.HypothesisActionComment), nil
	}
	// A link from an agent's hypothesis is that agent's, or a researcher's (AddHypothesisLink).
	if h.AgentID == "" || h.AgentID == r.Caller.AgentID || r.Curates {
		actions = append(actions, domain.HypothesisActionLink)
	}
	actions = append(actions, domain.HypothesisActionComment)
	if !r.Curates {
		return actions, nil
	}
	if h.Status == domain.HypothesisOpen {
		actions = append(actions, domain.HypothesisActionMerge, domain.HypothesisActionRetire)
	}
	return append(actions, domain.HypothesisActionUnlink), nil
}
