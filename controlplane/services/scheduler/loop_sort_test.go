package scheduler

import (
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestSortBurstOrdersByDominantUtilizationOverPriorityScore(t *testing.T) {
	t0 := time.Now().UTC()
	agentA := &domain.AgentQuota{GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 9} // 90% used
	agentB := &domain.AgentQuota{GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 1} // 10% used
	quotaMap := map[string]*domain.AgentQuota{
		quotaKey("agent-a", "pe-1"): agentA,
		quotaKey("agent-b", "pe-1"): agentB,
	}
	// agent-a's job has a much higher PriorityScore, but agent-b should still be scheduled
	// first: dominant utilization is the primary signal, PriorityScore only a final tiebreak.
	expA := &domain.Experiment{ID: "a", AgentID: "agent-a", PlatformExperimentID: "pe-1", EstimatedCostAccH: 1, PriorityScore: 100, QueuedAt: &t0}
	expB := &domain.Experiment{ID: "b", AgentID: "agent-b", PlatformExperimentID: "pe-1", EstimatedCostAccH: 1, PriorityScore: 0, QueuedAt: &t0}

	exps := []*domain.Experiment{expA, expB}
	sortBurst(exps, quotaMap, nil)

	if exps[0].ID != "b" {
		t.Errorf("sortBurst order = [%s, %s], want b first (agent-b has lower dominant utilization)", exps[0].ID, exps[1].ID)
	}
}

func TestSortBurstFallsBackToPriorityScoreWhenUtilizationTied(t *testing.T) {
	t0 := time.Now().UTC()
	quotaMap := map[string]*domain.AgentQuota{} // no quota rows — both utilizations are 0

	lowPriority := &domain.Experiment{ID: "low", AgentID: "agent-a", PlatformExperimentID: "pe-1", PriorityScore: 1, QueuedAt: &t0}
	highPriority := &domain.Experiment{ID: "high", AgentID: "agent-b", PlatformExperimentID: "pe-1", PriorityScore: 5, QueuedAt: &t0}

	exps := []*domain.Experiment{lowPriority, highPriority}
	sortBurst(exps, quotaMap, nil)

	if exps[0].ID != "high" {
		t.Errorf("sortBurst order = [%s, %s], want high first (higher PriorityScore breaks the tie)", exps[0].ID, exps[1].ID)
	}
}

func TestDominantCostFractionTiebreakPrefersSmallerJob(t *testing.T) {
	quotaMap := map[string]*domain.AgentQuota{
		quotaKey("agent-a", "pe-1"): {GuaranteedAcceleratorHours: 10},
	}
	small := &domain.Experiment{AgentID: "agent-a", PlatformExperimentID: "pe-1", EstimatedCostAccH: 1}
	big := &domain.Experiment{AgentID: "agent-a", PlatformExperimentID: "pe-1", EstimatedCostAccH: 9}

	if got := dominantCostFraction(quotaMap, small); got >= dominantCostFraction(quotaMap, big) {
		t.Errorf("dominantCostFraction(small)=%v should be less than dominantCostFraction(big)=%v", got, dominantCostFraction(quotaMap, big))
	}
}
