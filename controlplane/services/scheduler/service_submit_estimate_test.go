package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type measuredStore struct {
	poolOnlyStore
	measured float64
}

func (s measuredStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", JobEstimatedDurationHours: s.measured}, nil
}

func (measuredStore) GetAgentSignup(context.Context, string, string) (*domain.AgentSignup, error) {
	return nil, nil
}

type baselineStore struct {
	poolOnlyStore
	maxJobHours float64
}

func (s baselineStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpOpen, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git",
		Stages: []domain.Stage{{LengthPct: 100, MaxJobHours: s.maxJobHours}}, CurrentStage: 1}, nil
}

func (baselineStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleBaseline, Flavor: domain.DefaultAgentFlavor}, nil
}

// shareQuota answers one agent's quota as admission counts it, nil for an agent with none.
type shareQuota struct {
	QuotaService
	quota *domain.AgentQuota
}

func (q shareQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return q.quota, nil
}

// Nothing is measured before the baseline, so its agent guessed: 0.25 h against a small share was
// refused 402, and the job then ran 10 s. A baseline job that states no estimate takes the largest
// its agent's quota admits, within the stage's per-job limit, and a stated one stands.
func TestABaselineJobThatStatesNoEstimateTakesTheLargestItsQuotaAdmits(t *testing.T) {
	share := func(guaranteed, used float64) *domain.AgentQuota {
		return &domain.AgentQuota{GuaranteedAcceleratorHours: guaranteed, UsedGuaranteedAccH: used}
	}
	for name, tc := range map[string]struct {
		stated      float64
		quota       *domain.AgentQuota
		maxJobHours float64
		want        float64
		reason      string
	}{
		"all that remains of the share": {0, share(0.5, 0.25), 0, 0.25, ""},
		"rounded down to a step":        {0, share(0.33337, 0), 0, 0.3333, ""},
		"within the per-job limit":      {0, share(0.5, 0.25), 0.125, 0.125, ""},
		"a stated one stands":           {0.0625, nil, 0, 0.0625, ""},
		"nothing remains":               {0, share(0.5, 0.5), 0, 0, ReasonInsufficientCredits},
		"no quota":                      {0, nil, 0, 0, "no_quota"},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: baselineStore{maxJobHours: tc.maxJobHours}, quota: shareQuota{quota: tc.quota}, hypotheses: oneHypothesis{}}
		exp := baselineSubmission()
		exp.EstimatedDurationHours = tc.stated
		err := s.Submit(context.Background(), exp)
		var refused *AdmissionError
		switch {
		case tc.reason != "":
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Errorf("%s: got = %v, want %s", name, err, tc.reason)
			}
		case !errors.Is(err, errReachedScoring) || exp.EstimatedDurationHours != tc.want || exp.EstimatedCostAccH != tc.want:
			t.Errorf("%s: got = %v with an estimate of %g holding %g, want the job taken with %g", name, err, exp.EstimatedDurationHours, exp.EstimatedCostAccH, tc.want)
		}
	}
	if got := admissionHTTPStatus(ReasonInsufficientCredits); got != 402 {
		t.Errorf("a baseline job nothing fits is answered %d, want 402", got)
	}

	// A researcher's job has the measured estimate or states one: the largest that fits would hold
	// its whole share for one trial.
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, quota: shareQuota{quota: share(0.5, 0)}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()
	exp.EstimatedDurationHours = 0
	var refused *AdmissionError
	if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonEstimateRequired {
		t.Errorf("a researcher's job with no estimate and none measured: got = %v, want %s", err, ReasonEstimateRequired)
	}
}

// Held to the estimate its spec guessed before anything ran, a baseline ran twice, the second
// time only to carry a corrected one. The estimate of a job that states none is the one the
// platform measured on the baseline; a stated one stands, and with neither the job is refused.
func TestAJobThatStatesNoEstimateTakesTheOneItsExperimentMeasured(t *testing.T) {
	for name, tc := range map[string]struct {
		measured float64
		stated   float64
		reason   string
		want     float64
	}{
		"stated, where one was measured":   {0.025, 0.5, "not_signed_up", 0.5},
		"not stated, and one was measured": {0.025, 0, "not_signed_up", 0.025},
		"not stated, and none measured":    {0, 0, ReasonEstimateRequired, 0},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: measuredStore{measured: tc.measured}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.EstimatedDurationHours = tc.stated
		var admission *AdmissionError
		// The signup check comes after the estimate is settled, and this store signs nobody up.
		if err := s.Submit(context.Background(), exp); !errors.As(err, &admission) || admission.Reason != tc.reason || exp.EstimatedDurationHours != tc.want {
			t.Errorf("%s: got = %v with an estimate of %g, want %s and %g", name, err, exp.EstimatedDurationHours, tc.reason, tc.want)
		}
		if tc.reason == ReasonEstimateRequired && (admissionHTTPStatus(admission.Reason) != 422 || !strings.Contains(admission.Message, "--estimated-duration-hours")) {
			t.Errorf("%s: refused %d %q, want 422 naming the flag", name, admissionHTTPStatus(admission.Reason), admission.Message)
		}
	}
}
