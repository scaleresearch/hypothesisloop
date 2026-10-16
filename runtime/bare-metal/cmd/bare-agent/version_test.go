package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/hostinstall"
)

// The version is linked into the binary, so the agent on a node says what it was built as
// whatever its environment holds.
func TestTheBinaryReportsTheVersionItWasBuiltWith(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bare-agent")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=abc1234", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "abc1234" {
		t.Fatalf("bare-agent version = %q (err %v), want abc1234", out, err)
	}
}

// The agent install checks the token with sends the binary's version, as the one run on a node
// does on every call through the shared loop.
func TestTheAgentSendsItsVersionOnTheControlPlane(t *testing.T) {
	version = "abc1234"
	t.Cleanup(func() { version = "" })
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get(clusterapi.AgentVersionHeader)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	agent := remoteFor(hostinstall.Settings{ClusterName: "node-a", APIURL: server.URL, Token: "t"})
	if err := agent.CheckToken(context.Background()); err != nil {
		t.Fatalf("check token: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen["/internal/clusters/node-a/access"]; got != "abc1234" {
		t.Errorf("%s = %q, want the binary's version", clusterapi.AgentVersionHeader, got)
	}
}
