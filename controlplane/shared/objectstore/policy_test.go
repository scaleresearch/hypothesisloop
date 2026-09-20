package objectstore

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testBucket = "hl-data"
	testPE     = "pe-1"
	testAgent  = "agent-a"
	testJob    = "exp-7"
)

// "No agent can overwrite another's evidence" is a stated guarantee of the platform, not advice.
// It holds only if the credentials a job physically holds cannot write outside its own prefix —
// so the grant is asserted here, on the document itself, rather than trusted to jobs behaving.
func TestJobPolicyGrantsWriteOnlyWithinTheJobsOwnPrefix(t *testing.T) {
	policy := mustJobPolicy(t)

	own := "arn:aws:s3:::hl-data/pe-1/agent-a/exp-7/checkpoint.pt"
	if !policy.Allows("s3:PutObject", own) {
		t.Fatalf("policy denies s3:PutObject on %s — a job that cannot write its own prefix has nowhere to leave a checkpoint", own)
	}
}

// The neighbouring cases are the guarantee itself: another agent's prefix, another job of the
// same agent, and the platform experiment's root all have to be unwritable. IAM denies anything
// no statement allows, so a policy that grants a write here is a policy that leaks one.
func TestJobPolicyRefusesWriteOutsideTheJobsOwnPrefix(t *testing.T) {
	policy := mustJobPolicy(t)

	for _, resource := range []string{
		"arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/checkpoint.pt",
		"arn:aws:s3:::hl-data/pe-1/agent-a/exp-8/checkpoint.pt",
		"arn:aws:s3:::hl-data/pe-1/rogue.pt",
		"arn:aws:s3:::hl-data/pe-2/agent-a/exp-7/checkpoint.pt",
	} {
		if policy.Allows("s3:PutObject", resource) {
			t.Fatalf("policy allows s3:PutObject on %s — one agent can overwrite another's evidence", resource)
		}
		if policy.Allows("s3:DeleteObject", resource) {
			t.Fatalf("policy allows s3:DeleteObject on %s — erasing a rival's evidence is the same failure as rewriting it", resource)
		}
	}
}

// Read spans the whole platform experiment on purpose: that is the shared-notebook model applied
// to bytes, and it is what lets a later stage load the checkpoint behind any claim, including one
// a different agent made.
func TestJobPolicyGrantsReadAcrossThePlatformExperiment(t *testing.T) {
	policy := mustJobPolicy(t)

	other := "arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/checkpoint.pt"
	if !policy.Allows("s3:GetObject", other) {
		t.Fatalf("policy denies s3:GetObject on %s — no agent could load the checkpoint behind another's claim", other)
	}
	if policy.Allows("s3:GetObject", "arn:aws:s3:::hl-data/pe-2/agent-a/exp-7/checkpoint.pt") {
		t.Fatal("policy allows reading another platform experiment — the shared prefix is one program's, not the whole bucket's")
	}
}

// The listing grant is the one place the platform-experiment scope lives in a condition rather
// than in the resource ARN, because s3:ListBucket is taken against the bucket itself. Losing the
// condition silently widens a job from listing its own program to listing every program's.
func TestJobPolicyScopesListingToThePlatformExperimentPrefix(t *testing.T) {
	document, err := mustJobPolicy(t).JSON()
	if err != nil {
		t.Fatal(err)
	}
	var reparsed Policy
	if err := json.Unmarshal([]byte(document), &reparsed); err != nil {
		t.Fatalf("job policy does not round-trip as JSON, so the store cannot read it: %v", err)
	}
	if !strings.Contains(document, `"s3:prefix"`) || !strings.Contains(document, `"pe-1/*"`) {
		t.Fatalf("listing grant carries no s3:prefix condition scoping it to pe-1/: %s", document)
	}
}

func mustJobPolicy(t *testing.T) Policy {
	t.Helper()
	policy, err := JobPolicy(testBucket, testPE, testAgent, testJob)
	if err != nil {
		t.Fatalf("JobPolicy: %v", err)
	}
	return policy
}

// IAM expands '*' and '?' anywhere in a Resource or StringLike value, not only at the end. An
// agent registering as "agent-*" would otherwise be handed the write resource
// "<pe>/agent-*/<job>/*" and could write into every other agent's prefix in the platform
// experiment -- a privilege escalation that reads, in the generated document, as a perfectly
// ordinary policy. Registration rejects such an id; this is the second lock on the same door,
// because this function is what decides what a job may touch.
func TestJobPolicyRefusesAnIdentifierCarryingAnIAMWildcard(t *testing.T) {
	for _, bad := range []struct{ what, pe, agent, job string }{
		{"agent id", testPE, "agent-*", testJob},
		{"job id", testPE, testAgent, "job-?"},
		{"platform experiment id", "pe-*", testAgent, testJob},
	} {
		if _, err := JobPolicy(testBucket, bad.pe, bad.agent, bad.job); err == nil {
			t.Errorf("JobPolicy accepted a wildcard in the %s — the generated policy would grant more than the job's own prefix", bad.what)
		}
	}
}

// An empty segment collapses the prefix, so "<pe>//<job>/*" would not name the job's prefix at
// all. Refusing beats emitting a policy whose scope nobody can read off it.
func TestJobPolicyRefusesAnEmptyIdentifier(t *testing.T) {
	if _, err := JobPolicy(testBucket, testPE, "", testJob); err == nil {
		t.Error("JobPolicy accepted an empty agent id")
	}
}

func TestAgentPolicyWritesTheAgentsPrefixAndReadsThePlatformExperiment(t *testing.T) {
	policy, err := AgentPolicy(testBucket, testPE, testAgent)
	if err != nil {
		t.Fatalf("AgentPolicy: %v", err)
	}
	for _, own := range []string{
		"arn:aws:s3:::hl-data/pe-1/agent-a/notes.md",
		"arn:aws:s3:::hl-data/pe-1/agent-a/exp-7/checkpoint.pt",
	} {
		for _, action := range []string{"s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"} {
			if !policy.Allows(action, own) {
				t.Errorf("policy denies %s on %s, inside the agent's own prefix", action, own)
			}
		}
	}
	for _, other := range []string{
		"arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/checkpoint.pt",
		"arn:aws:s3:::hl-data/pe-1/agent-ab/x",
		"arn:aws:s3:::hl-data/pe-1/rogue.pt",
		"arn:aws:s3:::hl-data/pe-2/agent-a/x",
	} {
		for _, action := range []string{"s3:PutObject", "s3:DeleteObject"} {
			if policy.Allows(action, other) {
				t.Errorf("policy allows %s on %s, outside the agent's own prefix", action, other)
			}
		}
	}
	if !policy.Allows("s3:GetObject", "arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/checkpoint.pt") {
		t.Error("policy denies reading another agent's object in the same platform experiment")
	}
	if policy.Allows("s3:GetObject", "arn:aws:s3:::hl-data/pe-2/agent-a/x") {
		t.Error("policy allows reading another platform experiment")
	}
	document, err := policy.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document, `"s3:prefix":["pe-1/*"]`) {
		t.Errorf("listing is not scoped to pe-1/: %s", document)
	}
}

func TestAgentPolicyRefusesAWildcardOrEmptyIdentifier(t *testing.T) {
	for _, bad := range []struct{ pe, agent string }{{testPE, "agent-*"}, {"pe-?", testAgent}, {testPE, ""}} {
		if _, err := AgentPolicy(testBucket, bad.pe, bad.agent); err == nil {
			t.Errorf("AgentPolicy accepted platform experiment %q, agent %q", bad.pe, bad.agent)
		}
	}
}
