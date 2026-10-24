package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
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

// ErrChecksOutNothing is a checkout by an agent whose role submits no job.
var ErrChecksOutNothing = errors.New("a researcher runs no job, so it checks out no hypothesis: register the idea for an executor to take, or comment on it")

// CheckOut records that agentID, an agent of the hypothesis's platform experiment that submits
// jobs, checked hyp out on branch, so the agents and the coordinator see who works on it before a
// job does. A merged or retired hypothesis is refused. Another agent on it is said in a warning:
// two approaches to one idea can both be worth running.
func (s *Service) CheckOut(ctx context.Context, hyp *domain.Hypothesis, agentID, branch string) (*Checkout, error) {
	signup, err := s.store.GetAgentSignup(ctx, hyp.PlatformExperimentID, agentID)
	if err != nil {
		return nil, fmt.Errorf("registry.CheckOut: %w", err)
	}
	if signup == nil {
		return nil, domain.ErrNotSignedUp
	}
	if !signup.Role.SubmitsJobs() {
		return nil, ErrChecksOutNothing
	}
	if hyp.Curated() {
		return nil, &graphdb.CuratedError{Hypothesis: hyp, Refused: "takes no job"}
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
