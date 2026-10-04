package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// buildsAPI is a control plane that lists builds, hands out push logins and takes reports.
type buildsAPI struct {
	mu            sync.Mutex
	listed        []domain.EnvironmentBuild
	reportsBuilds []bool
	credentials   []domain.EnvironmentBuildCredentialRequest
	reports       map[string][]domain.EnvironmentBuildReport
	refuse        bool
}

func (api *buildsAPI) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		switch r.URL.Path {
		case "/internal/clusters/test/reconcile":
			var report struct {
				Builds bool `json:"builds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&report)
			api.reportsBuilds = append(api.reportsBuilds, report.Builds)
			_ = json.NewEncoder(w).Encode(map[string]any{"experiments": []any{}, "builds": api.listed})
		case "/internal/clusters/test/builds/k1/credential":
			var req domain.EnvironmentBuildCredentialRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			api.credentials = append(api.credentials, req)
			_ = json.NewEncoder(w).Encode(domain.RegistryLogin{Registry: "registry.test", Username: "oauth2accesstoken", Password: "push-token"})
		case "/internal/clusters/test/builds/k1", "/internal/clusters/test/builds/k2":
			if api.refuse {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var report domain.EnvironmentBuildReport
			_ = json.NewDecoder(r.Body).Decode(&report)
			key := r.URL.Path[len("/internal/clusters/test/builds/"):]
			api.reports[key] = append(api.reports[key], report)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (api *buildsAPI) list(builds ...domain.EnvironmentBuild) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.listed = builds
}

func (api *buildsAPI) reported(key string) []domain.EnvironmentBuildReport {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]domain.EnvironmentBuildReport(nil), api.reports[key]...)
}

// fakeBuilder runs a build until it is released or cancelled, asking for the push login once.
type fakeBuilder struct {
	mu        sync.Mutex
	runs      map[string]int
	cancelled map[string]int
	logins    []string
	release   chan struct{}
}

func newFakeBuilder() *fakeBuilder {
	return &fakeBuilder{runs: map[string]int{}, cancelled: map[string]int{}, release: make(chan struct{})}
}

func (b *fakeBuilder) Run(ctx context.Context, build domain.EnvironmentBuild, credential func(context.Context) (string, string, error), progress func(string, ...any)) domain.EnvironmentBuildReport {
	progress("[1/1] RUN make: started")
	b.mu.Lock()
	b.runs[build.Key]++
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled[build.Key]++
		b.mu.Unlock()
		return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildPlatformFault, LogTail: "cancelled"}
	case <-b.release:
	}
	username, password, err := credential(ctx)
	if err != nil {
		return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildPlatformFault, LogTail: err.Error()}
	}
	b.mu.Lock()
	b.logins = append(b.logins, username+":"+password)
	b.mu.Unlock()
	return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildPushed, Digest: "sha256:abc"}
}

func (b *fakeBuilder) count(m map[string]int, key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return m[key]
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func buildAgent(server *httptest.Server, builder Builder) *Agent {
	agent := newTestAgent(server, &captureExecutor{})
	agent.Builder = builder
	return agent
}

var k1 = domain.EnvironmentBuild{Key: "k1", Attempt: 1, Base: "registry.test/hl/job:1", Dockerfile: "ARG BASE\nFROM ${BASE}\n", Repository: "registry.test/envs/env"}

func TestAListedBuildRunsOnceFetchesItsLoginAtItsAttemptAndIsReported(t *testing.T) {
	api := &buildsAPI{reports: map[string][]domain.EnvironmentBuildReport{}}
	server := api.serve(t)
	builder := newFakeBuilder()
	agent := buildAgent(server, builder)
	ctx := context.Background()
	api.list(k1)

	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	close(builder.release)
	waitFor(t, "the report", func() bool { return len(api.reported("k1")) == 1 })
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	agent.building.Wait()

	if got := api.reported("k1")[0]; got.Attempt != 1 || got.Outcome != domain.EnvironmentBuildPushed || got.Digest != "sha256:abc" {
		t.Errorf("report %+v, want attempt 1 pushed with its digest", got)
	}
	if len(api.credentials) != 1 || api.credentials[0].Attempt != 1 || builder.logins[0] != "oauth2accesstoken:push-token" {
		t.Errorf("credential requests %v and logins %v, want one at attempt 1 handed to the daemon", api.credentials, builder.logins)
	}
	if runs := builder.count(builder.runs, "k1"); runs != 1 {
		t.Errorf("ran %d times, want a reported build not run again while it is still listed", runs)
	}
	if !api.reportsBuilds[0] {
		t.Error("an agent with a builder does not report that it builds")
	}
}

func TestABuildNoLongerListedAtItsAttemptIsCancelledAndNotReported(t *testing.T) {
	api := &buildsAPI{reports: map[string][]domain.EnvironmentBuildReport{}}
	server := api.serve(t)
	builder := newFakeBuilder()
	agent := buildAgent(server, builder)
	ctx := context.Background()
	k2 := domain.EnvironmentBuild{Key: "k2", Attempt: 1}

	api.list(k1, k2)
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both builds", func() bool { return builder.count(builder.runs, "k1") == 1 && builder.count(builder.runs, "k2") == 1 })
	reassigned := k1
	reassigned.Attempt = 2
	api.list(reassigned)
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cancels and the new attempt", func() bool {
		return builder.count(builder.cancelled, "k1") == 1 && builder.count(builder.cancelled, "k2") == 1 && builder.count(builder.runs, "k1") == 2
	})
	close(builder.release)
	waitFor(t, "the report", func() bool { return len(api.reported("k1")) == 1 })
	agent.building.Wait()

	if got := api.reported("k1"); got[0].Attempt != 2 {
		t.Errorf("reports %+v, want the second attempt's alone", got)
	}
	if got := api.reported("k2"); len(got) != 0 {
		t.Errorf("a cancelled build reported %+v", got)
	}
}

func TestABuildWhoseReportWasRefusedRunsAgain(t *testing.T) {
	api := &buildsAPI{reports: map[string][]domain.EnvironmentBuildReport{}, refuse: true}
	server := api.serve(t)
	builder := newFakeBuilder()
	close(builder.release)
	agent := buildAgent(server, builder)
	api.list(k1)

	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.building.Wait()
	api.mu.Lock()
	api.refuse = false
	api.mu.Unlock()
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.building.Wait()

	if runs := builder.count(builder.runs, "k1"); runs != 2 || len(api.reported("k1")) != 1 {
		t.Errorf("ran %d times with %d reports taken, want it run again and reported", runs, len(api.reported("k1")))
	}
}

func TestAnAgentWithoutABuilderSaysItDoesNotBuildAndRunsNone(t *testing.T) {
	api := &buildsAPI{reports: map[string][]domain.EnvironmentBuildReport{}}
	server := api.serve(t)
	agent := newTestAgent(server, &captureExecutor{})
	api.list(k1)
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.reportsBuilds[0] || len(agent.builds) != 0 {
		t.Errorf("reported builds %v and runs %v, want neither", api.reportsBuilds[0], agent.builds)
	}
}

// agentLog is what an agent logs, safe to read while its builds run.
type agentLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *agentLog) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *agentLog) has(line string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.lines, line)
}

// A build the control plane took the report of leaves the list on the next pass. It ended, and
// only a build a cancel cut short is told as cancelled.
func TestABuildIsToldAsItGoesAndCancelledOnlyWhenACancelCutItShort(t *testing.T) {
	api := &buildsAPI{reports: map[string][]domain.EnvironmentBuildReport{}}
	server := api.serve(t)
	builder := newFakeBuilder()
	agent := buildAgent(server, builder)
	var logged agentLog
	agent.Log = logged.log
	ctx := context.Background()
	k2 := domain.EnvironmentBuild{Key: "k2", Attempt: 1}

	api.list(k1, k2)
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both builds", func() bool { return builder.count(builder.runs, "k1") == 1 && builder.count(builder.runs, "k2") == 1 })
	api.list(k1)
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	close(builder.release)
	waitFor(t, "the report", func() bool { return len(api.reported("k1")) == 1 })
	api.list()
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	agent.building.Wait()

	if !logged.has("build k1 attempt 1: [1/1] RUN make: started") {
		t.Errorf("logged %q, want the build's steps", logged.lines)
	}
	if !logged.has("build k2 attempt 1: no longer this cluster's, cancelled") {
		t.Errorf("logged %q, want the build a cancel cut short told as cancelled", logged.lines)
	}
	if logged.has("build k1 attempt 1: no longer this cluster's, cancelled") {
		t.Errorf("logged %q: a build that ended and reported is told as cancelled", logged.lines)
	}
}
