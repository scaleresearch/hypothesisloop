package githost

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// projects is prj-a of org-a and prj-b of org-b.
type projects struct{}

func (projects) GetProject(_ context.Context, id string) (*domain.Project, error) {
	org, ok := map[string]string{"prj-a": "org-a", "prj-b": "org-b"}[id]
	if !ok {
		return nil, nil
	}
	return &domain.Project{ID: id, OrgID: org}, nil
}

// gitHost serves the bare repositories under a temp dir with git's own HTTP backend, asking for
// the credential when one is given, and creating a repository on its first push. It returns the
// host's address.
func gitHost(t *testing.T, username, password string, repositories ...string) string {
	address, _ := gitHostAt(t, username, password, repositories...)
	return address
}

func gitHostAt(t *testing.T, username, password string, repositories ...string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	for _, name := range repositories {
		if out, err := exec.Command("git", "init", "--bare", "--quiet", filepath.Join(root, name)).CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", name, err, out)
		}
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=test"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if given, secret, _ := r.BasicAuth(); username != "" && (given != username || secret != password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if repository, ok := strings.CutSuffix(r.URL.Path, "/info/refs"); ok && r.URL.Query().Get("service") == "git-receive-pack" {
			if _, err := os.Stat(filepath.Join(root, repository)); err != nil {
				if out, err := exec.Command("git", "init", "--bare", "--quiet", filepath.Join(root, repository)).CombinedOutput(); err != nil {
					t.Errorf("git init %s: %v\n%s", repository, err, out)
				}
			}
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, root
}

func refused(err error) (int, string) {
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refused.GetStatus(), refused.Reason
	}
	return 0, ""
}

func newPlain(t *testing.T, baseURL, username, password string) *Plain {
	t.Helper()
	plain, err := NewPlain(baseURL, username, password, "experiment-template", projects{})
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestAPlainRepositoryIsANameUnderTheOrgsDirectoryThatAnswersGit(t *testing.T) {
	address := gitHost(t, "", "", "org-a/model.git", "org-a/team/sweep")
	plain := newPlain(t, address+"/", "", "")
	ctx := context.Background()
	for name, want := range map[string]string{"model.git": "org-a/model.git", "team/sweep": "org-a/team/sweep", "org-a/model.git": "org-a/model.git"} {
		if named, err := plain.Reachable(ctx, "prj-a", name); err != nil || named != want {
			t.Errorf("Reachable(%s) = %q, %v, want %s", name, named, err, want)
		}
		if got := plain.URL(want); got != address+"/"+want {
			t.Errorf("URL(%s) = %q, want it under the root", want, got)
		}
		if out, err := exec.Command("git", "ls-remote", plain.URL(want)).CombinedOutput(); err != nil {
			t.Errorf("git ls-remote %s: %v\n%s", plain.URL(want), err, out)
		}
	}
	for name, tc := range map[string]struct{ repository, reason string }{
		"a name nothing is under":     {"missing.git", "repository_not_reachable"},
		"a directory that is no repo": {"team", "repository_not_reachable"},
		"the org's directory":         {"org-a", "repository_not_reachable"},
		"a path out of the root":      {"../model.git", "invalid_repository"},
		"an address":                  {address + "/model.git", "invalid_repository"},
		"an option for git":           {"-model", "invalid_repository"},
		"no name":                     {"", "invalid_repository"},
	} {
		named, err := plain.Reachable(ctx, "prj-a", tc.repository)
		if status, reason := refused(err); named != "" || status != 422 || reason != tc.reason {
			t.Errorf("%s: Reachable = %q, %v, want 422 %s", name, named, err, tc.reason)
		}
	}
	if _, err := plain.Reachable(ctx, "prj-gone", "model.git"); err == nil {
		t.Error("a project with no row reached a repository")
	}
	gone := newPlain(t, "http://127.0.0.1:1", "", "")
	_, err := gone.Reachable(ctx, "prj-a", "model.git")
	if status, reason := refused(err); status != 422 || reason != "repository_not_reachable" {
		t.Errorf("a host that does not answer: %v, want 422 repository_not_reachable", err)
	}
}

func TestAPlainHostIsReachedWithTheDeploymentsOneCredential(t *testing.T) {
	const password = "s3cret-of-the-deployment"
	address := gitHost(t, "platform", password, "org-a/model.git")
	ctx := context.Background()
	if named, err := newPlain(t, address, "platform", password).Reachable(ctx, "prj-a", "model.git"); err != nil || named != "org-a/model.git" {
		t.Fatalf("with the credential: Reachable = %q, %v", named, err)
	}
	for name, plain := range map[string]*Plain{
		"no credential":    newPlain(t, address, "", ""),
		"another password": newPlain(t, address, "platform", "guess"),
	} {
		_, err := plain.Reachable(ctx, "prj-a", "model.git")
		if status, reason := refused(err); status != 422 || reason != "repository_not_reachable" || strings.Contains(err.Error(), password) {
			t.Errorf("%s: %v, want 422 repository_not_reachable", name, err)
		}
	}
	plain := newPlain(t, address, "platform", password)
	for _, write := range []bool{false, true} {
		credential, err := plain.Credential(ctx, "prj-a", "org-a/model.git", write)
		if err != nil || credential.Username != "platform" || credential.Password != password || !credential.ExpiresAt.IsZero() {
			t.Errorf("Credential(write %v) = %+v, %v, want the deployment's, which does not run out", write, credential, err)
		}
	}
	_, err := newPlain(t, address, "", "").Credential(ctx, "prj-a", "org-a/model.git", true)
	if status, reason := refused(err); status != 404 || reason != "not_found" {
		t.Errorf("a host that takes no credential: %v, want 404 not_found", err)
	}
}

// The host knows no orgs, so another org's repository is out of reach whatever names it: a name a
// request gives is read under the org's own directory, and every access to a repository outside
// it is refused, the credential first.
func TestAnOrgReachesNoRepositoryOfAnotherOnAPlainHost(t *testing.T) {
	const password = "s3cret-of-the-deployment"
	address := gitHost(t, "platform", password, "org-a/model.git", "experiment-template")
	plain := newPlain(t, address, "platform", password)
	ctx := context.Background()
	for _, name := range []string{"org-a/model.git", "../org-a/model.git", "experiment-template"} {
		if named, err := plain.Reachable(ctx, "prj-b", name); err == nil {
			t.Errorf("org-b reached %s as %s", name, named)
		}
	}
	for _, name := range []string{"org-a/model.git", "model.git", "experiment-template"} {
		if credential, err := plain.Credential(ctx, "prj-b", name, true); credential != nil || err == nil {
			t.Errorf("org-b was handed a credential for %s", name)
		} else if status, reason := refused(err); status != 422 || reason != "repository_not_reachable" || strings.Contains(err.Error(), password) {
			t.Errorf("Credential(%s) = %v, want 422 repository_not_reachable", name, err)
		}
		if _, err := plain.HasBranch(ctx, "prj-b", name, "main"); err == nil {
			t.Errorf("org-b read the branches of %s", name)
		}
		if _, err := plain.FileAt(ctx, "prj-b", name, "main", "environment.yaml"); errors.Is(err, ErrFileNotFound) || err == nil {
			t.Errorf("FileAt(%s) for org-b = %v, want it refused", name, err)
		}
	}
	if _, err := plain.FileAt(ctx, "prj-a", "org-a/model.git", "main", "environment.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("FileAt of the org's own repository = %v, want ErrFileNotFound", err)
	}
}

func TestAPlainRepositoryIsCreatedFromTheTemplateUnderTheOrgsDirectory(t *testing.T) {
	address, root := gitHostAt(t, "", "", "experiment-template")
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"}, {"commit", "--quiet", "--allow-empty", "-m", "template"},
		{"push", "--quiet", filepath.Join(root, "experiment-template"), "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	plain := newPlain(t, address, "", "")
	ctx := context.Background()
	created, branch, err := plain.Create(ctx, "prj-b", "fresh.git", "")
	if err != nil || created != "org-b/fresh.git" || branch != "main" {
		t.Fatalf("Create = %q %q, %v, want org-b/fresh.git at main", created, branch, err)
	}
	if has, err := plain.HasBranch(ctx, "prj-b", created, "main"); err != nil || !has {
		t.Fatalf("HasBranch = %v, %v, want main", has, err)
	}
	if _, _, err := plain.Create(ctx, "prj-b", created, ""); refusal.ReasonOf(err) != "repository_exists" {
		t.Fatalf("a second create = %v, want repository_exists", err)
	}
	if again, _, err := plain.Create(ctx, "prj-a", created, ""); err != nil || again != "org-a/org-b/fresh.git" {
		t.Fatalf("org-a naming org-b's repository = %q, %v, want one of its own under org-a", again, err)
	}
}

func TestAPlainHostNamesNoAccountToCreateOnAndReadsNoFile(t *testing.T) {
	plain := newPlain(t, "https://git.example/experiments", "", "")
	ctx := context.Background()
	owner, err := plain.CreatesOn(ctx, "prj-a", "")
	if status, reason := refused(err); owner != "" || status != 422 || reason != "repository_required" {
		t.Errorf("CreatesOn = %q, %v, want 422 repository_required", owner, err)
	}
	data, err := plain.File(ctx, "prj-a", "model.git", "main", "experiment.yaml")
	if status, reason := refused(err); data != nil || status != 422 || reason != "experiment_file_unsupported" {
		t.Errorf("File = %q, %v, want 422 experiment_file_unsupported", data, err)
	}
}

func TestAPlainHostServesOnlyTheRepositorysAddress(t *testing.T) {
	plain := newPlain(t, "https://git.example:8443/experiments", "", "")
	for name, tc := range map[string]struct {
		protocol, host, path string
		want                 bool
	}{
		"the clone address":         {"https", "git.example:8443", "experiments/team/model.git", true},
		"without .git":              {"https", "Git.Example:8443", "/experiments/team/model/", true},
		"git lfs":                   {"https", "git.example:8443", "experiments/team/model.git/info/lfs", true},
		"another repository":        {"https", "git.example:8443", "experiments/team/other.git", false},
		"another casing":            {"https", "git.example:8443", "experiments/Team/model.git", false},
		"a repository under it":     {"https", "git.example:8443", "experiments/team/model.git/extra", false},
		"the name outside the root": {"https", "git.example:8443", "team/model.git", false},
		"another port":              {"https", "git.example", "experiments/team/model.git", false},
		"another host":              {"https", "git.example.evil:8443", "experiments/team/model.git", false},
		"plain http":                {"http", "git.example:8443", "experiments/team/model.git", false},
	} {
		if got := plain.Serves("team/model.git", tc.protocol, tc.host, tc.path); got != tc.want {
			t.Errorf("%s: Serves = %v, want %v", name, got, tc.want)
		}
	}
	if root := newPlain(t, "https://git.example", "", ""); !root.Serves("model", "https", "git.example", "model.git") || root.URL("model") != "https://git.example/model" {
		t.Error("a root with no path does not serve the name right under it")
	}
}

func TestAPlainHostIsRefusedWithoutAWholeRootOrWithHalfACredential(t *testing.T) {
	for name, tc := range map[string][4]string{
		"no scheme":             {"git.example/experiments", "", "", "template"},
		"no host":               {"https:///experiments", "", "", "template"},
		"a credential in it":    {"https://user:pass@git.example", "", "", "template"},
		"a query":               {"https://git.example/?x=1", "", "", "template"},
		"a username alone":      {"https://git.example", "platform", "", "template"},
		"a password alone":      {"https://git.example", "", "secret", "template"},
		"nothing where it goes": {"", "", "", "template"},
		"no template":           {"https://git.example", "", "", ""},
		"a template out of it":  {"https://git.example", "", "", "../template"},
	} {
		if plain, err := NewPlain(tc[0], tc[1], tc[2], tc[3], projects{}); err == nil {
			t.Errorf("%s: NewPlain = %+v, want it refused", name, plain)
		}
	}
}
