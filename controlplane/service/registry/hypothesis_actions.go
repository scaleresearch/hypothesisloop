package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Reader is who the hypotheses of one platform experiment are answered to, with what bears on
// what they may do to them: whether they write the platform experiment and own its org, and
// whether it closed, after which its graph takes nothing from anyone.
type Reader struct {
	Caller   auth.Principal
	Writes   bool
	OrgOwner bool
	Closed   bool
}

// PlatformExperimentClosed says whether the platform experiment id is closed.
func (s *Service) PlatformExperimentClosed(ctx context.Context, id string) (bool, error) {
	closed, err := s.store.PlatformExperimentClosed(ctx, id)
	if err != nil {
		return false, fmt.Errorf("registry.PlatformExperimentClosed: %w", err)
	}
	return closed, nil
}

// HypothesisActions is what r may do to h now, by the rules its edit, its removal, a link from it
// and a comment on it are refused by. Only an idea r may otherwise change is read for whether it
// is in the record.
func (s *Service) HypothesisActions(ctx context.Context, h *domain.Hypothesis, r Reader) ([]domain.HypothesisAction, error) {
	actions := []domain.HypothesisAction{}
	if !r.Writes || r.Closed {
		return actions, nil
	}
	changer := Changer{UserID: r.Caller.UserID, OrgOwner: r.OrgOwner}
	edits, removes := ChangeRefusal(h, changer, false) == nil, ChangeRefusal(h, changer, true) == nil
	if edits || removes {
		jobs, linked, err := s.record(ctx, h)
		if err != nil {
			return nil, fmt.Errorf("registry.HypothesisActions: %w", err)
		}
		if len(jobs) == 0 && len(linked) == 0 {
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
	// A link from an agent's hypothesis is that agent's alone (graphdb.Store.AddLink).
	if h.AgentID == "" || h.AgentID == r.Caller.AgentID {
		actions = append(actions, domain.HypothesisActionLink)
	}
	return append(actions, domain.HypothesisActionComment), nil
}
