package agentloop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// Every call names the version the agent runs, so the control plane shows a cluster left on an
// older install.
func TestEveryCallToTheControlPlaneCarriesTheAgentVersion(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get(clusterapi.AgentVersionHeader)
		mu.Unlock()
		if r.URL.Path == "/internal/clusters/test/access" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"experiments":[],"status":"ok"}`))
	}))
	defer server.Close()

	agent := newTestAgent(server, &captureExecutor{})
	remoteOf(agent).Version = "abc1234"
	if _, err := agent.fetchDesiredState(context.Background(), agent.Executor); err != nil {
		t.Fatalf("fetch desired state: %v", err)
	}
	agent.pushStatus(context.Background(), nil)
	if err := remoteOf(agent).CheckToken(context.Background()); err != nil {
		t.Fatalf("check token: %v", err)
	}
	for _, path := range []string{"/internal/clusters/test/reconcile", "/internal/clusters/test/status", "/internal/clusters/test/access"} {
		if seen[path] != "abc1234" {
			t.Errorf("%s on %s: got = %q, want the agent's version", clusterapi.AgentVersionHeader, path, seen[path])
		}
	}
}

// The first exchange the control plane answers is logged once: the silence before it is what an
// operator watching a new install waits through.
func TestTheFirstContactIsLoggedOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"experiments":[],"status":"ok"}`))
	}))
	defer server.Close()
	var mu sync.Mutex
	var logged []string
	agent := newTestAgent(server, &captureExecutor{})
	agent.Log = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	agent.ReconcileInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = agent.reconcileLoop(ctx)
	mu.Lock()
	defer mu.Unlock()
	first := 0
	for _, line := range logged {
		if strings.HasPrefix(line, "first contact with the control plane ok") {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("first contact logged %d times in %q, want once", first, logged)
	}
}

// The dry run's token check says which of the control plane's answers it got.
func TestTheTokenCheckSaysWhatTheControlPlaneAnswered(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   func(error) bool
	}{
		"taken":        {http.StatusNoContent, "", func(err error) bool { return err == nil }},
		"unregistered": {http.StatusUnauthorized, `{"reason":"` + clusterapi.ReasonClusterNotRegistered + `"}`, func(err error) bool { return errors.Is(err, ErrClusterRemoved) }},
		"refused": {http.StatusUnauthorized, `{"message":"unauthenticated: bad signature"}`, func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "401") && strings.Contains(err.Error(), "bad signature")
		}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		err := remoteOf(newTestAgent(server, &captureExecutor{})).CheckToken(context.Background())
		server.Close()
		if !tc.want(err) {
			t.Errorf("%s: CheckToken = %v", name, err)
		}
	}
}
