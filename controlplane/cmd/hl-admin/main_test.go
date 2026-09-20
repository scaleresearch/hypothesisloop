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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
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
		{"launcher", "add"}, {"launcher", "remove"}, {"launcher", "add", "main", "default"}, {"launcher", "list", "main"},
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
		{"launcher", "add", "main"}, {"launcher", "remove", "main"},
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

type fakeRepoProjects struct {
	projects map[string]bool
	attached []*domain.ProjectRepo
}

func (f *fakeRepoProjects) GetProject(_ context.Context, id string) (*domain.Project, error) {
	if !f.projects[id] {
		return nil, nil
	}
	return &domain.Project{ID: id}, nil
}

func (f *fakeRepoProjects) AttachProjectRepo(_ context.Context, repo *domain.ProjectRepo) error {
	f.attached = append(f.attached, repo)
	return nil
}

type fakeInstallations struct {
	covered map[int64]map[int64]string
	asked   []github.Access
}

func (f *fakeInstallations) Token(_ context.Context, installation, repository int64, access github.Access) (*github.Token, error) {
	f.asked = append(f.asked, access)
	repos, ok := f.covered[installation]
	if !ok {
		return nil, github.ErrInstallationGone
	}
	name, ok := repos[repository]
	if !ok {
		return nil, github.ErrRepositoryNotInstalled
	}
	return &github.Token{Value: "ghs_a_token_the_command_must_not_print", Repository: name}, nil
}

func TestRepoAttachRecordsOnlyWhatGitHubConfirms(t *testing.T) {
	ctx := context.Background()
	app := &fakeInstallations{covered: map[int64]map[int64]string{100: {7: "Acme/Model"}}}
	projects := &fakeRepoProjects{projects: map[string]bool{"prj_1": true}}
	var out bytes.Buffer
	if err := attachRepo(ctx, projects, app, "prj_1", 100, 7, &out); err != nil {
		t.Fatalf("attach: %v", err)
	}
	want := domain.ProjectRepo{ProjectID: "prj_1", InstallationID: 100, RepositoryID: 7, Repo: "Acme/Model"}
	if len(projects.attached) != 1 || *projects.attached[0] != want {
		t.Fatalf("attached = %+v, want %+v with the name GitHub stated", projects.attached, want)
	}
	if len(app.asked) != 1 || app.asked[0] != github.Read {
		t.Fatalf("GitHub was asked for %v, want one read token", app.asked)
	}
	if !strings.Contains(out.String(), `"repo":"Acme/Model"`) || strings.Contains(out.String(), "ghs_") {
		t.Fatalf("output = %q, want the repository and no token", out.String())
	}

	for name, tc := range map[string]struct {
		project                  string
		installation, repository int64
		want                     string
	}{
		"a project that does not exist":               {"prj_missing", 100, 7, "project prj_missing does not exist"},
		"an installation that is not the App's":       {"prj_1", 999, 7, github.ErrInstallationGone.Error()},
		"a repository the installation does not hold": {"prj_1", 100, 8, github.ErrRepositoryNotInstalled.Error()},
	} {
		projects.attached, out = nil, bytes.Buffer{}
		err := attachRepo(ctx, projects, app, tc.project, tc.installation, tc.repository, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(projects.attached) != 0 || out.Len() != 0 {
			t.Errorf("%s: got = %v, attached %+v, want an error holding %q and nothing recorded", name, err, projects.attached, tc.want)
		}
	}
	asked := len(app.asked)
	if err := attachRepo(ctx, projects, app, "prj_missing", 100, 7, &out); err == nil || len(app.asked) != asked {
		t.Errorf("an unknown project reached GitHub: %v", err)
	}
}

func TestRepoAttachNeedsItsNumbersAndAnApp(t *testing.T) {
	settingsPath(t)
	var stdout bytes.Buffer
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the old five-argument form":      {[]string{"repo", "attach", "prj_1", "100", "7", "acme/model", "main"}, "usage: hl-admin"},
		"an installation that is a name":  {[]string{"repo", "attach", "prj_1", "acme", "7"}, "INSTALLATION"},
		"a repository that is a name":     {[]string{"repo", "attach", "prj_1", "100", "acme/model"}, "REPOSITORY_ID"},
		"a deployment with no GitHub App": {[]string{"repo", "attach", "prj_1", "100", "7"}, "no github section"},
	} {
		if err := run(tc.args, &stdout); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got = %v, want an error holding %q", name, err, tc.want)
		}
	}
}
