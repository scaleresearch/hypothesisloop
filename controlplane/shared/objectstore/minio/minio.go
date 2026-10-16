package minio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	addServiceAccountPath    = "/minio/admin/v3/add-service-account"
	deleteServiceAccountPath = "/minio/admin/v3/delete-service-account"
)

// Store sends the admin API's requests: signed as the control plane's key, to where the control
// plane reaches the store (objectstore.Client).
type Store interface {
	SignedRequest(ctx context.Context, method, path string, query map[string]string, body []byte) (*http.Request, error)
	Do(req *http.Request) (*http.Response, error)
}

// Keys creates and deletes data keys as service accounts of the key store signs with, whose
// secret is secretAccessKey.
type Keys struct {
	store           Store
	secretAccessKey string
}

// New is the Keys of store, signing as the key whose secret is secretAccessKey. That key must be
// allowed to create service accounts; the store's root user is.
func New(store Store, secretAccessKey string) *Keys {
	return &Keys{store: store, secretAccessKey: secretAccessKey}
}

type addServiceAccountRequest struct {
	Policy      json.RawMessage `json:"policy"`
	AccessKey   string          `json:"accessKey"`
	SecretKey   string          `json:"secretKey"`
	Description string          `json:"description"`
}

// CreateKey registers key as a service account limited by policy, an IAM policy document. The
// store refuses an access key that already exists, so a key is never silently redefined.
func (k *Keys) CreateKey(ctx context.Context, key domain.DataKey, policy []byte, description string) error {
	body, err := json.Marshal(addServiceAccountRequest{
		Policy: json.RawMessage(policy), AccessKey: key.AccessKeyID, SecretKey: key.SecretAccessKey,
		Description: description,
	})
	if err != nil {
		return fmt.Errorf("objectstore minio: encode data key request: %w", err)
	}
	encrypted, err := encryptAdminBody(k.secretAccessKey, body)
	if err != nil {
		return err
	}
	req, err := k.store.SignedRequest(ctx, http.MethodPut, addServiceAccountPath, nil, encrypted)
	if err != nil {
		return err
	}
	resp, err := k.store.Do(req)
	if err != nil {
		return fmt.Errorf("objectstore minio: create data key for %s: %w", description, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("objectstore minio: create data key for %s: status %d: %s", description, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// DeleteKey takes a key CreateKey registered out of the store. A key the store no longer has is
// deleted already, so a removal whose first attempt lost the store's answer can be repeated.
func (k *Keys) DeleteKey(ctx context.Context, accessKeyID string) error {
	req, err := k.store.SignedRequest(ctx, http.MethodDelete, deleteServiceAccountPath, map[string]string{"accessKey": accessKeyID}, nil)
	if err != nil {
		return err
	}
	resp, err := k.store.Do(req)
	if err != nil {
		return fmt.Errorf("objectstore minio: delete data key %s: %w", accessKeyID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("objectstore minio: delete data key %s: status %d: %s", accessKeyID, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// adminBodyPBKDF2AESGCM is madmin's algorithm id for a PBKDF2-SHA256 key with AES-256-GCM, which
// the server decrypts in every mode. It saves vendoring madmin-go and its dependency tree.
const (
	adminBodyPBKDF2AESGCM = 0x02
	adminBodyPBKDF2Rounds = 8192
)

// encryptAdminBody produces madmin's EncryptData format: salt(32) | algorithm id(1) | nonce(8) |
// one final sio fragment. sio derives the fragment's associated data by sealing nothing under
// sequence number 0, then seals the payload under sequence number 1 with the final flag 0x80.
// One fragment holds up to 16 KiB, far above any request body here.
func encryptAdminBody(password string, plaintext []byte) ([]byte, error) {
	if len(plaintext) > 1<<14 {
		return nil, fmt.Errorf("objectstore minio: admin request body of %d bytes exceeds one encryption fragment", len(plaintext))
	}
	header := make([]byte, 32+1+8)
	if _, err := rand.Read(header); err != nil {
		return nil, fmt.Errorf("objectstore minio: draw admin body salt: %w", err)
	}
	header[32] = adminBodyPBKDF2AESGCM
	key, err := pbkdf2.Key(sha256.New, password, header[:32], adminBodyPBKDF2Rounds, 32)
	if err != nil {
		return nil, fmt.Errorf("objectstore minio: derive admin body key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("objectstore minio: admin body cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("objectstore minio: admin body cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	copy(nonce, header[33:])
	associated := append([]byte{0x80}, aead.Seal(nil, nonce, nil, nil)...)
	binary.LittleEndian.PutUint32(nonce[8:], 1)
	return aead.Seal(header, nonce, plaintext, associated), nil
}
