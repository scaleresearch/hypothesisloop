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

func TestInterleaveByAgentBoundsQueueDepthAdvantage(t *testing.T) {
	// agent-a has 5 jobs queued, agent-b has 1 — without interleaving, a single linear
	// admission pass over this priority order would let agent-a claim every slot that frees
	// up this tick before agent-b's only job is ever reached.
	exps := []*domain.Experiment{
		{ID: "a1", AgentID: "agent-a"},
		{ID: "a2", AgentID: "agent-a"},
		{ID: "a3", AgentID: "agent-a"},
		{ID: "a4", AgentID: "agent-a"},
		{ID: "a5", AgentID: "agent-a"},
		{ID: "b1", AgentID: "agent-b"},
	}
	got := interleaveByAgent(exps)
	if len(got) != len(exps) {
		t.Fatalf("interleaveByAgent dropped or added jobs: got %d, want %d", len(got), len(exps))
	}
	if got[0].ID != "a1" || got[1].ID != "b1" {
		t.Errorf("interleaveByAgent order = %v, want agent-b's only job admitted in round 1 (second overall), not after all of agent-a's", ids(got))
	}
	// agent-a's own relative order among its jobs must be untouched.
	var aOrder []string
	for _, e := range got {
		if e.AgentID == "agent-a" {
			aOrder = append(aOrder, e.ID)
		}
	}
	want := []string{"a1", "a2", "a3", "a4", "a5"}
	for i := range want {
		if aOrder[i] != want[i] {
			t.Errorf("agent-a's relative order = %v, want %v (unchanged)", aOrder, want)
			break
		}
	}
}

func TestInterleaveByAgentHandlesEmptyAndSingleAgent(t *testing.T) {
	if got := interleaveByAgent(nil); len(got) != 0 {
		t.Errorf("interleaveByAgent(nil) = %v, want empty", got)
	}
	exps := []*domain.Experiment{
		{ID: "x1", AgentID: "agent-a"},
		{ID: "x2", AgentID: "agent-a"},
	}
	got := interleaveByAgent(exps)
	if ids(got) != "x1,x2" {
		t.Errorf("single-agent interleave = %v, want unchanged order [x1 x2]", ids(got))
	}
}

func ids(exps []*domain.Experiment) string {
	s := ""
	for i, e := range exps {
		if i > 0 {
			s += ","
		}
		s += e.ID
	}
	return s
}
