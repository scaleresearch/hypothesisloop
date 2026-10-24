package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func jwt(exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestAnAuthJSONIsItsTokensAndTheirExpiry(t *testing.T) {
	exp := time.Unix(1793626534, 0)
	content := `{"auth_mode":"chatgpt","tokens":{"id_token":"i","access_token":"` + jwt(exp) + `","refresh_token":"r-1","account_id":"acc"}}`
	cred, digest, err := ParseAuthJSON(content)
	if err != nil || cred.RefreshToken != "r-1" || cred.AccountID != "acc" || !cred.ExpiresAt.Equal(exp) || len(digest) != 64 {
		t.Fatalf("%+v %q %v", cred, digest, err)
	}
	if _, again, _ := ParseAuthJSON(content + " "); again == digest {
		t.Error("two auth.json contents share a digest")
	}
	for name, bad := range map[string]string{
		"an API key login": `{"OPENAI_API_KEY":"sk-1","tokens":null}`,
		"no JSON":          `tokens`,
		"no JWT":           `{"tokens":{"access_token":"opaque","refresh_token":"r","account_id":"a"}}`,
	} {
		if _, _, err := ParseAuthJSON(bad); err == nil {
			t.Errorf("%s: parsed, want refused", name)
		}
	}
}

// store is one stored credential, renewed as the database renews it.
type store struct{ cred domain.ChatGPTCredential }

func (s *store) FreshChatGPTCredential(ctx context.Context, due func(domain.ChatGPTCredential) bool,
	renew func(context.Context, domain.ChatGPTCredential) (domain.ChatGPTCredential, error)) (domain.ChatGPTCredential, error) {
	if !due(s.cred) {
		return s.cred, nil
	}
	renewed, err := renew(ctx, s.cred)
	if err != nil {
		return domain.ChatGPTCredential{}, err
	}
	s.cred = renewed
	return renewed, nil
}

func (s *store) ExpireChatGPTCredential(_ context.Context, accessToken string) error {
	if s.cred.AccessToken == accessToken {
		s.cred.ExpiresAt = time.Time{}
	}
	return nil
}

func TestACredentialAboutToExpireIsRenewedAsTheCodexCLIRenewsIt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	renewedExp := now.Add(10 * 24 * time.Hour)
	var asked []map[string]string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body)
		if r.Header.Get("Content-Type") != "application/json" || body["refresh_token"] != "r-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_reused","message":"already used"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "i", "access_token": jwt(renewedExp), "refresh_token": "r-2"})
	}))
	defer endpoint.Close()
	s := &store{cred: domain.ChatGPTCredential{AccessToken: jwt(now.Add(time.Hour)), RefreshToken: "r-1", AccountID: "acc", ExpiresAt: now.Add(time.Hour)}}
	c := NewCredentials(s, endpoint.URL)
	c.now = func() time.Time { return now }
	if cred, err := c.Fresh(context.Background()); err != nil || cred.RefreshToken != "r-1" || len(asked) != 0 {
		t.Fatalf("an hour from expiry: %+v %v after %d renewals, want the stored one", cred, err, len(asked))
	}
	c.now = func() time.Time { return now.Add(56 * time.Minute) }
	cred, err := c.Fresh(context.Background())
	if err != nil || cred.RefreshToken != "r-2" || cred.AccountID != "acc" || !cred.ExpiresAt.Equal(renewedExp) {
		t.Fatalf("4 minutes from expiry: %+v %v, want it renewed", cred, err)
	}
	want := map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": "r-1", "scope": "openid profile email"}
	if len(asked) != 1 || len(asked[0]) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for k, v := range want {
		if asked[0][k] != v {
			t.Errorf("%s = %q, want %q", k, asked[0][k], v)
		}
	}
	if cred, err := c.Fresh(context.Background()); err != nil || cred.RefreshToken != "r-2" || len(asked) != 1 {
		t.Fatalf("renewed: %+v %v after %d renewals, want the renewed one kept", cred, err, len(asked))
	}
	if err := c.Expire(context.Background(), cred.AccessToken); err != nil {
		t.Fatal(err)
	}
	s.cred.RefreshToken = "r-1"
	if cred, err := c.Fresh(context.Background()); err != nil || len(asked) != 2 || !cred.ExpiresAt.Equal(renewedExp) {
		t.Fatalf("its access token refused: %+v %v after %d renewals, want it renewed again", cred, err, len(asked))
	}
	s.cred.ExpiresAt, s.cred.RefreshToken = now, "r-1-reused"
	if _, err := c.Fresh(context.Background()); err == nil || !strings.Contains(err.Error(), "MODEL_OPENAI_AUTH_JSON") {
		t.Fatalf("a refused renewal: %v, want an error saying to sign in again", err)
	}
}
