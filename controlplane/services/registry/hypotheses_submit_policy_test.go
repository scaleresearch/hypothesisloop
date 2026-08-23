package registry

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// policyStore is poolStore plus a fixed submitter policy for the one platform experiment ID the
// tests here use — the narrow read RegisterHypothesis gates on.
type policyStore struct {
	poolStore
	hypothesisPolicy domain.SubmitterPolicy
}

func (s *policyStore) GetPlatformExperimentSubmitPolicies(_ context.Context, _ string) (domain.SubmitterPolicy, domain.SubmitterPolicy, bool, error) {
	return s.hypothesisPolicy, domain.SubmitterPolicyMixed, true, nil
}

func newPolicyTestService(policy domain.SubmitterPolicy) *Service {
	return New(&policyStore{poolStore: *newPoolStore(), hypothesisPolicy: policy}, zap.NewNop(), "", nil)
}

// A human_only platform experiment must refuse an autonomous agent's submission with a message
// that names the policy and what to check — an agent that fired this off automatically without
// reading the experiment's rules needs to know why, not just that it failed.
func TestRegisterHypothesisRejectsAnAgentWhenPolicyIsHumanOnly(t *testing.T) {
	svc := newPolicyTestService(domain.SubmitterPolicyHumanOnly)

	_, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps")
	if !errors.Is(err, ErrHypothesisSubmitterNotAllowed) {
		t.Fatalf("RegisterHypothesis (agent, human_only policy): err = %v, want %v", err, ErrHypothesisSubmitterNotAllowed)
	}
}

// The same policy must let a human through — that's the whole point of the restriction.
func TestRegisterHypothesisAllowsAHumanWhenPolicyIsHumanOnly(t *testing.T) {
	svc := newPolicyTestService(domain.SubmitterPolicyHumanOnly)

	h, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis (human, human_only policy): err = %v, want nil", err)
	}
	if h.Source != domain.HypothesisSourceHuman {
		t.Errorf("source = %v, want %v", h.Source, domain.HypothesisSourceHuman)
	}
}

// An agent_only platform experiment must refuse a human submission.
func TestRegisterHypothesisRejectsAHumanWhenPolicyIsAgentOnly(t *testing.T) {
	svc := newPolicyTestService(domain.SubmitterPolicyAgentOnly)

	_, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps")
	if !errors.Is(err, ErrHypothesisSubmitterNotAllowed) {
		t.Fatalf("RegisterHypothesis (human, agent_only policy): err = %v, want %v", err, ErrHypothesisSubmitterNotAllowed)
	}
}

// The same policy must let an agent through.
func TestRegisterHypothesisAllowsAnAgentWhenPolicyIsAgentOnly(t *testing.T) {
	svc := newPolicyTestService(domain.SubmitterPolicyAgentOnly)

	h, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis (agent, agent_only policy): err = %v, want nil", err)
	}
	if h.Source != domain.HypothesisSourceAgent {
		t.Errorf("source = %v, want %v", h.Source, domain.HypothesisSourceAgent)
	}
}

// Mixed accepts both — today's default behavior, unchanged.
func TestRegisterHypothesisAllowsBothSourcesWhenPolicyIsMixed(t *testing.T) {
	svc := newPolicyTestService(domain.SubmitterPolicyMixed)

	if _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps"); err != nil {
		t.Fatalf("RegisterHypothesis (agent, mixed policy): err = %v, want nil", err)
	}
	if _, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "cooldown helps"); err != nil {
		t.Fatalf("RegisterHypothesis (human, mixed policy): err = %v, want nil", err)
	}
}
