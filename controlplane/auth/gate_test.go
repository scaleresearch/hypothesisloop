package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

type tokenFunc func(token string) (Principal, error)

func (f tokenFunc) Authenticate(_ context.Context, token string) (Principal, error) { return f(token) }

var gateTokens = tokenFunc(func(token string) (Principal, error) {
	switch token {
	case "staff":
		return Principal{Kind: KindStaff}, nil
	case "removed-cluster":
		refusal := unauthenticated("cluster gone is not registered")
		refusal.Reason = domain.ReasonClusterNotRegistered
		return Principal{}, refusal
	case "broken-directory":
		return Principal{}, errors.New("pq: database is down at 10.0.0.7")
	}
	return Principal{}, unauthenticated("unknown token")
})

func gated(t *testing.T) (http.Handler, *Principal) {
	t.Helper()
	var seen Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
	return Gate(gateTokens, zap.NewNop(), map[string]string{"/watch": "proto"})(next), &seen
}

func serve(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func TestGateLetsOnlyItsAllowListThroughWithoutAToken(t *testing.T) {
	h, _ := gated(t)
	for path, open := range map[string]bool{
		"/health": true, "/healthz": true, "/openapi.json": true, "/docs": true, "/schemas/Thing.json": true,
		"/explore": true, "/explore/coordinator": true, "/explore/rules": true,
		"/internal/clusters/openapi.json": true, "/internal/clusters/schemas/X.json": true,
		"/metrics": false, "/me": false, "/internal/clusters": false, "/internal/clusters/c/state": false,
		"/internal/clustersexplore": false, "/internal/node-metrics": false, "/watch": false, "/platform-experiments/explore": false,
		"/nowhere": false,
	} {
		got := serve(h, path, nil).Code
		if open && got != http.StatusTeapot {
			t.Errorf("%s: got = %d, want it served without a token", path, got)
		}
		if !open && got != http.StatusUnauthorized {
			t.Errorf("%s: got = %d, want = 401", path, got)
		}
	}
}

func TestGateAnswersEachRefusalInTheAPIsEnvelope(t *testing.T) {
	h, seen := gated(t)
	for name, tc := range map[string]struct {
		authorization string
		status        int
		body          string
	}{
		"no token":        {"", http.StatusUnauthorized, `{"reason":"unauthenticated","message":"send Authorization: Bearer <token>"}`},
		"not a bearer":    {"Basic abc", http.StatusUnauthorized, `{"reason":"unauthenticated","message":"send Authorization: Bearer <token>"}`},
		"unknown token":   {"Bearer nope", http.StatusUnauthorized, `{"reason":"unauthenticated","message":"unknown token"}`},
		"removed cluster": {"Bearer removed-cluster", http.StatusUnauthorized, `{"reason":"cluster_not_registered","message":"cluster gone is not registered"}`},
		"broken store":    {"Bearer broken-directory", http.StatusInternalServerError, `{"reason":"internal","message":"internal error"}`},
	} {
		recorder := serve(h, "/me", map[string]string{"Authorization": tc.authorization})
		if recorder.Code != tc.status || strings.TrimSpace(recorder.Body.String()) != tc.body || recorder.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: got = %d %s, want = %d %s", name, recorder.Code, recorder.Body, tc.status, tc.body)
		}
	}
	if recorder := serve(h, "/me", map[string]string{"Authorization": "Bearer staff"}); recorder.Code != http.StatusTeapot || seen.Kind != KindStaff {
		t.Fatalf("staff: got = %d with %+v, want the handler given staff", recorder.Code, *seen)
	}
}

func TestAWebSocketPathTakesItsTokenFromTheSubprotocolOnly(t *testing.T) {
	h, _ := gated(t)
	for name, tc := range map[string]struct {
		headers map[string]string
		want    int
	}{
		"subprotocol and token":    {map[string]string{"Sec-WebSocket-Protocol": "proto, staff"}, http.StatusTeapot},
		"bearer header":            {map[string]string{"Authorization": "Bearer staff"}, http.StatusUnauthorized},
		"token alone":              {map[string]string{"Sec-WebSocket-Protocol": "staff"}, http.StatusUnauthorized},
		"another subprotocol":      {map[string]string{"Sec-WebSocket-Protocol": "other, staff"}, http.StatusUnauthorized},
		"three offered":            {map[string]string{"Sec-WebSocket-Protocol": "proto, staff, more"}, http.StatusUnauthorized},
		"an unknown token offered": {map[string]string{"Sec-WebSocket-Protocol": "proto, nope"}, http.StatusUnauthorized},
	} {
		if got := serve(h, "/watch", tc.headers).Code; got != tc.want {
			t.Errorf("%s: got = %d, want = %d", name, got, tc.want)
		}
	}
	if body := serve(h, "/watch", nil).Body.String(); !strings.Contains(body, "send Sec-WebSocket-Protocol: proto, ") {
		t.Errorf("a refusal on a WebSocket path names the subprotocol, got %s", body)
	}
}

func TestRecheckResolvesTheAdmittedTokenAgain(t *testing.T) {
	valid := true
	tokens := tokenFunc(func(string) (Principal, error) {
		if !valid {
			return Principal{}, unauthenticated("revoked")
		}
		return Principal{Kind: KindKey}, nil
	})
	var recheck func() (Principal, error)
	h := Gate(tokens, zap.NewNop(), nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		recheck = func() (Principal, error) { return Recheck(r.Context()) }
	}))
	serve(h, "/stream", map[string]string{"Authorization": "Bearer k"})
	if p, err := recheck(); err != nil || p.Kind != KindKey {
		t.Fatalf("while valid: got = %+v %v, want the key", p, err)
	}
	valid = false
	if _, err := recheck(); err == nil {
		t.Fatal("after revoking: got no error, want the refusal")
	}
	if _, err := Recheck(context.Background()); err == nil {
		t.Fatal("outside the gate: got no error, want one")
	}
}

func TestARouteRequirementWithoutTheGateFailsClosed(t *testing.T) {
	h := RequireStaff(zap.NewNop(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran with no principal")
	}))
	if got := serve(h, "/metrics", map[string]string{"Authorization": "Bearer staff"}).Code; got != http.StatusInternalServerError {
		t.Fatalf("got = %d, want = 500", got)
	}
}

// git sends the token as a Bearer header, the only way the git proxy takes it: Basic
// authentication is no credential anywhere, and nothing asks for it.
func TestGateTakesATokenOnTheGitPathsAsBearerAlone(t *testing.T) {
	h, _ := gated(t)
	basic := func(user, password string) map[string]string {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.SetBasicAuth(user, password)
		return map[string]string{"Authorization": request.Header.Get("Authorization")}
	}
	unauthenticated := serve(h, "/git/pe-1/info/refs", nil)
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("no token on /git/: got = %d with WWW-Authenticate %q, want 401 asking for nothing", unauthenticated.Code, unauthenticated.Header().Get("WWW-Authenticate"))
	}
	if got := serve(h, "/git/pe-1/info/refs", map[string]string{"Authorization": "Bearer staff"}); got.Code != http.StatusTeapot {
		t.Errorf("a Bearer token on /git/: got = %d, want it served", got.Code)
	}
	for _, path := range []string{"/git/pe-1/info/refs", "/me"} {
		if got := serve(h, path, basic("anyone", "staff")); got.Code != http.StatusUnauthorized || got.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("Basic authentication on %s: got = %d with WWW-Authenticate %q, want 401 asking for nothing", path, got.Code, got.Header().Get("WWW-Authenticate"))
		}
	}
}
