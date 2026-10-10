package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// checkoutStore answers that others work on the hypothesis already.
type checkoutStore struct {
	*poolStore
	others   []domain.HypothesisWorker
	recorded []string
}

func (s *checkoutStore) CheckOutHypothesis(_ context.Context, _, hypothesisID, agentID, branch string) ([]domain.HypothesisWorker, error) {
	s.recorded = append(s.recorded, hypothesisID+" "+agentID+" "+branch)
	return s.others, nil
}

// Two researchers started the same people's ideas within two minutes because nothing showed a
// claim before a job. A checkout is recorded, and one of a hypothesis another agent works on is
// told who, as a warning: two approaches to one idea can both be worth running.
func TestACheckoutIsRecordedAndOneOfAClaimedHypothesisSaysWhoHasIt(t *testing.T) {
	store := &checkoutStore{poolStore: newPoolStore()}
	svc := poolService(store)
	hyp := &domain.Hypothesis{ID: "hyp-1", PlatformExperimentID: "pe-1"}

	alone, err := svc.CheckOut(context.Background(), hyp, "agent-a", "hl/pe-1/a/idea")
	if err != nil || alone.Warning != "" || len(alone.Working) != 0 || len(store.recorded) != 1 || store.recorded[0] != "hyp-1 agent-a hl/pe-1/a/idea" {
		t.Fatalf("a first checkout: got = %+v, %v (recorded %v), want it recorded with no warning", alone, err, store.recorded)
	}

	store.others = []domain.HypothesisWorker{{AgentID: "agent-a", Name: "researcher-1"}}
	second, err := svc.CheckOut(context.Background(), hyp, "agent-b", "hl/pe-1/b/idea")
	if err != nil || len(second.Working) != 1 || len(store.recorded) != 2 {
		t.Fatalf("a second checkout: got = %+v, %v, want it recorded and answered with who works on it", second, err)
	}
	for _, want := range []string{"researcher-1", "hyp-1", "different approach"} {
		if !strings.Contains(second.Warning, want) {
			t.Errorf("warning %q does not say %q", second.Warning, want)
		}
	}

	if _, err := svc.CheckOut(context.Background(), hyp, "agent-elsewhere", "hl/pe-1/x/idea"); !errors.Is(err, db.ErrNotSignedUp) || len(store.recorded) != 2 {
		t.Errorf("an agent outside the platform experiment: err = %v (recorded %v), want ErrNotSignedUp and nothing recorded", err, store.recorded)
	}
}
