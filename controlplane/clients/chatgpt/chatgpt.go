package chatgpt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const (
	// ResponsesURL is where the Codex CLI makes a model call on a ChatGPT subscription.
	ResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	// TokenURL is OpenAI's OAuth token endpoint, which renews a ChatGPT access token.
	TokenURL = "https://auth.openai.com/oauth/token"
	// clientID is the Codex CLI's OAuth client, which a ChatGPT sign-in of the Codex CLI is of.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// renewBefore is how long before its expiry an access token is renewed, as the Codex CLI does.
	renewBefore = 5 * time.Minute
	// maxAnswerBytes bounds the token endpoint's answer read.
	maxAnswerBytes = 1 << 20
)

// ParseAuthJSON is the credential of the auth.json content of a Codex CLI signed in with ChatGPT,
// and the content's digest, which tells one seed from another.
func ParseAuthJSON(content string) (domain.ChatGPTCredential, string, error) {
	var auth struct {
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(content), &auth); err != nil {
		return domain.ChatGPTCredential{}, "", errors.New("chatgpt: the auth.json is no JSON object")
	}
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" || auth.Tokens.AccountID == "" {
		return domain.ChatGPTCredential{}, "", errors.New("chatgpt: the auth.json holds no tokens.access_token, tokens.refresh_token and tokens.account_id: sign the Codex CLI in with ChatGPT")
	}
	expires, err := expiry(auth.Tokens.AccessToken)
	if err != nil {
		return domain.ChatGPTCredential{}, "", err
	}
	digest := sha256.Sum256([]byte(content))
	return domain.ChatGPTCredential{AccessToken: auth.Tokens.AccessToken, RefreshToken: auth.Tokens.RefreshToken,
		AccountID: auth.Tokens.AccountID, ExpiresAt: expires}, hex.EncodeToString(digest[:]), nil
}

// expiry is the exp claim of an access token, a JWT read and not verified: OpenAI verifies it.
func expiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("chatgpt: the access token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, errors.New("chatgpt: the access token states no expiry")
	}
	return time.Unix(claims.Exp, 0), nil
}

// Store keeps the credential. *db.Store satisfies it.
type Store interface {
	FreshChatGPTCredential(ctx context.Context, due func(domain.ChatGPTCredential) bool,
		renew func(context.Context, domain.ChatGPTCredential) (domain.ChatGPTCredential, error)) (domain.ChatGPTCredential, error)
	ExpireChatGPTCredential(ctx context.Context, accessToken string) error
}

// Credentials is the stored credential, renewed at OpenAI's token endpoint when it is about to
// expire.
type Credentials struct {
	store    Store
	tokenURL string
	client   *http.Client
	now      func() time.Time
}

// NewCredentials reads the credential from store and renews it at tokenURL.
func NewCredentials(store Store, tokenURL string) *Credentials {
	return &Credentials{store: store, tokenURL: tokenURL, client: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
}

// Fresh is a credential whose access token outlives the next renewBefore.
func (c *Credentials) Fresh(ctx context.Context) (domain.ChatGPTCredential, error) {
	return c.store.FreshChatGPTCredential(ctx, func(cred domain.ChatGPTCredential) bool {
		return !c.now().Add(renewBefore).Before(cred.ExpiresAt)
	}, c.renew)
}

// Expire makes the credential due when accessToken, one the ChatGPT backend refused, is its
// access token: the next Fresh renews it.
func (c *Credentials) Expire(ctx context.Context, accessToken string) error {
	return c.store.ExpireChatGPTCredential(ctx, accessToken)
}

// renew trades the refresh token of cred for a new pair, as the Codex CLI does.
func (c *Credentials) renew(ctx context.Context, cred domain.ChatGPTCredential) (domain.ChatGPTCredential, error) {
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": cred.RefreshToken, "scope": "openid profile email"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return cred, fmt.Errorf("chatgpt: renew: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return cred, fmt.Errorf("chatgpt: renew: %w", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return cred, fmt.Errorf("chatgpt: renew: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var refused struct {
			Error any `json:"error"`
		}
		_ = json.Unmarshal(answer, &refused)
		return cred, fmt.Errorf("chatgpt: renew: the token endpoint answered %s (%v): sign the Codex CLI in again and set MODEL_OPENAI_AUTH_JSON to its auth.json", resp.Status, refused.Error)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(answer, &tokens); err != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return cred, errors.New("chatgpt: renew: the token endpoint answered no access_token and refresh_token")
	}
	expires, err := expiry(tokens.AccessToken)
	if err != nil {
		return cred, err
	}
	return domain.ChatGPTCredential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, AccountID: cred.AccountID, ExpiresAt: expires}, nil
}
