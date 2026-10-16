package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// exhaustionStore is the controller's Store for one agent's jobs in one platform experiment. It
// records every termination it is asked for and fails the ones named in failOn.
type exhaustionStore struct {
	Store
	running, preRun []*domain.Experiment
	preRunErr       error
	failOn          map[string]bool
	terminated      []string
}

func (s *exhaustionStore) GetAgentRunningExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return s.running, nil
}

func (s *exhaustionStore) GetAgentQueuedExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return s.preRun, s.preRunErr
}

func (s *exhaustionStore) ResolveTermination(_ context.Context, id string, _, _ domain.ExperimentStatus, _ string) (domain.Termination, error) {
	if s.failOn[id] {
		return domain.TerminationSkipped, errors.New("write failed for " + id)
	}
	s.terminated = append(s.terminated, id)
	return domain.TerminationWritten, nil
}

func (s *exhaustionStore) MarkQuotaSettled(context.Context, string) error { return nil }

type observedQuota struct {
	QuotaService
	quota *domain.AgentQuota
}

func (q observedQuota) GetObservedAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return q.quota, nil
}

type settledNothing struct{}

func (settledNothing) Settle(context.Context, *domain.Experiment) error { return nil }

func guaranteedJob(id string, status domain.ExperimentStatus) *domain.Experiment {
	return &domain.Experiment{ID: id, Status: status, CapacityTier: domain.CapacityGuaranteed}
}

func exhaustionController(store *exhaustionStore, quota *domain.AgentQuota) *Controller {
	return New(store, observedQuota{quota: quota}, zap.NewNop()).WithSettler(settledNothing{}).WithResearcherKeeper(keptNothing{})
}

// Exhaustion fired at 99% of the budget: an agent with 100 accelerator-hours lost its running
// jobs, unrefunded, with one hour unspent. A budget is exhausted when observed usage reaches it.
func TestQuotaIsExhaustedWhenObservedUsageReachesTheBudget(t *testing.T) {
	for _, tc := range []struct {
		used    float64
		evicted bool
	}{
		{99, false},
		{99.9, false},
		{100 - 1e-9, true},
		{100, true},
		{100.5, true},
	} {
		store := &exhaustionStore{running: []*domain.Experiment{guaranteedJob("job-1", domain.StatusRunning)}}
		c := exhaustionController(store, &domain.AgentQuota{GuaranteedAcceleratorHours: 100, UsedGuaranteedAccH: tc.used})
		if err := c.checkQuotaExhaustion(context.Background(), "agent-1", "pe-1"); err != nil {
			t.Fatalf("used %v: checkQuotaExhaustion: got = %v, want nil", tc.used, err)
		}
		if got := len(store.terminated) == 1; got != tc.evicted {
			t.Errorf("used %v of 100: evicted = %v, want %v", tc.used, got, tc.evicted)
		}
	}
}

// A termination that could not be written was logged and dropped, and so was a pre-run list that
// could not be read: the pass answered nil with jobs still running on a spent budget. Every other
// job is still ended, and what failed is returned.
func TestQuotaExhaustionReturnsEveryFailureAndEndsTheOtherJobs(t *testing.T) {
	spent := &domain.AgentQuota{GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 10}
	store := &exhaustionStore{
		running: []*domain.Experiment{guaranteedJob("job-1", domain.StatusRunning), guaranteedJob("job-2", domain.StatusRunning)},
		preRun:  []*domain.Experiment{guaranteedJob("job-3", domain.StatusQueued), guaranteedJob("job-4", domain.StatusQueued)},
		failOn:  map[string]bool{"job-1": true, "job-3": true},
	}
	err := exhaustionController(store, spent).checkQuotaExhaustion(context.Background(), "agent-1", "pe-1")
	if err == nil || !strings.Contains(err.Error(), "job-1") || !strings.Contains(err.Error(), "job-3") {
		t.Errorf("checkQuotaExhaustion: got = %v, want the failures of job-1 and job-3", err)
	}
	if got := strings.Join(store.terminated, ","); got != "job-2,job-4" {
		t.Errorf("terminated: got = %s, want job-2,job-4", got)
	}

	store = &exhaustionStore{
		running:   []*domain.Experiment{guaranteedJob("job-1", domain.StatusRunning)},
		preRunErr: errors.New("query failed"),
		failOn:    map[string]bool{"job-1": true},
	}
	err = exhaustionController(store, spent).checkQuotaExhaustion(context.Background(), "agent-1", "pe-1")
	if err == nil || !strings.Contains(err.Error(), "job-1") || !strings.Contains(err.Error(), "query failed") {
		t.Errorf("checkQuotaExhaustion with an unreadable pre-run list: got = %v, want that failure and job-1's", err)
	}
}

// An agent whose jobs run with no quota row has nothing bounding what it spends, which the pass
// answered with nil.
func TestQuotaExhaustionRefusesAnAgentWithRunningJobsAndNoQuota(t *testing.T) {
	store := &exhaustionStore{running: []*domain.Experiment{guaranteedJob("job-1", domain.StatusRunning)}}
	if err := exhaustionController(store, nil).checkQuotaExhaustion(context.Background(), "agent-1", "pe-1"); err == nil {
		t.Error("checkQuotaExhaustion with no quota row: got = nil, want an error")
	}
}
