package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type draftStore struct{ poolOnlyStore }

func (draftStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpDraft, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git"}, nil
}

func (draftStore) IsSignedUp(context.Context, string, string) (bool, error) { return false, nil }

// A draft takes its planner's baseline job, so being a draft is not what refuses a job there:
// being signed up is, and a draft's only signup is the planner.
func TestSubmitToADraftIsHeldBySignupAndNotByItsStatus(t *testing.T) {
	s := &Service{codeHost: testCodeHost{}, store: draftStore{}, hypotheses: humanHypothesis("pe-1")}
	var admission *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &admission) || admission.Reason != "not_signed_up" {
		t.Fatalf("Submit to a draft by someone who is not its planner: got = %v, want not_signed_up", err)
	}
}

type measuredStore struct {
	poolOnlyStore
	proposal string
}

func (s measuredStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", Proposal: []byte(s.proposal)}, nil
}

func (measuredStore) IsSignedUp(context.Context, string, string) (bool, error) { return false, nil }

// Held to the estimate its spec guessed before anything ran, a baseline ran twice, the second
// time only to carry a corrected one. The estimate of a job that states none is the one the
// platform measured on the baseline; a stated one stands, and with neither the job is refused.
func TestAJobThatStatesNoEstimateTakesTheOneItsExperimentMeasured(t *testing.T) {
	const measured = `{"baseline_job_id":"job-1","job_estimated_duration_hours":0.025}`
	for name, tc := range map[string]struct {
		proposal string
		stated   float64
		reason   string
		want     float64
	}{
		"stated, where one was measured":   {measured, 0.5, "not_signed_up", 0.5},
		"not stated, and one was measured": {measured, 0, "not_signed_up", 0.025},
		"not stated, and none measured":    {`{"baseline_job_id":"job-1"}`, 0, ReasonEstimateRequired, 0},
		"not stated, created directly":     {"", 0, ReasonEstimateRequired, 0},
	} {
		s := &Service{codeHost: testCodeHost{}, store: measuredStore{proposal: tc.proposal}, hypotheses: humanHypothesis("pe-1")}
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
