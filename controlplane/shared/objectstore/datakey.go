package objectstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// NewDataKey draws a fresh key for a job or an agent. A job's is created in the store once at
// submission, an agent's on its first request; either is stored and never rotated or deleted, so a
// restarted pod or agent starts with the same working key. The store bounds access keys to 3..20 characters and secrets to
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

const addServiceAccountPath = "/minio/admin/v3/add-service-account"

type addServiceAccountRequest struct {
	Policy      json.RawMessage `json:"policy"`
	AccessKey   string          `json:"accessKey"`
	SecretKey   string          `json:"secretKey"`
	Description string          `json:"description"`
}

// CreateDataKey registers key with the store as a service account of the control plane's own key,
// limited by JobPolicy. This is the MinIO admin API (Silo, MinIO, AIStor). The store refuses an
// access key that already exists, so a key is never silently redefined.
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

func (c *Client) createKey(ctx context.Context, key domain.DataKey, policy Policy, description string) error {
	document, err := policy.JSON()
	if err != nil {
		return err
	}
	body, err := json.Marshal(addServiceAccountRequest{
		Policy: json.RawMessage(document), AccessKey: key.AccessKeyID, SecretKey: key.SecretAccessKey,
		Description: description,
	})
	if err != nil {
		return fmt.Errorf("objectstore: encode data key request: %w", err)
	}
	encrypted, err := encryptAdminBody(c.SecretAccessKey, body)
	if err != nil {
		return err
	}
	req, err := c.signedRequest(ctx, http.MethodPut, addServiceAccountPath, nil, encrypted)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("objectstore: create data key for %s: %w", description, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("objectstore: create data key for %s: status %d: %s", description, resp.StatusCode, strings.TrimSpace(string(msg)))
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
		return nil, fmt.Errorf("objectstore: admin request body of %d bytes exceeds one encryption fragment", len(plaintext))
	}
	header := make([]byte, 32+1+8)
	if _, err := rand.Read(header); err != nil {
		return nil, fmt.Errorf("objectstore: draw admin body salt: %w", err)
	}
	header[32] = adminBodyPBKDF2AESGCM
	key, err := pbkdf2.Key(sha256.New, password, header[:32], adminBodyPBKDF2Rounds, 32)
	if err != nil {
		return nil, fmt.Errorf("objectstore: derive admin body key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("objectstore: admin body cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("objectstore: admin body cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	copy(nonce, header[33:])
	associated := append([]byte{0x80}, aead.Seal(nil, nonce, nil, nil)...)
	binary.LittleEndian.PutUint32(nonce[8:], 1)
	return aead.Seal(header, nonce, plaintext, associated), nil
}
