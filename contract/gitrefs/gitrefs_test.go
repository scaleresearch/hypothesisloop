package gitrefs

import "testing"

func TestAnAgentsNameIsOneSegmentOfABranch(t *testing.T) {
	for name, want := range map[string]string{"Researcher 2": "researcher-2", "  Ada / Lovelace!! ": "ada-lovelace", "baseline_1": "baseline-1", "***": ""} {
		if got := AgentBranchName(name); got != want {
			t.Errorf("AgentBranchName(%q) = %q, want %q", name, got, want)
		}
	}
}
