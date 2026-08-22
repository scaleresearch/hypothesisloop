package objectstore

import (
	"encoding/json"
	"fmt"
)

// PolicyStatement is one statement of an IAM session policy. Exported so the policy a job is
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

// SessionPolicy is the grant one job runs under: write confined to its own prefix, read spanning
// the platform experiment it belongs to. This is what makes "any agent can load the checkpoint
// behind any claim, and no agent can overwrite another's evidence" a property of the store rather
// than a convention jobs are asked to observe — a job holds credentials that cannot write
// anywhere but its own prefix, whatever code it runs.
//
// Nothing grants s3:DeleteObject outside the job's prefix either, so a job cannot erase the
// evidence behind a rival's claim any more than it can rewrite it.
func SessionPolicy(bucket, platformExperimentID, agentID, experimentID string) Policy {
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
				Resource: []string{bucketARN + "/" + JobPrefix(platformExperimentID, agentID, experimentID) + "*"},
			},
		},
	}
}

// JSON renders the policy as the compact document STS takes as its session policy.
func (p Policy) JSON() (string, error) {
	buf, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("objectstore: encode session policy: %w", err)
	}
	return string(buf), nil
}
