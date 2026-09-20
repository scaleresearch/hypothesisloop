package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	apiKeyPrefix = "hlk_"
	invitePrefix = "hli_"
)

// NewAPIKey generates an API key: its id, which names it in listings and revocation, and the
// token hlk_<id>_<secret>, which is shown once and stored only as HashAPIKey of it.
func NewAPIKey() (id, token string, err error) {
	return newSecret(apiKeyPrefix)
}

// NewInvite generates an invite to an org: its id and the token hli_<id>_<secret>, shown once
// and stored only as HashAPIKey of it, as an API key is.
func NewInvite() (id, token string, err error) {
	return newSecret(invitePrefix)
}

func newSecret(prefix string) (id, token string, err error) {
	buf := make([]byte, 6+32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate %s token: %w", prefix, err)
	}
	id = hex.EncodeToString(buf[:6])
	return id, prefix + id + "_" + encoding.EncodeToString(buf[6:]), nil
}

// HashAPIKey is the form an API key token, and an invite token, is stored and looked up under.
func HashAPIKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
