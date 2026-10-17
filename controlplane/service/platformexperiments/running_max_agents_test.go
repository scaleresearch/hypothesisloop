package platformexperiments

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
)

// seatStore is createdStore with the quotas of its researchers, and the hours each used.
type seatStore struct {
	createdStore
	quotas map[string]domain.AgentQuota
	cut    map[string]bool
}

func (s *seatStore) GetAgentQuota(_ context.Context, agentID, _ string) (*domain.AgentQuota, error) {
	q, ok := s.quotas[agentID]
	if !ok {
		return nil, nil
	}
	q.AgentID = agentID
	return &q, nil
}

func (s *seatStore) IsAgentCut(_ context.Context, _, agentID string) (bool, error) {
	return s.cut[agentID], nil
}

// UpdatePlatformExperiment holds max_agents to the researchers at work: a cut one is not.
func (s *seatStore) UpdatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, researchers domain.ResearcherPool) error {
	atWork := 0
	for _, id := range s.researchers {
		if !s.cut[id] {
			atWork++
		}
	}
	return s.update(pe, status, researchers, atWork)
}

func (s *seatStore) AddDesiredQuotaUsageOne(context.Context, *domain.AgentQuota) error { return nil }

func (s *seatStore) GetAgentRunningExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return nil, nil
}

// The owner who wants more researchers at work had to close the run and start another. max_agents
// changes while it runs, for the places nobody took, as long as what the researchers left unspent
// still holds one trial of the baseline for each of them.
func TestARunningExperimentTakesAMaxAgentsItsUnspentHoursHold(t *testing.T) {
	update := func(maxAgents int, cut ...string) (*seatStore, error) {
		t.Helper()
		store := &seatStore{
			createdStore: createdStore{created: []*domain.PlatformExperiment{runningOnBaseline()}, researchers: []string{"ag-1", "ag-2", "ag-3"}},
			// 2 + 2.5 unspent, and ag-3 cut with all of its share.
			quotas: map[string]domain.AgentQuota{
				"ag-1": {GuaranteedAcceleratorHours: 3, UsedGuaranteedAccH: 1},
				"ag-2": {GuaranteedAcceleratorHours: 3, UsedBurstAccH: 0.5},
				"ag-3": {GuaranteedAcceleratorHours: 3},
			},
			cut: map[string]bool{"ag-3": true},
		}
		for _, id := range cut {
			store.cut[id] = true
		}
		svc := NewPlatformExperimentsService(store, platformexperimentstest.Settings, zap.NewNop(), platformexperimentstest.NoUsage(t), time.Minute)
		svc.baseline = func(context.Context, *domain.PlatformExperiment, string) (*Baseline, error) {
			return measuredBaseline(), nil
		}
		_, err := svc.Update(context.Background(), "pe-1", PlatformExperimentPatch{MaxAgents: &maxAgents})
		return store, err
	}
	reason := func(err error) string {
		var refused *refusal.Refusal
		if errors.As(err, &refused) {
			return refused.Reason
		}
		return ""
	}

	// A trial of the baseline holds 2 h100 for 0.3 h: 0.6, and 4.5 over 7 is 0.64 each.
	if store, err := update(7); err != nil || store.created[0].MaxAgents != 7 {
		t.Errorf("seven places 4.5 unspent hours hold: got = %v, stored %d, want 7", err, store.created[0].MaxAgents)
	}
	// The store cuts the newest researchers above a lowered max_agents of a running one.
	if store, err := update(1); err != nil || store.created[0].MaxAgents != 1 {
		t.Errorf("fewer places than at work: got = %v, stored %d, want 1", err, store.created[0].MaxAgents)
	}
	for name, c := range map[string]struct {
		maxAgents int
		cut       []string
		reason    string
	}{
		"more places than a trial apiece":           {8, nil, "budget_below_baseline"},
		"a cut researcher's hours count for nobody": {5, []string{"ag-2"}, "budget_below_baseline"},
	} {
		store, err := update(c.maxAgents, c.cut...)
		if reason(err) != c.reason || store.created[0].MaxAgents != 2 {
			t.Errorf("%s: got = %v, stored %d, want %s and max_agents kept at 2", name, err, store.created[0].MaxAgents, c.reason)
		}
	}
}
