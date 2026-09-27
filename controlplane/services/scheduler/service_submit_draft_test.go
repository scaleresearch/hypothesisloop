package scheduler

import (
	"context"
	"errors"
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
