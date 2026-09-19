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
func TestSessionPolicyGrantsWriteOnlyWithinTheJobsOwnPrefix(t *testing.T) {
	policy := mustSessionPolicy(t)

	own := "arn:aws:s3:::hl-data/pe-1/agent-a/exp-7/checkpoint.pt"
	if !policy.Allows("s3:PutObject", own) {
		t.Fatalf("policy denies s3:PutObject on %s — a job that cannot write its own prefix has nowhere to leave a checkpoint", own)
	}
}

// The neighbouring cases are the guarantee itself: another agent's prefix, another job of the
// same agent, and the platform experiment's root all have to be unwritable. IAM denies anything
// no statement allows, so a policy that grants a write here is a policy that leaks one.
func TestSessionPolicyRefusesWriteOutsideTheJobsOwnPrefix(t *testing.T) {
	policy := mustSessionPolicy(t)

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
func TestSessionPolicyGrantsReadAcrossThePlatformExperiment(t *testing.T) {
	policy := mustSessionPolicy(t)

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
func TestSessionPolicyScopesListingToThePlatformExperimentPrefix(t *testing.T) {
	document, err := mustSessionPolicy(t).JSON()
	if err != nil {
		t.Fatal(err)
	}
	var reparsed Policy
	if err := json.Unmarshal([]byte(document), &reparsed); err != nil {
		t.Fatalf("session policy does not round-trip as JSON, so STS cannot read it: %v", err)
	}
	if !strings.Contains(document, `"s3:prefix"`) || !strings.Contains(document, `"pe-1/*"`) {
		t.Fatalf("listing grant carries no s3:prefix condition scoping it to pe-1/: %s", document)
	}
}

func mustSessionPolicy(t *testing.T) Policy {
	t.Helper()
	policy, err := SessionPolicy(testBucket, testPE, testAgent, testJob)
	if err != nil {
		t.Fatalf("SessionPolicy: %v", err)
	}
	return policy
}

// IAM expands '*' and '?' anywhere in a Resource or StringLike value, not only at the end. An
// agent registering as "agent-*" would otherwise be handed the write resource
// "<pe>/agent-*/<job>/*" and could write into every other agent's prefix in the platform
// experiment -- a privilege escalation that reads, in the generated document, as a perfectly
// ordinary policy. Registration rejects such an id; this is the second lock on the same door,
// because this function is what decides what a job may touch.
func TestSessionPolicyRefusesAnIdentifierCarryingAnIAMWildcard(t *testing.T) {
	for _, bad := range []struct{ what, pe, agent, job string }{
		{"agent id", testPE, "agent-*", testJob},
		{"job id", testPE, testAgent, "job-?"},
		{"platform experiment id", "pe-*", testAgent, testJob},
	} {
		if _, err := SessionPolicy(testBucket, bad.pe, bad.agent, bad.job); err == nil {
			t.Errorf("SessionPolicy accepted a wildcard in the %s — the generated policy would grant more than the job's own prefix", bad.what)
		}
	}
}

// An empty segment collapses the prefix, so "<pe>//<job>/*" would not name the job's prefix at
// all. Refusing beats emitting a policy whose scope nobody can read off it.
func TestSessionPolicyRefusesAnEmptyIdentifier(t *testing.T) {
	if _, err := SessionPolicy(testBucket, testPE, "", testJob); err == nil {
		t.Error("SessionPolicy accepted an empty agent id")
	}
}
