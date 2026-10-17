package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// Checkout is what a checkout of a hypothesis answers: the branch recorded and who else works on
// the hypothesis now.
type Checkout struct {
	HypothesisID string                    `json:"hypothesis_id"`
	AgentID      string                    `json:"agent_id"`
	Branch       string                    `json:"branch"`
	Working      []domain.HypothesisWorker `json:"working" doc:"The other agents working on it now: a job of theirs queued or running, or a checkout within two hours."`
	Warning      string                    `json:"warning,omitempty" doc:"Set when working is not empty: who already has it and what to weigh."`
}

// CheckOut records that agentID, an agent of the hypothesis's platform experiment, checked hyp
// out on branch, so the agents and the coordinator see who works on it before a job does. Another
// agent on it is said in a warning: two approaches to one idea can both be worth running.
func (s *Service) CheckOut(ctx context.Context, hyp *domain.Hypothesis, agentID, branch string) (*Checkout, error) {
	signup, err := s.store.GetAgentSignup(ctx, hyp.PlatformExperimentID, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry.CheckOut: %w", err)
	}
	if signup == nil {
		return nil, domain.ErrNotSignedUp
	}
	others, err := s.store.CheckOutHypothesis(ctx, hyp.PlatformExperimentID, hyp.ID, agentID, branch)
	if err != nil {
		return nil, fmt.Errorf("registry.CheckOut: %w", err)
	}
	out := &Checkout{HypothesisID: hyp.ID, AgentID: agentID, Branch: branch, Working: others}
	if len(others) > 0 {
		names := make([]string, len(others))
		for i, w := range others {
			names[i] = w.Name
		}
		out.Warning = fmt.Sprintf("%s already works on hypothesis %s (a job in flight or a checkout within two hours). "+
			"A different approach to the same idea can be worth running beside theirs; one that repeats it wastes a seat: "+
			"read hl hypothesis get %s and take another hypothesis if yours would", strings.Join(names, ", "), hyp.ID, hyp.ID)
	}
	s.logger.Info("hypothesis checked out", zap.String("id", hyp.ID), zap.String("agent", agentID), zap.Int("others", len(others)))
	return out, nil
}

// Working is, for each of ids with any, the agents working on it now.
func (s *Service) Working(ctx context.Context, ids []string) (map[string][]domain.HypothesisWorker, error) {
	working, err := s.store.Working(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("registry.Working: %w", err)
	}
	return working, nil
}

// TakenHypotheses are the ids of the hypotheses some agent works on now, within one platform
// experiment or, empty, all of them.
func (s *Service) TakenHypotheses(ctx context.Context, platformExpID string) ([]string, error) {
	ids, err := s.store.TakenHypotheses(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("registry.TakenHypotheses: %w", err)
	}
	return ids, nil
}

func registerCheckoutHuma(doc *apidocs.Doc, h *Handler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "check-out-hypothesis", Method: "POST", Path: "/hypotheses/{id}/checkouts",
		Summary: "Record that an agent works on a hypothesis", Tags: []string{"hypotheses"},
		Description: "hl hypothesis checkout calls this once the branch is checked out: from then the agent is in working of " +
			"GET /hypotheses and GET /hypotheses/{id}, its branch in implementations, and the hypothesis in hypotheses_in_flight " +
			"of the situation, for two hours or until a job of its shows it. Checking out again renews it. Nothing is refused for " +
			"another agent on the same hypothesis: the answer names them in working, with a warning. agent_id is the agent's: " +
			"an agent token's own, named or not; any other caller names an agent it reaches. 403 not_signed_up for an agent " +
			"outside the hypothesis's platform experiment.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string `json:"agent_id,omitempty" required:"false" doc:"The agent that checked it out. Absent, the agent token's own."`
			Branch  string `json:"branch" doc:"The branch checked out for it."`
		}
	}) (*struct{ Body *Checkout }, error) {
		if in.Body.Branch == "" {
			return nil, huma.Error400BadRequest("branch is required")
		}
		hyp, err := h.svc.GetHypothesis(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err)
		}
		if hyp == nil {
			return nil, auth.NotFound("hypothesis")
		}
		if err := access.Within(ctx, "hypothesis", hyp.PlatformExperimentID); err != nil {
			return nil, err
		}
		agentID, _, err := access.Writer(ctx, in.Body.AgentID)
		if err != nil {
			return nil, err
		}
		if agentID == "" {
			return nil, huma.Error400BadRequest("agent_id is required: a checkout is an agent's")
		}
		out, err := h.svc.CheckOut(ctx, hyp, agentID, in.Body.Branch)
		if err != nil {
			return nil, h.hypothesisWriteError(err)
		}
		return &struct{ Body *Checkout }{Body: out}, nil
	})
}
