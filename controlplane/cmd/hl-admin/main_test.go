package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost/github"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// inSettings starts hl-admin with the committed settings as a deployment renders them.
func inSettings(t *testing.T) {
	t.Helper()
	committed, err := os.ReadFile(filepath.Join("..", "..", "settings", "hypothesisloop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	rendered := strings.ReplaceAll(string(committed), config.Placeholder+"HOST-ADDRESS", "host.example.invalid")
	rendered = strings.ReplaceAll(rendered, config.Placeholder+"IMAGE-TAG", "abc1234")
	rendered = strings.ReplaceAll(rendered, config.Placeholder+"UI-ORIGIN", "http://localhost:3000")
	rendered += "\ngit_host:\n  proxy_url: https://api.example.invalid/git\n  provider: plain\n  plain:\n    base_url: https://git.example.invalid\n"
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.SettingsPath.Name, path)
	t.Setenv(config.AuthSigningSecret.Name, signingSecret)
	t.Setenv(config.DatabaseURL.Name, "postgres://test.example.invalid/hypothesisloop")
}

const signingSecret = "a-signing-secret-only-this-test-knows"

func TestTokenPrintsTheTokenTheControlPlaneOfThatConfigAccepts(t *testing.T) {
	inSettings(t)
	for _, scope := range []string{auth.StaffScope, auth.RuntimeScope("local"), auth.AgentScope("pe-1", "agent-1"), auth.JobScope("job-1")} {
		var stdout bytes.Buffer
		if err := run([]string{"token", scope}, &stdout); err != nil {
			t.Fatalf("token %s: %v", scope, err)
		}
		want, _ := auth.Mint(signingSecret, scope)
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Errorf("token %s: got = %q, want = %q", scope, got, want)
		}
	}
}

func TestAMalformedScopeOrCommandIsAnError(t *testing.T) {
	inSettings(t)
	for _, args := range [][]string{
		{"token", "root"}, {"token", "agent:pe-only"}, {"token"}, {"cluster", "add", "name"}, {"user", "add", "subject"}, {},
		{"user", "staff", "subject"}, {"user", "staff", "subject", "yes"}, {"cluster", "share", "name"}, {"cluster", "share", "name", "true"},
		{"platform-experiment", "share"}, {"platform-experiment", "share", "pe-1", "on"}, {"platform-experiment", "show", "pe-1"},
		{"launcher", "add"}, {"launcher", "remove"}, {"launcher", "add", "main", "default"}, {"launcher", "list", "main"},
		{"cluster", "settings", "name"}, {"cluster", "settings", "name", "{}"}, {"cluster", "settings", "name", `{"silence":1}`},
	} {
		var stdout bytes.Buffer
		if err := run(args, &stdout); err == nil || stdout.Len() != 0 {
			t.Errorf("hl-admin %v: got = %v, printed %q, want an error and nothing printed", args, err, stdout.String())
		}
	}
}

func TestDatabaseCommandsNeedADatabase(t *testing.T) {
	inSettings(t)
	t.Setenv(config.DatabaseURL.Name, "")
	var stdout bytes.Buffer
	for _, args := range [][]string{
		{"cluster", "add", "name", "default"}, {"cluster", "share", "name", "on"}, {"user", "staff", "subject", "off"},
		{"cluster", "settings", "name", `{"status_silence_ceiling_seconds":21600}`}, {"launcher", "add", "main"}, {"launcher", "remove", "main"},
		{"platform-experiment", "share", "pe-1"}, {"platform-experiment", "unshare", "pe-1"},
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

func (f fakeHomes) EnsureUserHome(_ context.Context, userID, _, _ string) (*domain.User, []string, error) {
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

type fakeInstallationStore struct {
	orgs      map[string]bool
	connected []*domain.OrgGitHubInstallation
}

func (f *fakeInstallationStore) ConnectGitHubInstallation(_ context.Context, installation *domain.OrgGitHubInstallation) error {
	if !f.orgs[installation.OrgID] {
		return db.ErrOrgNotFound
	}
	f.connected = append(f.connected, installation)
	return nil
}

type fakeInstallations map[int64]string

func (f fakeInstallations) Installation(_ context.Context, id int64) (*github.Installation, error) {
	login, ok := f[id]
	if !ok {
		return nil, github.ErrInstallationGone
	}
	return &github.Installation{ID: id, AccountLogin: login, AccountType: github.AccountOrganization}, nil
}

func TestGitHubConnectRecordsOnlyWhatGitHubConfirms(t *testing.T) {
	ctx := context.Background()
	app := fakeInstallations{100: "Acme"}
	store := &fakeInstallationStore{orgs: map[string]bool{"org-1": true}}
	var out bytes.Buffer
	if err := connectGitHub(ctx, store, app, "org-1", 100, &out); err != nil {
		t.Fatalf("connect: %v", err)
	}
	want := domain.OrgGitHubInstallation{OrgID: "org-1", InstallationID: 100, AccountLogin: "Acme", AccountType: github.AccountOrganization, ConnectedActor: domain.Actor{Kind: domain.ActorStaff}}
	if len(store.connected) != 1 || *store.connected[0] != want {
		t.Fatalf("connected = %+v, want %+v with the account GitHub stated", store.connected, want)
	}
	if !strings.Contains(out.String(), `"account_login":"Acme"`) || !strings.Contains(out.String(), `"installation_id":100`) {
		t.Fatalf("output = %q, want the installation as it was recorded", out.String())
	}

	for name, tc := range map[string]struct {
		org          string
		installation int64
		want         error
	}{
		"an org that does not exist":            {"org-missing", 100, db.ErrOrgNotFound},
		"an installation that is not the App's": {"org-1", 999, github.ErrInstallationGone},
	} {
		store.connected, out = nil, bytes.Buffer{}
		err := connectGitHub(ctx, store, app, tc.org, tc.installation, &out)
		if !errors.Is(err, tc.want) || len(store.connected) != 0 || out.Len() != 0 {
			t.Errorf("%s: got = %v, connected %+v, want %v and nothing recorded", name, err, store.connected, tc.want)
		}
	}
}

func TestGitHubConnectNeedsItsNumberAndAnApp(t *testing.T) {
	inSettings(t)
	var stdout bytes.Buffer
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the command it replaced":         {[]string{"repo", "attach", "prj_1", "100", "7"}, "usage: hl-admin"},
		"an installation that is a name":  {[]string{"github", "connect", "org-1", "acme"}, "INSTALLATION_ID"},
		"no installation":                 {[]string{"github", "connect", "org-1"}, "usage: hl-admin"},
		"a deployment with no GitHub App": {[]string{"github", "connect", "org-1", "100"}, "git host is not github"},
	} {
		if err := run(tc.args, &stdout); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got = %v, want an error holding %q", name, err, tc.want)
		}
	}
}
