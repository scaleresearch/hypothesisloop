package provider

import (
	"strings"
	"testing"
)

func TestASegmentIsShortLowercaseAndHoldsNoDash(t *testing.T) {
	for _, ok := range []string{"main", "a", "team7", strings.Repeat("x", 16)} {
		if err := ValidateSegment("namespace", ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "team-a", "Main", "a_b", strings.Repeat("x", 17)} {
		err := ValidateSegment("namespace", bad)
		if err == nil || !strings.Contains(err.Error(), "namespace") {
			t.Errorf("%q: err = %v, want a refusal naming the setting", bad, err)
		}
	}
}

func TestJobAndAgentInstancesOfOneNamespaceNeverShareAPrefix(t *testing.T) {
	job, agent := JobPrefix("main"), AgentPrefix("main")
	if job != "hl-job-main-" || agent != "hl-agent-main-" {
		t.Fatalf("prefixes = %q, %q", job, agent)
	}
	if strings.HasPrefix(job, agent) || strings.HasPrefix(agent, job) {
		t.Fatal("one prefix starts the other")
	}
	if strings.HasPrefix(AgentPrefix("main2"), AgentPrefix("main")) {
		t.Fatal("one namespace's prefix starts another's")
	}
	for _, prefix := range []string{job, agent} {
		if err := ValidateName(prefix + "x"); err != nil {
			t.Errorf("%q: %v", prefix, err)
		}
	}
}
