package objectstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

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

// decryptAdminBody is the server side of madmin's format for algorithm 0x02 with one sio fragment,
// written from the format description independently of encryptAdminBody.
func decryptAdminBody(t *testing.T, password string, data []byte) []byte {
	t.Helper()
	if len(data) < 41 || data[32] != adminBodyPBKDF2AESGCM {
		t.Fatalf("body is not madmin algorithm 0x02: % x", data[:min(len(data), 41)])
	}
	key, err := pbkdf2.Key(sha256.New, password, data[:32], 8192, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	copy(nonce, data[33:41])
	tag := aead.Seal(nil, nonce, nil, nil)
	binary.LittleEndian.PutUint32(nonce[8:], 1)
	plain, err := aead.Open(nil, nonce, data[41:], append([]byte{0x80}, tag...))
	if err != nil {
		t.Fatalf("body does not open as one final sio fragment: %v", err)
	}
	return plain
}

func TestCreateDataKeySendsTheKeyAndItsPolicyEncryptedToTheAdminAPI(t *testing.T) {
	var got addServiceAccountRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != addServiceAccountPath {
			t.Errorf("request %s %s, want PUT %s", r.Method, r.URL.Path, addServiceAccountPath)
		}
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential=root/") || !strings.Contains(auth, "/s3/aws4_request") {
			t.Errorf("Authorization %q is not SigV4 in the s3 scope with the control plane's key", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decryptAdminBody(t, "root-secret", body), &got); err != nil {
			t.Errorf("decrypted body is not JSON: %v", err)
		}
	}))
	defer srv.Close()
	client, err := New(srv.URL, srv.URL, "us-east-1", testBucket, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	key := domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}
	if err := client.CreateDataKey(context.Background(), key, testPE, nil, testAgent, testJob); err != nil {
		t.Fatalf("CreateDataKey: %v", err)
	}
	if got.AccessKey != "AK" || got.SecretKey != "SK" {
		t.Fatalf("store was asked for key %q/%q, want the one stored with the job", got.AccessKey, got.SecretKey)
	}
	var policy Policy
	if err := json.Unmarshal(got.Policy, &policy); err != nil {
		t.Fatalf("policy is not a JSON document: %v", err)
	}
	if !allows(policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-a/exp-7/x") ||
		allows(policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-b/exp-9/x") {
		t.Fatalf("key policy is not the job's write scope: %s", got.Policy)
	}
}

func TestCreateAgentDataKeyAsksForTheAgentsWriteScope(t *testing.T) {
	var got addServiceAccountRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decryptAdminBody(t, "root-secret", body), &got); err != nil {
			t.Errorf("decrypted body is not JSON: %v", err)
		}
	}))
	defer srv.Close()
	client, err := New(srv.URL, srv.URL, "us-east-1", testBucket, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateAgentDataKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, testPE, nil, testAgent); err != nil {
		t.Fatalf("CreateAgentDataKey: %v", err)
	}
	if got.AccessKey != "AK" || got.SecretKey != "SK" || got.Description != "agent pe-1/agent-a/" {
		t.Fatalf("store was asked for %q/%q %q", got.AccessKey, got.SecretKey, got.Description)
	}
	var policy Policy
	if err := json.Unmarshal(got.Policy, &policy); err != nil {
		t.Fatalf("policy is not a JSON document: %v", err)
	}
	if !allows(policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-a/x") ||
		allows(policy, "s3:PutObject", "arn:aws:s3:::hl-data/pe-1/agent-b/x") {
		t.Fatalf("key policy is not the agent's write scope: %s", got.Policy)
	}
}

// A refusal must reach the submitter as an error: a key the store never created would be stored
// with the job and hand it credentials that work nowhere.
func TestCreateDataKeyReportsAStoreRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"Code":"XMinioIAMServiceAccountNotAllowed"}`)
	}))
	defer srv.Close()
	client, err := New(srv.URL, srv.URL, "us-east-1", testBucket, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	err = client.CreateDataKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, testPE, nil, testAgent, testJob)
	if err == nil || !strings.Contains(err.Error(), "XMinioIAMServiceAccountNotAllowed") {
		t.Fatalf("CreateDataKey = %v, want the store's refusal", err)
	}
}
