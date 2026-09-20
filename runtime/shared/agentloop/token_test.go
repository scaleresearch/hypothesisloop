package agentloop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
	if _, _, err := agent.fetchDesiredState(context.Background(), agent.Executor); err != nil {
		t.Fatalf("fetch desired state: %v", err)
	}
	agent.pushStatus(context.Background(), nil)

	for _, path := range []string{"/internal/clusters/test/reconcile", "/internal/clusters/test/status"} {
		if seen[path] != "Bearer runtime-token" {
			t.Errorf("Authorization on %s: got = %q, want = %q", path, seen[path], "Bearer runtime-token")
		}
	}
}

// A refused runtime keeps asking, since the fix is somebody else's to make, and says each time
// what the control plane told it.
func TestARefusalIsLoggedWithTheControlPlanesReasonAndTheLoopGoesOn(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthenticated: cluster test is not registered"}`))
	}))
	defer server.Close()

	var logged []string
	agent := newTestAgent(server, &captureExecutor{})
	agent.Log = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	agent.ReconcileInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	agent.reconcileLoop(ctx)
	agent.pushStatus(context.Background(), nil)

	mu.Lock()
	defer mu.Unlock()
	if asked < 4 {
		t.Fatalf("requests after a refusal: got = %d, want the loop to keep asking", asked)
	}
	for _, where := range []string{"reconcile: ", "push status: "} {
		found := false
		for _, line := range logged {
			found = found || (strings.HasPrefix(line, where) && strings.Contains(line, "401") && strings.Contains(line, "cluster test is not registered"))
		}
		if !found {
			t.Errorf("no %q line carries the status and the reason: %q", where, logged)
		}
	}
}
