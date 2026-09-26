package agentloop

import (
	"context"
	"errors"
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
	agent.ReconcileInterval = time.Millisecond
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
	agent.ReconcileInterval = time.Millisecond
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

func TestDetachEndsTheClusterWithItsOwnTokenAndNamesTheRequeuedJobs(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"requeued":["job-a","job-b"]}`))
	}))
	defer server.Close()
	agent := newTestAgent(server, &captureExecutor{})
	agent.Token = "runtime-token"

	requeued, err := agent.Detach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "DELETE /internal/clusters/test Bearer runtime-token" || len(requeued) != 2 {
		t.Fatalf("request = %q, requeued = %v", got, requeued)
	}
}

// Detaching twice, or after the owner removed the cluster, says the cluster is already gone.
func TestDetachOfAClusterAlreadyRemovedSaysSo(t *testing.T) {
	server, _ := refusingServer(t, `{"error":"unauthenticated: cluster test is not registered","reason":"cluster_not_registered"}`)
	agent := newTestAgent(server, &captureExecutor{})
	if _, err := agent.Detach(context.Background()); !errors.Is(err, ErrClusterRemoved) {
		t.Fatalf("Detach returned %v, want ErrClusterRemoved", err)
	}
}
