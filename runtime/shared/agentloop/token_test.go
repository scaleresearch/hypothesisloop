package agentloop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestBothCallsToTheControlPlaneCarryTheRuntimeToken(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"experiments":[],"status":"ok"}`))
	}))
	defer server.Close()

	agent := newTestAgent(server, &captureExecutor{})
	agent.Token = "runtime-token"
	if _, _, err := agent.fetchDesiredState(context.Background()); err != nil {
		t.Fatalf("fetch desired state: %v", err)
	}
	agent.pushStatus(context.Background(), nil)

	for _, path := range []string{"/internal/clusters/test/reconcile", "/internal/clusters/test/status"} {
		if seen[path] != "Bearer runtime-token" {
			t.Errorf("Authorization on %s: got = %q, want = %q", path, seen[path], "Bearer runtime-token")
		}
	}
}
