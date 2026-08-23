package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// jobSubmitPolicyStore lets each test fix the platform experiment's JobSubmitPolicy and the
// submitting agent's Kind independently — exactly the two inputs Submit's job_submit_policy gate
// (service_submit.go) reads. Reuses poolOnlyStore's GetRunningAndQueued short-circuit
// (errReachedScoring, see service_submit_test.go) so a test proving admission never needs quota
// or persistence stood up.
type jobSubmitPolicyStore struct {
	poolOnlyStore
	policy domain.SubmitterPolicy
	kind   domain.AgentKind
}

func (s jobSubmitPolicyStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, JobSubmitPolicy: s.policy}, nil
}

func (s jobSubmitPolicyStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return &domain.Agent{ID: id, Kind: s.kind}, nil
}

// agentOwnHypothesis is the one row jobSubmitPolicySubmission names — an agent's own hypothesis
// under pe-1, so the job_submit_policy gate (which runs before the hypothesis binding) is the
// only thing a test here is exercising.
func agentOwnHypothesis() oneHypothesis {
	return oneHypothesis{h: &domain.Hypothesis{
		ID: "h-agent", Source: domain.HypothesisSourceAgent, AgentID: "agent-1",
		PlatformExperimentID: "pe-1", Text: "an agent's own idea",
	}}
}

func jobSubmitPolicySubmission() *domain.Experiment {
	retries := 0
	return &domain.Experiment{
		ID: "policy-job", AgentID: "agent-1", ProjectID: "proj", PlatformExperimentID: "pe-1",
		HypothesisID: "h-agent", Theory: "t", Objective: "o", EstimatedDurationHours: 1,
		CodeRef: "https://example.com/repo.git@" + strings.Repeat("a", 40),
		Job: domain.JobSpec{
			Image: "img", CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries,
		},
	}
}

func TestSubmitRejectsAHumanWhenJobSubmitPolicyIsAgentOnly(t *testing.T) {
	s := &Service{
		store:      jobSubmitPolicyStore{policy: domain.SubmitterPolicyAgentOnly, kind: domain.AgentKindHuman},
		hypotheses: agentOwnHypothesis(),
	}
	err := s.Submit(context.Background(), jobSubmitPolicySubmission())
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("Submit: got = %v, want an AdmissionError", err)
	}
	if admission.Reason != ReasonSubmitterNotAllowed {
		t.Errorf("reason: got = %v, want %v", admission.Reason, ReasonSubmitterNotAllowed)
	}
}

func TestSubmitAllowsAnAgentWhenJobSubmitPolicyIsAgentOnly(t *testing.T) {
	s := &Service{
		store:      jobSubmitPolicyStore{policy: domain.SubmitterPolicyAgentOnly, kind: domain.AgentKindAgent},
		hypotheses: agentOwnHypothesis(),
	}
	if err := s.Submit(context.Background(), jobSubmitPolicySubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to pass the policy gate and reach scoring", err)
	}
}

func TestSubmitRejectsAnAgentWhenJobSubmitPolicyIsHumanOnly(t *testing.T) {
	s := &Service{
		store:      jobSubmitPolicyStore{policy: domain.SubmitterPolicyHumanOnly, kind: domain.AgentKindAgent},
		hypotheses: agentOwnHypothesis(),
	}
	err := s.Submit(context.Background(), jobSubmitPolicySubmission())
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("Submit: got = %v, want an AdmissionError", err)
	}
	if admission.Reason != ReasonSubmitterNotAllowed {
		t.Errorf("reason: got = %v, want %v", admission.Reason, ReasonSubmitterNotAllowed)
	}
}

func TestSubmitAllowsAHumanWhenJobSubmitPolicyIsHumanOnly(t *testing.T) {
	s := &Service{
		store:      jobSubmitPolicyStore{policy: domain.SubmitterPolicyHumanOnly, kind: domain.AgentKindHuman},
		hypotheses: agentOwnHypothesis(),
	}
	if err := s.Submit(context.Background(), jobSubmitPolicySubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to pass the policy gate and reach scoring", err)
	}
}

func TestSubmitAllowsBothKindsWhenJobSubmitPolicyIsMixed(t *testing.T) {
	for _, kind := range []domain.AgentKind{domain.AgentKindAgent, domain.AgentKindHuman} {
		s := &Service{
			store:      jobSubmitPolicyStore{policy: domain.SubmitterPolicyMixed, kind: kind},
			hypotheses: agentOwnHypothesis(),
		}
		if err := s.Submit(context.Background(), jobSubmitPolicySubmission()); !errors.Is(err, errReachedScoring) {
			t.Errorf("Submit (kind=%v): got = %v, want it to pass the policy gate and reach scoring", kind, err)
		}
	}
}

// job_submit_policy governs jobs; a platform experiment restricting THAT must not silently also
// block hypothesis registration, which hypothesis_submit_policy owns independently (see
// registry/hypotheses_submit_policy_test.go) — an operator wanting agent-run jobs against
// human-curated hypotheses depends on the two staying decoupled.
func TestSubmitDoesNotConsultHypothesisSubmitPolicyForJobAdmission(t *testing.T) {
	s := &Service{
		store: jobSubmitPolicyStore{
			poolOnlyStore: poolOnlyStore{},
			policy:        domain.SubmitterPolicyMixed,
			kind:          domain.AgentKindAgent,
		},
		hypotheses: humanHypothesis("pe-1"),
	}
	exp := humanIdeaSubmission()
	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to pass the job_submit_policy gate and reach scoring", err)
	}
}
