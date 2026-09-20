package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

// verifyingKey is a JWK parsed once at startup.
type verifyingKey struct {
	alg    string
	public crypto.PublicKey
}

func parseJWK(k config.JWTKey) (verifyingKey, error) {
	if k.Kid == "" {
		return verifyingKey{}, errors.New("jwt key has no kid")
	}
	switch {
	case k.Kty == "EC" && k.Crv == "P-256" && k.Alg == "ES256":
		x, err := bigInt(k.X)
		if err != nil {
			return verifyingKey{}, fmt.Errorf("jwt key %s: x: %w", k.Kid, err)
		}
		y, err := bigInt(k.Y)
		if err != nil {
			return verifyingKey{}, fmt.Errorf("jwt key %s: y: %w", k.Kid, err)
		}
		if !elliptic.P256().IsOnCurve(x, y) { //nolint:staticcheck // validating a configured point
			return verifyingKey{}, fmt.Errorf("jwt key %s: point is not on P-256", k.Kid)
		}
		return verifyingKey{alg: k.Alg, public: &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}}, nil
	case k.Kty == "RSA" && k.Alg == "RS256":
		n, err := bigInt(k.N)
		if err != nil {
			return verifyingKey{}, fmt.Errorf("jwt key %s: n: %w", k.Kid, err)
		}
		e, err := bigInt(k.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return verifyingKey{}, fmt.Errorf("jwt key %s: invalid exponent", k.Kid)
		}
		if n.BitLen() < 2048 {
			return verifyingKey{}, fmt.Errorf("jwt key %s: RSA modulus is shorter than 2048 bits", k.Kid)
		}
		return verifyingKey{alg: k.Alg, public: &rsa.PublicKey{N: n, E: int(e.Int64())}}, nil
	default:
		return verifyingKey{}, fmt.Errorf("jwt key %s: only ES256 (EC P-256) and RS256 (RSA) keys are supported, got kty=%q crv=%q alg=%q", k.Kid, k.Kty, k.Crv, k.Alg)
	}
}

func bigInt(b64 string) (*big.Int, error) {
	raw, err := encoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("not base64url")
	}
	return new(big.Int).SetBytes(raw), nil
}

// audiences is the aud claim, which the JWT standard lets be one string or a list.
type audiences []string

func (a *audiences) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		*a = audiences{one}
		return nil
	}
	return json.Unmarshal(raw, (*[]string)(a))
}

type jwtClaims struct {
	Issuer    string    `json:"iss"`
	Subject   string    `json:"sub"`
	Audience  audiences `json:"aud"`
	Expiry    float64   `json:"exp"`
	NotBefore float64   `json:"nbf"`
	// Name is the OIDC standard claim. Supabase states the same fact under user_metadata.
	Name         string `json:"name"`
	UserMetadata struct {
		Name string `json:"name"`
	} `json:"user_metadata"`
	// IsAnonymous is Supabase's mark on a session nobody signed in to.
	IsAnonymous bool `json:"is_anonymous"`
}

func (c jwtClaims) displayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.UserMetadata.Name
}

// verifyJWT checks a JWT's signature against the configured key its header names, then its
// issuer, audience and validity window, and returns its claims. An anonymous session is refused:
// an account is a person the identity provider knows.
func verifyJWT(token, issuer, audience string, keys map[string]verifyingKey, now time.Time) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return jwtClaims{}, errors.New("malformed token header")
	}
	key, ok := keys[header.Kid]
	if !ok {
		return jwtClaims{}, errors.New("token is signed by a key that is not configured")
	}
	if header.Alg != key.alg {
		return jwtClaims{}, errors.New("token algorithm does not match its key")
	}
	signature, err := encoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, errors.New("malformed token signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch public := key.public.(type) {
	case *ecdsa.PublicKey:
		if len(signature) != 64 {
			return jwtClaims{}, errors.New("bad signature")
		}
		r, s := new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])
		if !ecdsa.Verify(public, digest[:], r, s) {
			return jwtClaims{}, errors.New("bad signature")
		}
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) != nil {
			return jwtClaims{}, errors.New("bad signature")
		}
	}
	var claims jwtClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return jwtClaims{}, errors.New("malformed token claims")
	}
	switch {
	case claims.Issuer != issuer:
		return jwtClaims{}, errors.New("token is from another issuer")
	case !slices.Contains(claims.Audience, audience):
		return jwtClaims{}, errors.New("token is for another audience")
	case claims.Expiry == 0 || !now.Before(time.Unix(int64(claims.Expiry), 0)):
		return jwtClaims{}, errors.New("token has expired")
	case claims.NotBefore != 0 && now.Before(time.Unix(int64(claims.NotBefore), 0)):
		return jwtClaims{}, errors.New("token is not valid yet")
	case claims.Subject == "":
		return jwtClaims{}, errors.New("token has no subject")
	case claims.IsAnonymous:
		return jwtClaims{}, errors.New("anonymous sessions have no account: sign in")
	}
	return claims, nil
}

func decodeSegment(segment string, into any) error {
	raw, err := encoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
