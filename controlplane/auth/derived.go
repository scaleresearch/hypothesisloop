package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
)

var encoding = base64.RawURLEncoding

// Mint derives the token for a scope (scope.Token). Nothing is stored, so minting the same scope
// again returns the same token.
func Mint(signingSecret, s string) (string, error) {
	if signingSecret == "" {
		return "", errors.New("auth: signing secret is empty")
	}
	if _, err := principalOf(s); err != nil {
		return "", err
	}
	return scope.Token(s, mac(signingSecret, s)), nil
}

func mac(signingSecret, s string) []byte {
	h := hmac.New(sha256.New, []byte(signingSecret))
	h.Write([]byte(s))
	return h.Sum(nil)
}

// verifyDerived returns the principal an hls_ token names. It checks the signature only: whether
// the thing named still exists is the authenticator's question.
func verifyDerived(signingSecret, token string) (Principal, error) {
	s, err := scope.Of(token)
	if err != nil {
		return Principal{}, err
	}
	if !hmac.Equal([]byte(token), []byte(scope.Token(s, mac(signingSecret, s)))) {
		return Principal{}, errors.New("bad signature")
	}
	return principalOf(s)
}

func principalOf(s string) (Principal, error) {
	named, err := scope.Parse(s)
	if err != nil {
		return Principal{}, errors.New("auth: " + err.Error())
	}
	return Principal{
		Kind:                 Kind(named.Kind),
		ExperimentID:         named.ExperimentID,
		PlatformExperimentID: named.PlatformExperimentID,
		AgentID:              named.AgentID,
		ClusterName:          named.ClusterName,
		ClusterRegisteredAt:  named.ClusterRegisteredAt,
	}, nil
}
