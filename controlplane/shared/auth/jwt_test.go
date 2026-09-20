package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

const (
	testIssuer   = "https://issuer.example/auth/v1"
	testAudience = "authenticated"
)

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type testSigner struct {
	jwk  config.JWTKey
	sign func(signingInput string) []byte
}

func newECSigner(t *testing.T, kid string) testSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return testSigner{
		jwk: config.JWTKey{Kid: kid, Kty: "EC", Alg: "ES256", Crv: "P-256",
			X: encoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: encoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))},
		sign: func(input string) []byte {
			digest := sha256.Sum256([]byte(input))
			r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		},
	}
}

func newRSASigner(t *testing.T, kid string) testSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return testSigner{
		jwk: config.JWTKey{Kid: kid, Kty: "RSA", Alg: "RS256",
			N: encoding.EncodeToString(key.N.Bytes()), E: encoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())},
		sign: func(input string) []byte {
			digest := sha256.Sum256([]byte(input))
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return sig
		},
	}
}

func (s testSigner) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	return s.tokenWithHeader(t, map[string]any{"alg": s.jwk.Alg, "kid": s.jwk.Kid, "typ": "JWT"}, claims)
}

func (s testSigner) tokenWithHeader(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	input := segment(t, header) + "." + segment(t, claims)
	return input + "." + encoding.EncodeToString(s.sign(input))
}

func segment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoding.EncodeToString(raw)
}

func goodClaims() map[string]any {
	return map[string]any{"iss": testIssuer, "aud": testAudience, "sub": "user-1", "exp": testNow.Add(time.Hour).Unix(), "name": "Ada Lovelace"}
}

func keysOf(t *testing.T, signers ...testSigner) map[string]verifyingKey {
	t.Helper()
	keys := map[string]verifyingKey{}
	for _, s := range signers {
		parsed, err := parseJWK(s.jwk)
		if err != nil {
			t.Fatalf("parse jwk %s: %v", s.jwk.Kid, err)
		}
		keys[s.jwk.Kid] = parsed
	}
	return keys
}

func TestAJWTSignedByAConfiguredKeyVerifies(t *testing.T) {
	ec, rs := newECSigner(t, "ec-1"), newRSASigner(t, "rsa-1")
	keys := keysOf(t, ec, rs)
	for _, signer := range []testSigner{ec, rs} {
		claims, err := verifyJWT(signer.token(t, goodClaims()), testIssuer, testAudience, keys, testNow)
		if err != nil || claims.Subject != "user-1" || claims.displayName() != "Ada Lovelace" {
			t.Errorf("%s: got = %+v, %v, want user-1, Ada Lovelace", signer.jwk.Alg, claims, err)
		}
	}
}

func TestTheAudienceMayBeOneOfSeveralAndNotBeforeMayHavePassed(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	claims := goodClaims()
	claims["aud"] = []string{"another-service", testAudience}
	claims["nbf"] = testNow.Unix()
	if _, err := verifyJWT(ec.token(t, claims), testIssuer, testAudience, keysOf(t, ec), testNow); err != nil {
		t.Fatalf("got = %v, want it accepted", err)
	}
}

func TestTheNameIsReadWhereSupabasePutsItToo(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	claims := goodClaims()
	delete(claims, "name")
	claims["user_metadata"] = map[string]any{"name": "Grace Hopper"}
	got, err := verifyJWT(ec.token(t, claims), testIssuer, testAudience, keysOf(t, ec), testNow)
	if err != nil || got.displayName() != "Grace Hopper" {
		t.Fatalf("got = %q, %v, want Grace Hopper", got.displayName(), err)
	}
	delete(claims, "user_metadata")
	if got, err := verifyJWT(ec.token(t, claims), testIssuer, testAudience, keysOf(t, ec), testNow); err != nil || got.displayName() != "" {
		t.Fatalf("a token with no name: got = %q, %v, want it accepted with an empty name", got.displayName(), err)
	}
}

func TestJWTsThatMustBeRefused(t *testing.T) {
	configured, unlisted := newECSigner(t, "ec-1"), newECSigner(t, "ec-2")
	impostor := newECSigner(t, "ec-1")
	rs := newRSASigner(t, "rsa-1")
	keys := keysOf(t, configured, rs)

	with := func(change func(map[string]any)) map[string]any {
		claims := goodClaims()
		change(claims)
		return claims
	}
	good := configured.token(t, goodClaims())
	parts := strings.Split(good, ".")

	// The classic confusion: the public key is not secret, so an attacker signs with it as an
	// HMAC key and claims HS256.
	hmacInput := segment(t, map[string]any{"alg": "HS256", "kid": "rsa-1"}) + "." + segment(t, goodClaims())
	h := hmac.New(sha256.New, []byte(rs.jwk.N))
	h.Write([]byte(hmacInput))

	for name, token := range map[string]string{
		"signed by an unlisted key":          unlisted.token(t, goodClaims()),
		"another key under a configured kid": impostor.token(t, goodClaims()),
		"expired":                            configured.token(t, with(func(c map[string]any) { c["exp"] = testNow.Add(-time.Second).Unix() })),
		"expiring this instant":              configured.token(t, with(func(c map[string]any) { c["exp"] = testNow.Unix() })),
		"no expiry":                          configured.token(t, with(func(c map[string]any) { delete(c, "exp") })),
		"another issuer":                     configured.token(t, with(func(c map[string]any) { c["iss"] = "https://evil.example" })),
		"no issuer":                          configured.token(t, with(func(c map[string]any) { delete(c, "iss") })),
		"another audience":                   configured.token(t, with(func(c map[string]any) { c["aud"] = "another-service" })),
		"audiences that leave ours out":      configured.token(t, with(func(c map[string]any) { c["aud"] = []string{"a", "b"} })),
		"no audience":                        configured.token(t, with(func(c map[string]any) { delete(c, "aud") })),
		"not valid yet":                      configured.token(t, with(func(c map[string]any) { c["nbf"] = testNow.Add(time.Second).Unix() })),
		"anonymous session":                  configured.token(t, with(func(c map[string]any) { c["is_anonymous"] = true })),
		"no subject":                         configured.token(t, with(func(c map[string]any) { delete(c, "sub") })),
		"claims edited after signing":        parts[0] + "." + segment(t, with(func(c map[string]any) { c["sub"] = "user-2" })) + "." + parts[2],
		"alg none":                           segment(t, map[string]any{"alg": "none", "kid": "ec-1"}) + "." + parts[1] + ".",
		"alg none with the old signature":    segment(t, map[string]any{"alg": "none", "kid": "ec-1"}) + "." + parts[1] + "." + parts[2],
		"HS256 keyed with the public key":    hmacInput + "." + encoding.EncodeToString(h.Sum(nil)),
		"ES256 token naming the RSA key":     configured.tokenWithHeader(t, map[string]any{"alg": "ES256", "kid": "rsa-1"}, goodClaims()),
		"RS256 header on an EC key":          configured.tokenWithHeader(t, map[string]any{"alg": "RS256", "kid": "ec-1"}, goodClaims()),
		"no kid":                             configured.tokenWithHeader(t, map[string]any{"alg": "ES256"}, goodClaims()),
		"signature truncated":                parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4],
		"signature of zeros":                 parts[0] + "." + parts[1] + "." + encoding.EncodeToString(make([]byte, 64)),
		"two segments":                       parts[0] + "." + parts[1],
		"four segments":                      good + ".extra",
		"not a token":                        "hello",
		"empty":                              "",
	} {
		if claims, err := verifyJWT(token, testIssuer, testAudience, keys, testNow); err == nil {
			t.Errorf("%s: got subject %q, want a refusal", name, claims.Subject)
		}
	}
	if _, err := verifyJWT(good, testIssuer, testAudience, map[string]verifyingKey{}, testNow); err == nil {
		t.Error("a valid token with no keys configured: got = nil, want a refusal")
	}
}

func TestOnlyStrongKeysOfKnownKindsAreAccepted(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for name, key := range map[string]config.JWTKey{
		"no kid":            {Kty: "EC", Alg: "ES256", Crv: "P-256", X: ec.jwk.X, Y: ec.jwk.Y},
		"point off curve":   {Kid: "k", Kty: "EC", Alg: "ES256", Crv: "P-256", X: ec.jwk.X, Y: ec.jwk.X},
		"another curve":     {Kid: "k", Kty: "EC", Alg: "ES384", Crv: "P-384", X: ec.jwk.X, Y: ec.jwk.Y},
		"symmetric":         {Kid: "k", Kty: "oct", Alg: "HS256"},
		"EC key as RS256":   {Kid: "k", Kty: "EC", Alg: "RS256", Crv: "P-256", X: ec.jwk.X, Y: ec.jwk.Y},
		"short RSA modulus": {Kid: "k", Kty: "RSA", Alg: "RS256", N: encoding.EncodeToString(weak.N.Bytes()), E: "AQAB"},
		"RSA without n":     {Kid: "k", Kty: "RSA", Alg: "RS256", E: "AQAB"},
	} {
		if _, err := parseJWK(key); err == nil {
			t.Errorf("%s: got = nil, want an error", name)
		}
	}
}
