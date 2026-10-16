package agentloop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// drainingExecutor forgets a workload once it is deleted, as a real one does.
type drainingExecutor struct {
	*captureExecutor
}

func (e drainingExecutor) left() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var left []string
	for _, id := range e.managed {
		if !slices.Contains(e.deleted, id) {
			left = append(left, id)
		}
	}
	return left
}

func (e drainingExecutor) ListManagedJobs(context.Context) ([]string, error) { return e.left(), nil }
func (e drainingExecutor) ListManagedJobsForStatus(context.Context) ([]string, error) {
	return e.left(), nil
}

// refusingServer answers every call 401 with body and records what was asked.
func refusingServer(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// A removed cluster's jobs went back to the queue with the removal. What the machine still runs
// of them holds its accelerators for nobody, so the agent stops all of it and then exits.
func TestARemovedClustersAgentStopsItsWorkloadsAndReturns(t *testing.T) {
	server, asked := refusingServer(t, `{"error":"unauthenticated: cluster test is not registered","reason":"cluster_not_registered"}`)
	exec := drainingExecutor{&captureExecutor{managed: []string{"job-a", "job-b"}}}
	agent := newTestAgent(server, exec.captureExecutor)
	agent.Executor = exec
	agent.ReconcileInterval = 10 * time.Millisecond
	agent.StatusInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agent.Run(ctx); !errors.Is(err, ErrClusterRemoved) {
		t.Fatalf("Run returned %v, want ErrClusterRemoved", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run cancelled the caller's context, which an executor's own stops run under")
	}
	if left := exec.left(); len(left) != 0 {
		t.Fatalf("workloads still held after the cluster was removed: %v", left)
	}
	for _, call := range asked() {
		if strings.HasSuffix(call, "/status") {
			t.Fatalf("a removed cluster's agent still pushed status: %v", asked())
		}
	}
}

// A token refused for any other cause is somebody's to fix, and the jobs keep running meanwhile.
func TestARefusalWithoutThatReasonStopsNothing(t *testing.T) {
	server, _ := refusingServer(t, `{"error":"unauthenticated: invalid token signature"}`)
	exec := drainingExecutor{&captureExecutor{managed: []string{"job-a"}}}
	agent := newTestAgent(server, exec.captureExecutor)
	agent.Executor = exec
	agent.ReconcileInterval = 10 * time.Millisecond
	agent.StatusInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil when its context ends", err)
	}
	if left := exec.left(); len(left) != 1 {
		t.Fatalf("workloads held: %v, want job-a untouched", left)
	}
}

// Detach stops what this cluster holds here and asks nothing of the control plane: removing the
// cluster there is its owner's alone.
func TestDetachStopsEveryWorkloadAndTellsTheControlPlaneNothing(t *testing.T) {
	server, asked := refusingServer(t, `{"error":"unexpected"}`)
	exec := drainingExecutor{&captureExecutor{managed: []string{"job-a", "job-b"}}}
	agent := newTestAgent(server, exec.captureExecutor)
	agent.Executor = exec
	agent.ReconcileInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agent.Detach(ctx); err != nil {
		t.Fatalf("Detach returned %v", err)
	}
	if left := exec.left(); len(left) != 0 {
		t.Fatalf("workloads still held after detach: %v", left)
	}
	if calls := asked(); len(calls) != 0 {
		t.Fatalf("detach called the control plane: %v", calls)
	}
}

func TestAnAgentSaysHowToRemoveAClusterThatIsStillRegistered(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"registered":     {http.StatusNoContent, "", "is still registered at"},
		"not registered": {http.StatusUnauthorized, `{"error":"unauthenticated","reason":"cluster_not_registered"}`, "is not registered at"},
		"unreachable":    {http.StatusBadGateway, "", "if it is, remove it"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/access") {
				t.Errorf("%s: asked %s %s, want GET .../access", name, r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		var said string
		newTestAgent(server, &captureExecutor{}).SayIfStillRegistered(context.Background(), func(format string, args ...any) {
			said = fmt.Sprintf(format, args...)
		})
		server.Close()
		if !strings.Contains(said, tc.want) {
			t.Errorf("%s: said %q, want it to contain %q", name, said, tc.want)
		}
	}
}
