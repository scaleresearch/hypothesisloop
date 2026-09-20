package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

func settingsPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "settings", "hypothesisloop.yaml")
	t.Setenv("HYPOTHESISLOOP_CONFIG", path)
	return path
}

func TestTokenPrintsTheTokenTheControlPlaneOfThatConfigAccepts(t *testing.T) {
	cfg, err := config.Load(settingsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{auth.StaffScope, auth.RuntimeScope("local"), auth.AgentScope("pe-1", "agent-1"), auth.JobScope("job-1")} {
		var stdout bytes.Buffer
		if err := run([]string{"token", scope}, &stdout); err != nil {
			t.Fatalf("token %s: %v", scope, err)
		}
		want, _ := auth.Mint(cfg.Auth.SigningSecret, scope)
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Errorf("token %s: got = %q, want = %q", scope, got, want)
		}
	}
}

func TestAMalformedScopeOrCommandIsAnError(t *testing.T) {
	settingsPath(t)
	for _, args := range [][]string{
		{"token", "root"}, {"token", "agent:pe-only"}, {"token"}, {"cluster", "add", "name"}, {"user", "add", "subject"}, {},
	} {
		var stdout bytes.Buffer
		if err := run(args, &stdout); err == nil || stdout.Len() != 0 {
			t.Errorf("hl-admin %v: got = %v, printed %q, want an error and nothing printed", args, err, stdout.String())
		}
	}
}

func TestDatabaseCommandsNeedADatabase(t *testing.T) {
	settingsPath(t)
	t.Setenv("DATABASE_URL", "")
	var stdout bytes.Buffer
	if err := run([]string{"cluster", "add", "name", "default"}, &stdout); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("got = %v, want an error naming DATABASE_URL", err)
	}
}
