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

func (measuredStore) IsSignedUp(context.Context, string, string) (bool, error) { return false, nil }

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
		s := &Service{clusters: installedCapacity{}, codeHost: testCodeHost{}, store: measuredStore{measured: tc.measured}, hypotheses: humanHypothesis("pe-1")}
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
