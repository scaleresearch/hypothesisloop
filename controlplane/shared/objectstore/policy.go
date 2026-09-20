package objectstore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PolicyStatement is one statement of an IAM policy. Exported so the policy a job is
// actually granted can be asserted on directly, rather than inferred from a live store's
// behaviour after the fact.
type PolicyStatement struct {
	Sid       string         `json:"Sid"`
	Effect    string         `json:"Effect"`
	Action    []string       `json:"Action"`
	Resource  []string       `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

// Policy is an IAM policy document.
type Policy struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

// Allows reports whether the policy grants action on resource. IAM's rule is that anything not
// explicitly allowed is denied, so this is the whole evaluation: the write-scoping guarantee is
// "no statement allows a write outside the job's own prefix", not "a Deny statement forbids it".
func (p Policy) Allows(action, resource string) bool {
	for _, st := range p.Statement {
		if st.Effect != "Allow" {
			continue
		}
		if matchesAny(st.Action, action) && matchesAny(st.Resource, resource) {
			return true
		}
	}
	return false
}

// matchesAny applies IAM's only wildcard, a trailing "*", to each pattern.
func matchesAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if pattern == value {
			return true
		}
		if n := len(pattern); n > 0 && pattern[n-1] == '*' && len(value) >= n-1 && value[:n-1] == pattern[:n-1] {
			return true
		}
	}
	return false
}

// JobPolicy is the grant one job runs under: write confined to its own prefix, read spanning
// the platform experiment it belongs to. This is what makes "any agent can load the checkpoint
// behind any claim, and no agent can overwrite another's evidence" a property of the store rather
// than a convention jobs are asked to observe — a job holds credentials that cannot write
// anywhere but its own prefix, whatever code it runs.
//
// Nothing grants s3:DeleteObject outside the job's prefix either, so a job cannot erase the
// evidence behind a rival's claim any more than it can rewrite it.
// The ids are refused rather than escaped if they carry anything IAM would read as a pattern.
// IAM expands '*' and '?' ANYWHERE in a Resource or StringLike value, not only at the end, so an
// agent id of "agent-*" would produce the write resource "<pe>/agent-*/<job>/*" and grant that job
// write access to every agent's prefix in the platform experiment. Registration already rejects
// such an id (domain.ValidateIdentifier), and this is the second lock on the same door: this
// function decides what a job may touch, so it must not be reachable with input that changes the
// meaning of what it emits, whatever a future caller does upstream.
func JobPolicy(bucket, platformExperimentID, agentID, experimentID string) (Policy, error) {
	return prefixPolicy(bucket, platformExperimentID, JobPrefix(platformExperimentID, agentID, experimentID),
		policyID{"agent_id", agentID}, policyID{"experiment_id", experimentID})
}

// AgentPolicy is the grant an agent holds outside its jobs: the same read span, and write
// confined to the prefix its own jobs write under.
func AgentPolicy(bucket, platformExperimentID, agentID string) (Policy, error) {
	return prefixPolicy(bucket, platformExperimentID, AgentPrefix(platformExperimentID, agentID),
		policyID{"agent_id", agentID})
}

type policyID struct{ kind, value string }

// prefixPolicy grants list and read across the platform experiment and write under writePrefix.
func prefixPolicy(bucket, platformExperimentID, writePrefix string, ids ...policyID) (Policy, error) {
	ids = append([]policyID{{"bucket", bucket}, {"platform_experiment_id", platformExperimentID}}, ids...)
	for _, part := range ids {
		if strings.ContainsAny(part.value, "*?") {
			return Policy{}, fmt.Errorf(
				"objectstore: refusing to build a data policy from %s %q: IAM reads '*' and '?' as wildcards, so this would widen what the key may reach",
				part.kind, part.value)
		}
		if part.value == "" {
			return Policy{}, fmt.Errorf("objectstore: data policy needs a non-empty %s", part.kind)
		}
	}
	bucketARN := "arn:aws:s3:::" + bucket
	return Policy{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Sid:      "ListWithinPlatformExperiment",
				Effect:   "Allow",
				Action:   []string{"s3:ListBucket"},
				Resource: []string{bucketARN},
				Condition: map[string]any{
					"StringLike": map[string]any{
						"s3:prefix": []string{PlatformExperimentPrefix(platformExperimentID) + "*"},
					},
				},
			},
			{
				Sid:      "ReadAcrossPlatformExperiment",
				Effect:   "Allow",
				Action:   []string{"s3:GetObject"},
				Resource: []string{bucketARN + "/" + PlatformExperimentPrefix(platformExperimentID) + "*"},
			},
			{
				Sid:      "WriteOwnPrefixOnly",
				Effect:   "Allow",
				Action:   []string{"s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"},
				Resource: []string{bucketARN + "/" + writePrefix + "*"},
			},
		},
	}, nil
}

// JSON renders the policy as the compact document the store takes as a key's inline policy.
func (p Policy) JSON() (string, error) {
	buf, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("objectstore: encode data policy: %w", err)
	}
	return string(buf), nil
}
