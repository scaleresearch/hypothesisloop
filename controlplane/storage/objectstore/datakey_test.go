package objectstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore/minio"
)

func minioKeys(c *Client) KeyIssuer { return minio.New(c, c.SecretAccessKey) }

func TestNewDataKeyFitsTheStoresLengthBoundsAndIsFreshEachTime(t *testing.T) {
	a, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.AccessKeyID) != 20 || len(a.SecretAccessKey) != 40 {
		t.Fatalf("key lengths %d/%d, want 20/40: the store refuses access keys over 20 and secrets over 40", len(a.AccessKeyID), len(a.SecretAccessKey))
	}
	if a.AccessKeyID == b.AccessKeyID || a.SecretAccessKey == b.SecretAccessKey {
		t.Fatal("two drawn keys are equal: jobs would share a key")
	}
}

// issued records what the client asks its provider to create and delete.
type issued struct {
	key         domain.DataKey
	policy      Policy
	description string
	deleted     string
	err         error
}

func (f *issued) CreateKey(_ context.Context, key domain.DataKey, policy []byte, description string) error {
	f.key, f.description = key, description
	if err := json.Unmarshal(policy, &f.policy); err != nil {
		return err
	}
	return f.err
}

func (f *issued) DeleteKey(_ context.Context, accessKeyID string) error {
	f.deleted = accessKeyID
	return f.err
}

func clientIssuing(t *testing.T, keys KeyIssuer) *Client {
	t.Helper()
	client, err := New("http://127.0.0.1:1", "http://127.0.0.1:1", "us-east-1", testBucket, "root", "root-secret", func(*Client) KeyIssuer { return keys })
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCreateDataKeyAsksTheProviderForTheJobsScope(t *testing.T) {
	keys := &issued{}
	key := domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}
	if err := clientIssuing(t, keys).CreateDataKey(context.Background(), key, testPE, testAgent, testJob); err != nil {
		t.Fatalf("CreateDataKey: %v", err)
	}
	if keys.key != key || keys.description != "job pe-1/agent-a/exp-7/" {
		t.Fatalf("provider was asked for %+v %q, want the job's key", keys.key, keys.description)
	}
	if !allows(keys.policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-a/exp-7/x") ||
		allows(keys.policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/x") {
		t.Fatalf("key policy is not the job's write scope: %+v", keys.policy)
	}
}

func TestCreateAgentDataKeyAsksTheProviderForTheAgentsScope(t *testing.T) {
	keys := &issued{}
	if err := clientIssuing(t, keys).CreateAgentDataKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, testPE, testAgent); err != nil {
		t.Fatalf("CreateAgentDataKey: %v", err)
	}
	if keys.description != "agent pe-1/agent-a/" {
		t.Fatalf("provider was asked for %q", keys.description)
	}
	if !allows(keys.policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-a/x") ||
		allows(keys.policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-b/x") {
		t.Fatalf("key policy is not the agent's write scope: %+v", keys.policy)
	}
}

// A refusal must reach the submitter as an error: a key the store never created would be stored
// with the job and hand it credentials that work nowhere.
func TestCreateDataKeyReportsTheProvidersRefusal(t *testing.T) {
	keys := &issued{err: errors.New("XMinioIAMServiceAccountNotAllowed")}
	err := clientIssuing(t, keys).CreateDataKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, testPE, testAgent, testJob)
	if err == nil || !strings.Contains(err.Error(), "XMinioIAMServiceAccountNotAllowed") {
		t.Fatalf("CreateDataKey = %v, want the store's refusal", err)
	}
}

func TestAStoreWithNoKeyIssuerIsRefused(t *testing.T) {
	if _, err := New("http://127.0.0.1:1", "http://127.0.0.1:1", "us-east-1", testBucket, "root", "root-secret", nil); err == nil {
		t.Fatal("New accepted a store with no key issuer")
	}
}
