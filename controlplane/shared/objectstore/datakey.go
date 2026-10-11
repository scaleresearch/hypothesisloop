package objectstore

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// NewDataKey draws a fresh key for a job or an agent. A job's is created in the store once at
// submission, an agent's on its first request; either is stored and never rotated, so a
// restarted pod or agent starts with the same working key. An agent's is deleted when the agent is
// removed from its platform experiment. The store bounds access keys to 3..20 characters and secrets to
// 8..40; 12 and 25 random bytes encode to exactly 20 and 40.
func NewDataKey() (domain.DataKey, error) {
	access := make([]byte, 12)
	secret := make([]byte, 25)
	if _, err := rand.Read(access); err != nil {
		return domain.DataKey{}, fmt.Errorf("objectstore: draw access key: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return domain.DataKey{}, fmt.Errorf("objectstore: draw secret key: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return domain.DataKey{AccessKeyID: enc.EncodeToString(access), SecretAccessKey: enc.EncodeToString(secret)}, nil
}

// KeyIssuer registers the data keys jobs and agents reach the store with, each limited by an IAM
// policy document, and takes them out again. How is the store's own: one sub-package per provider.
type KeyIssuer interface {
	CreateKey(ctx context.Context, key domain.DataKey, policy []byte, description string) error
	DeleteKey(ctx context.Context, accessKeyID string) error
}

// CreateDataKey registers key with the store, limited by JobPolicy. The store refuses an access
// key that already exists, so a key is never silently redefined.
func (c *Client) CreateDataKey(ctx context.Context, key domain.DataKey, platformExperimentID, agentID, experimentID string) error {
	policy, err := JobPolicy(c.Bucket, platformExperimentID, agentID, experimentID)
	if err != nil {
		return err
	}
	return c.createKey(ctx, key, policy, "job "+JobPrefix(platformExperimentID, agentID, experimentID))
}

// CreateAgentDataKey is CreateDataKey for an agent's own key, limited by AgentPolicy.
func (c *Client) CreateAgentDataKey(ctx context.Context, key domain.DataKey, platformExperimentID, agentID string) error {
	policy, err := AgentPolicy(c.Bucket, platformExperimentID, agentID)
	if err != nil {
		return err
	}
	return c.createKey(ctx, key, policy, "agent "+AgentPrefix(platformExperimentID, agentID))
}

// DeleteDataKey takes a key CreateDataKey or CreateAgentDataKey registered out of the store, after
// which it opens nothing. A key the store no longer has is deleted already, so a removal whose
// first attempt lost the store's answer can be repeated.
func (c *Client) DeleteDataKey(ctx context.Context, accessKeyID string) error {
	return c.keys.DeleteKey(ctx, accessKeyID)
}

func (c *Client) createKey(ctx context.Context, key domain.DataKey, policy Policy, description string) error {
	document, err := policy.JSON()
	if err != nil {
		return err
	}
	return c.keys.CreateKey(ctx, key, []byte(document), description)
}
