package platformexperiments

import "testing"

// Two agents at 0.925 were shown as first and second, by the order of their ids.
func TestAgentsWithTheSameBestShareARank(t *testing.T) {
	standings := []AgentStanding{{AgentID: "a", Best: 0.93}, {AgentID: "b", Best: 0.925}, {AgentID: "c", Best: 0.925}, {AgentID: "d", Best: 0.9}}
	assignRanks(standings)
	for i, want := range []int{1, 2, 2, 4} {
		if standings[i].Rank != want {
			t.Errorf("%s with best %v has rank %d, want %d", standings[i].AgentID, standings[i].Best, standings[i].Rank, want)
		}
	}
}
