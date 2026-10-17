package githost

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// gitHost serves the bare repositories under a temp dir with git's own HTTP backend, asking for
// the credential when one is given. It returns the host's address.
func gitHost(t *testing.T, username, password string, repositories ...string) string {
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
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if given, secret, _ := r.BasicAuth(); username != "" && (given != username || secret != password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
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
	plain, err := NewPlain(baseURL, username, password)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestAPlainRepositoryIsANameUnderTheRootThatAnswersGit(t *testing.T) {
	address := gitHost(t, "", "", "model.git", "team/sweep")
	plain := newPlain(t, address+"/", "", "")
	ctx := context.Background()
	for _, name := range []string{"model.git", "team/sweep"} {
		if named, err := plain.Reachable(ctx, "prj-a", name); err != nil || named != name {
			t.Errorf("Reachable(%s) = %q, %v, want the name", name, named, err)
		}
		if got := plain.URL(name); got != address+"/"+name {
			t.Errorf("URL(%s) = %q, want it under the root", name, got)
		}
		if out, err := exec.Command("git", "ls-remote", plain.URL(name)).CombinedOutput(); err != nil {
			t.Errorf("git ls-remote %s: %v\n%s", plain.URL(name), err, out)
		}
	}
	for name, tc := range map[string]struct{ repository, reason string }{
		"a name nothing is under":     {"missing.git", "repository_not_reachable"},
		"a directory that is no repo": {"team", "repository_not_reachable"},
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
	gone := newPlain(t, "http://127.0.0.1:1", "", "")
	_, err := gone.Reachable(ctx, "prj-a", "model.git")
	if status, reason := refused(err); status != 422 || reason != "repository_not_reachable" {
		t.Errorf("a host that does not answer: %v, want 422 repository_not_reachable", err)
	}
}

func TestAPlainHostIsReachedWithTheDeploymentsOneCredential(t *testing.T) {
	const password = "s3cret-of-the-deployment"
	address := gitHost(t, "platform", password, "model.git")
	ctx := context.Background()
	if named, err := newPlain(t, address, "platform", password).Reachable(ctx, "prj-a", "model.git"); err != nil || named != "model.git" {
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
		credential, err := plain.Credential(ctx, "prj-a", "model.git", write)
		if err != nil || credential.Username != "platform" || credential.Password != password || !credential.ExpiresAt.IsZero() {
			t.Errorf("Credential(write %v) = %+v, %v, want the deployment's, which does not run out", write, credential, err)
		}
	}
	_, err := newPlain(t, address, "", "").Credential(ctx, "prj-a", "model.git", true)
	if status, reason := refused(err); status != 404 || reason != "not_found" {
		t.Errorf("a host that takes no credential: %v, want 404 not_found", err)
	}
}

func TestAPlainHostCreatesNoRepositoryAndReadsNoFile(t *testing.T) {
	plain := newPlain(t, "https://git.example/experiments", "", "")
	ctx := context.Background()
	named, err := plain.Create(ctx, "prj-a", "", "sweep-1a2b3c4d", "", map[string]string{"README.md": "x"})
	if status, reason := refused(err); named != "" || status != 422 || reason != "repository_required" {
		t.Errorf("Create = %q, %v, want 422 repository_required", named, err)
	}
	data, err := plain.File(ctx, "prj-a", "model.git", "experiment.yaml")
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
	for name, tc := range map[string][3]string{
		"no scheme":             {"git.example/experiments", "", ""},
		"no host":               {"https:///experiments", "", ""},
		"a credential in it":    {"https://user:pass@git.example", "", ""},
		"a query":               {"https://git.example/?x=1", "", ""},
		"a username alone":      {"https://git.example", "platform", ""},
		"a password alone":      {"https://git.example", "", "secret"},
		"nothing where it goes": {"", "", ""},
	} {
		if plain, err := NewPlain(tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("%s: NewPlain = %+v, want it refused", name, plain)
		}
	}
}
