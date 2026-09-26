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

// identityProvider is one configured issuer, parsed once at startup.
type identityProvider struct {
	audience string
	keys     map[string]verifyingKey
}

// parseIdentityProviders keys the configured providers by issuer. Two providers may use the same
// kid: a key is only ever looked up within the provider a token's issuer names.
func parseIdentityProviders(configured []config.IdentityProvider) (map[string]identityProvider, error) {
	providers := make(map[string]identityProvider, len(configured))
	for _, c := range configured {
		if c.Issuer == "" || c.Audience == "" || len(c.JWTKeys) == 0 {
			return nil, fmt.Errorf("identity provider %q needs an issuer, an audience and at least one key", c.Issuer)
		}
		if _, dup := providers[c.Issuer]; dup {
			return nil, fmt.Errorf("identity provider %s is listed twice", c.Issuer)
		}
		keys := make(map[string]verifyingKey, len(c.JWTKeys))
		for _, k := range c.JWTKeys {
			parsed, err := parseJWK(k)
			if err != nil {
				return nil, fmt.Errorf("identity provider %s: %w", c.Issuer, err)
			}
			if _, dup := keys[k.Kid]; dup {
				return nil, fmt.Errorf("identity provider %s: jwt key %s is listed twice", c.Issuer, k.Kid)
			}
			keys[k.Kid] = parsed
		}
		providers[c.Issuer] = identityProvider{audience: c.Audience, keys: keys}
	}
	return providers, nil
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

// verifyJWT finds the configured identity provider the token's issuer claim names, checks the
// signature against that provider's key the header names, then that provider's audience and the
// validity window, and returns the claims. The issuer is read before the signature is checked
// only to pick whose keys may vouch for the token: a token claiming one provider and signed by
// another's key is refused. An anonymous session is refused: an account is a person the identity
// provider knows.
func verifyJWT(token string, providers map[string]identityProvider, now time.Time) (jwtClaims, error) {
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
	var claims jwtClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return jwtClaims{}, errors.New("malformed token claims")
	}
	provider, ok := providers[claims.Issuer]
	if !ok {
		return jwtClaims{}, errors.New("token is from an issuer that is not configured")
	}
	key, ok := provider.keys[header.Kid]
	if !ok {
		return jwtClaims{}, errors.New("token is signed by a key its issuer does not list")
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
	switch {
	case !slices.Contains(claims.Audience, provider.audience):
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
