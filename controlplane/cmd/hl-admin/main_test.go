package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func settingsPath(t *testing.T) string {
	t.Helper()
	committed, err := os.ReadFile(filepath.Join("..", "..", "settings", "hypothesisloop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	rendered := strings.ReplaceAll(string(committed), config.Placeholder+"HOST-ADDRESS", "host.example.invalid")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HYPOTHESISLOOP_CONFIG", path)
	t.Setenv("HYPOTHESISLOOP_SIGNING_SECRET", "a-signing-secret-only-this-test-knows")
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
		{"user", "staff", "subject"}, {"user", "staff", "subject", "yes"}, {"cluster", "share", "name"}, {"cluster", "share", "name", "true"},
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
	for _, args := range [][]string{
		{"cluster", "add", "name", "default"}, {"cluster", "share", "name", "on"}, {"user", "staff", "subject", "off"},
	} {
		if err := run(args, &stdout); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("hl-admin %v: got = %v, want an error naming DATABASE_URL", args, err)
		}
	}
}

type fakeHomes struct {
	orgIDs   []string
	projects []*domain.Project
}

func (f fakeHomes) EnsureUserHome(_ context.Context, userID, _ string) (*domain.User, []string, error) {
	return &domain.User{ID: userID}, f.orgIDs, nil
}

func (f fakeHomes) ListProjects(context.Context, []string) ([]*domain.Project, error) {
	return f.projects, nil
}

func TestUserAddSaysSoWhenTheUserHasNoOrgOrTheOrgNoProject(t *testing.T) {
	var out bytes.Buffer
	if err := addUser(context.Background(), fakeHomes{}, "user-1", "Ada", &out); err == nil || !strings.Contains(err.Error(), "belongs to no org") {
		t.Errorf("a user in no org: got = %v, want an error saying so", err)
	}
	if err := addUser(context.Background(), fakeHomes{orgIDs: []string{"org_1"}}, "user-1", "Ada", &out); err == nil || !strings.Contains(err.Error(), "has no project") {
		t.Errorf("an org with no project: got = %v, want an error saying so", err)
	}
	if out.Len() != 0 {
		t.Errorf("output on failure: got = %q, want none", out.String())
	}
	home := fakeHomes{orgIDs: []string{"org_1", "org_2"}, projects: []*domain.Project{{ID: "prj_1"}}}
	if err := addUser(context.Background(), home, "user-1", "Ada", &out); err != nil || !strings.Contains(out.String(), `"org_id":"org_1"`) || !strings.Contains(out.String(), `"project_id":"prj_1"`) {
		t.Errorf("a user with a home: got = %q, %v", out.String(), err)
	}
}
