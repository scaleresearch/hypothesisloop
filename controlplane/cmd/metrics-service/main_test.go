package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
)

// Every route of metrics-service but its liveness refuses a request with no token before its
// handler runs.
func TestEveryRouteRefusesARequestWithoutAToken(t *testing.T) {
	served := false
	push := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
	r := newRouter(authtest.As{Kind: auth.KindStaff}, push, zap.NewNop())
	public := map[string]bool{"/health": true, "/healthz": true}
	routes := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[route] = true
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(method, route, nil))
		if public[route] {
			if recorder.Code != http.StatusOK {
				t.Errorf("%s %s: got = %d, want = 200", method, route, recorder.Code)
			}
			return nil
		}
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"error":"unauthenticated: `) {
			t.Errorf("%s %s: got = %d %s, want = 401 unauthenticated", method, route, recorder.Code, recorder.Body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/metrics", "/internal/node-metrics", "/health", "/healthz"} {
		if !routes[route] {
			t.Errorf("%s is not a route", route)
		}
	}
	if served {
		t.Error("the push handler ran for a request with no token")
	}
}
