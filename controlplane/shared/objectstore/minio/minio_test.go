package minio

import (
	"bytes"
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

// plainStore sends each request to url unsigned: the signature is objectstore's.
type plainStore struct{ url string }

func (s plainStore) SignedRequest(ctx context.Context, method, path string, query map[string]string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	return req, nil
}

func (plainStore) Do(req *http.Request) (*http.Response, error) { return http.DefaultClient.Do(req) }

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

func TestCreateKeySendsTheKeyAndItsPolicyEncryptedToTheAdminAPI(t *testing.T) {
	var got addServiceAccountRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != addServiceAccountPath {
			t.Errorf("request %s %s, want PUT %s", r.Method, r.URL.Path, addServiceAccountPath)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(decryptAdminBody(t, "root-secret", body), &got); err != nil {
			t.Errorf("decrypted body is not JSON: %v", err)
		}
	}))
	defer srv.Close()
	policy := `{"Version":"2012-10-17","Statement":[]}`
	err := New(plainStore{srv.URL}, "root-secret").CreateKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, []byte(policy), "job pe/a/j/")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if got.AccessKey != "AK" || got.SecretKey != "SK" || got.Description != "job pe/a/j/" || string(got.Policy) != policy {
		t.Fatalf("store was asked for %+v", got)
	}
}

// A refusal must reach the caller as an error: a key the store never created would be stored with
// the job and hand it credentials that work nowhere.
func TestCreateKeyReportsAStoreRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"Code":"XMinioIAMServiceAccountNotAllowed"}`)
	}))
	defer srv.Close()
	err := New(plainStore{srv.URL}, "root-secret").CreateKey(context.Background(), domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, []byte(`{}`), "job")
	if err == nil || !strings.Contains(err.Error(), "XMinioIAMServiceAccountNotAllowed") {
		t.Fatalf("CreateKey = %v, want the store's refusal", err)
	}
}

func TestDeleteKeyTakesAKeyTheStoreNoLongerHasAsDeleted(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path + " " + r.URL.Query().Get("accessKey")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := New(plainStore{srv.URL}, "root-secret").DeleteKey(context.Background(), "AK"); err != nil {
		t.Fatalf("DeleteKey of a key the store does not have: %v", err)
	}
	if asked != "DELETE "+deleteServiceAccountPath+" AK" {
		t.Fatalf("store was asked %q", asked)
	}
}
