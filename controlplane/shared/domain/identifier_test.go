package domain

import "testing"

// The characters this rejects are not cosmetic. An id reaches two places that interpret it as a
// pattern rather than a name: Kubernetes object names, and the object-store session policy that
// decides what a job may write. IAM expands '*' and '?' anywhere in a policy value, so an agent
// registered as "agent-*" would hold credentials able to write into every other agent's prefix.
func TestValidateIdentifierRejectsCharactersThatWidenAnAccessPolicy(t *testing.T) {
	for _, bad := range []string{
		"agent-*",     // IAM wildcard: matches every agent whose id shares the prefix
		"agent-?",     // IAM single-character wildcard
		"agent/other", // changes the prefix's segment structure
		"../escape",   // path traversal in anything that normalizes paths
		"Agent-One",   // uppercase: not a legal DNS label, so not a legal k8s object name
		"-leading",
		"trailing-",
		"",
	} {
		if err := ValidateIdentifier("agent_id", bad); err == nil {
			t.Errorf("ValidateIdentifier accepted %q", bad)
		}
	}
}

func TestValidateIdentifierAcceptsTheIdsTheSuiteAndAgentsActuallyUse(t *testing.T) {
	for _, good := range []string{
		"agent-dist-depth-1786534808",
		"job-d2dfbf80-1786534808-4004470",
		"pe-1b62dccc",
		"a",
		"agent1",
	} {
		if err := ValidateIdentifier("agent_id", good); err != nil {
			t.Errorf("ValidateIdentifier rejected %q: %v", good, err)
		}
	}
}

// Bounded at the DNS label limit, because a longer id cannot become a Kubernetes object name.
func TestValidateIdentifierRejectsAnIdTooLongToNameAKubernetesObject(t *testing.T) {
	long := ""
	for len(long) <= MaxIdentifierLength {
		long += "a"
	}
	if err := ValidateIdentifier("agent_id", long); err == nil {
		t.Errorf("ValidateIdentifier accepted a %d-character id", len(long))
	}
}
