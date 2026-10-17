package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

type endedJobStore struct {
	poolOnlyStore
	status domain.ExperimentStatus
	reason string
}

func (s *endedJobStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	return &domain.Experiment{ID: id, Status: s.status, EvictionReason: s.reason}, nil
}

// A cancelled job is stored REJECTED or EVICTED and answered CANCELLED. A cancel of it is refused
// in the status every read shows.
func TestACancelOfAnEndedJobIsRefusedInTheStatusItIsAnsweredWith(t *testing.T) {
	for _, c := range []struct {
		stored domain.ExperimentStatus
		reason string
		want   string
	}{
		{domain.StatusRejected, string(domain.EvictionCancelled), "CANCELLED"},
		{domain.StatusEvicted, string(domain.EvictionCancelled), "CANCELLED"},
		{domain.StatusEvicted, string(domain.EvictionSilent), "EVICTED"},
		{domain.StatusCompleted, "", "COMPLETED"},
	} {
		s := &Service{store: &endedJobStore{status: c.stored, reason: c.reason}}
		refusal := ChangeRefusal(s.CancelExperiment(context.Background(), "job-1"))
		if refusal == nil || refusal.GetStatus() != http.StatusConflict {
			t.Fatalf("cancel of a job stored %s (%s): got = %v, want a 409", c.stored, c.reason, refusal)
		}
		body, _ := json.Marshal(refusal)
		var answer struct{ Reason, Message string }
		if json.Unmarshal(body, &answer) != nil || answer.Reason != ReasonInvalidTransition || !strings.Contains(answer.Message, "job-1 is "+c.want+":") {
			t.Errorf("cancel of a job stored %s (%s): body = %s, want reason %s naming it %s", c.stored, c.reason, body, ReasonInvalidTransition, c.want)
		}
	}
}
