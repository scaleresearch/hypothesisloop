package github

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

// The live checks talk to github.com as the real App and run only when asked to:
//
//	HYPOTHESISLOOP_GITHUB_LIVE_APP_ID         the App's id
//	HYPOTHESISLOOP_GITHUB_LIVE_KEY_FILE       a file holding its private key
//	HYPOTHESISLOOP_GITHUB_LIVE_INSTALLATION   an installation of it, for the token checks
//	HYPOTHESISLOOP_GITHUB_LIVE_REPOSITORY     owner/name of a repository that installation covers
//	HYPOTHESISLOOP_GITHUB_LIVE_PUSH_REPOSITORY  owner/name of a sandbox repository the push check
//	                                          may push a branch to, which it deletes again
//
// Connecting an installation needs a person's authorisation code from a browser, so the code
// exchange, GET /user/installations with a person's token and its revocation have no check here.
func liveClient(t *testing.T) *Client {
	t.Helper()
	appID, keyFile := os.Getenv("HYPOTHESISLOOP_GITHUB_LIVE_APP_ID"), os.Getenv("HYPOTHESISLOOP_GITHUB_LIVE_KEY_FILE")
	if appID == "" || keyFile == "" {
		t.Skip("HYPOTHESISLOOP_GITHUB_LIVE_APP_ID and HYPOTHESISLOOP_GITHUB_LIVE_KEY_FILE are not set")
	}
	id, err := strconv.ParseInt(appID, 10, 64)
	if err != nil {
		t.Fatalf("HYPOTHESISLOOP_GITHUB_LIVE_APP_ID is not a number")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	client, err := New(config.GitHubConfig{AppID: id, AppSlug: "live", ClientID: "unused", ClientSecret: "unused", PrivateKey: string(key)})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func liveID(t *testing.T, name string) int64 {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not set", name)
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("%s is not a number", name)
	}
	return id
}

func TestLiveGitHubTakesTheAppJWT(t *testing.T) {
	client := liveClient(t)
	jwt, err := client.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := client.call(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		t.Fatalf("GET /app as the App: %v", err)
	}
	if app.ID != client.appID || app.Permissions["contents"] != "write" || app.Permissions["metadata"] != "read" {
		t.Fatalf("app = %+v, want id %d with contents write and metadata read", app, client.appID)
	}
}

func TestLiveGitHubRefusalsReadAsTheClientNamesThem(t *testing.T) {
	client := liveClient(t)
	if _, err := client.Token(ctx, 1, "nobody/nothing", Read); !errors.Is(err, ErrInstallationGone) {
		t.Errorf("a token for an installation that is not this App's: got = %v, want %v", err, ErrInstallationGone)
	}
	client.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	if _, err := client.Token(ctx, 1, "nobody/nothing", Read); !errors.Is(err, ErrAppCredentials) {
		t.Errorf("a token asked for with a key that is not the App's: got = %v, want %v", err, ErrAppCredentials)
	}
}

// Read only: it mints read tokens and reads with them, and writes to no repository.
func TestLiveInstallationTokenIsLimitedToOneRepository(t *testing.T) {
	client := liveClient(t)
	installation := liveID(t, "HYPOTHESISLOOP_GITHUB_LIVE_INSTALLATION")
	name := os.Getenv("HYPOTHESISLOOP_GITHUB_LIVE_REPOSITORY")
	if ValidRepository(name) != nil {
		t.Skip("HYPOTHESISLOOP_GITHUB_LIVE_REPOSITORY is not set to owner/name")
	}
	owner, repoName, _ := strings.Cut(name, "/")
	found, err := client.Installation(ctx, installation)
	if err != nil || found.ID != installation || !strings.EqualFold(found.AccountLogin, owner) {
		t.Fatalf("Installation = %+v, %v, want the one on the account that owns %s", found, err, name)
	}
	if _, err := client.Installation(ctx, 1); !errors.Is(err, ErrInstallationGone) {
		t.Errorf("an installation that is not this App's: got = %v, want %v", err, ErrInstallationGone)
	}

	all, err := client.Repositories(ctx, installation)
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	listed, sibling := false, ""
	for _, repo := range all {
		switch {
		case strings.EqualFold(repo.FullName, name):
			listed = true
		case repo.Private && sibling == "":
			sibling = repo.FullName
		}
	}
	if !listed {
		t.Fatalf("the installation does not list the repository among its %d", len(all))
	}

	token, err := client.Token(ctx, installation, strings.ToUpper(name), Read)
	if err != nil {
		t.Fatalf("Token for the name in another casing: %v", err)
	}
	if !strings.EqualFold(token.Repository, name) || token.Repository == strings.ToUpper(name) {
		t.Errorf("the token names its repository %q, want %q as GitHub spells it", token.Repository, name)
	}
	var reached struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := client.call(ctx, http.MethodGet, "/installation/repositories", token.Value, nil, &reached); err != nil {
		t.Fatalf("list what the token reaches: %v", err)
	}
	if len(reached.Repositories) != 1 || !strings.EqualFold(reached.Repositories[0].FullName, name) {
		t.Fatalf("the token reaches %d repositories, want the one asked for", len(reached.Repositories))
	}
	var read struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.call(ctx, http.MethodGet, "/repos/"+name, token.Value, nil, &read); err != nil || read.DefaultBranch == "" {
		t.Fatalf("the token reading its own repository: default branch %q, %v", read.DefaultBranch, err)
	}
	if sibling == "" {
		t.Log("the installation has no second private repository to try the token on")
	} else if err := client.call(ctx, http.MethodGet, "/repos/"+sibling, token.Value, nil, &read); !errors.Is(err, errNotFound) {
		t.Errorf("the token reading a private sibling on the same installation: got = %v, want not found", err)
	}

	if _, err := client.Token(ctx, installation, owner+"/a-repository-nobody-has-made-7c1f", Read); !errors.Is(err, ErrRepositoryNotInstalled) {
		t.Errorf("a token for a repository outside the installation: got = %v, want %v", err, ErrRepositoryNotInstalled)
	}
	// The installation names repositories without their owner: another owner's is refused here.
	if _, err := client.Token(ctx, installation, "an-owner-that-is-not-this-one-7c1f/"+repoName, Read); !errors.Is(err, ErrRepositoryNotInstalled) {
		t.Errorf("a token for the same name under another owner: got = %v, want %v", err, ErrRepositoryNotInstalled)
	}
	if _, err := client.File(ctx, token, "", "a-path-no-repository-holds.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("a file the repository does not hold: got = %v, want %v", err, ErrFileNotFound)
	}
	if _, err := client.File(ctx, token, "", "README.md"); err != nil && !errors.Is(err, ErrFileNotFound) {
		t.Errorf("reading README.md at the default branch: %v", err)
	}
	// A permission the App does not hold is refused with the message Token reads.
	jwt, err := client.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	var out map[string]any
	body := map[string]any{"repositories": []string{repoName}, "permissions": map[string]string{"issues": "write"}}
	err = client.call(ctx, http.MethodPost, path, jwt, body, &out)
	var refused *refusal
	if !errors.Is(err, errUnprocessable) || !errors.As(err, &refused) || !strings.Contains(strings.ToLower(refused.message), "permission") {
		t.Errorf("a permission the App lacks: got = %v, want a 422 whose message names permissions", err)
	}
	body = map[string]any{"repositories": []string{"a-repository-nobody-has-made-7c1f"}, "permissions": map[string]string{"metadata": "read"}}
	err = client.call(ctx, http.MethodPost, path, jwt, body, &out)
	if !errors.Is(err, errUnprocessable) || !errors.As(err, &refused) || strings.Contains(strings.ToLower(refused.message), "permission") {
		t.Errorf("a repository outside the installation: got = %v, want a 422 whose message does not name permissions", err)
	}
}

// liveGit runs git with token as the password git is given, the way hl git-credential gives it.
// The token travels in the environment of the child and appears in no argument and no output.
func liveGit(t *testing.T, dir, token string, args ...string) (string, error) {
	t.Helper()
	helper := `!f() { echo username=x-access-token; echo "password=$HL_LIVE_GIT_TOKEN"; }; f`
	cmd := exec.Command("git", append([]string{"-c", "credential.helper=", "-c", "credential.https://github.com.helper=" + helper}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HL_LIVE_GIT_TOKEN="+token, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=hl live check", "GIT_AUTHOR_EMAIL=live@hypothesisloop.local", "GIT_COMMITTER_NAME=hl live check", "GIT_COMMITTER_EMAIL=live@hypothesisloop.local")
	out, err := cmd.CombinedOutput()
	return strings.ReplaceAll(string(out), token, "<token>"), err
}

// A read token clones and cannot push. A write token pushes a branch, and the branch is deleted.
func TestLiveReadTokenClonesAndOnlyAWriteTokenPushes(t *testing.T) {
	client := liveClient(t)
	installation := liveID(t, "HYPOTHESISLOOP_GITHUB_LIVE_INSTALLATION")
	name := os.Getenv("HYPOTHESISLOOP_GITHUB_LIVE_PUSH_REPOSITORY")
	if ValidRepository(name) != nil {
		t.Skip("HYPOTHESISLOOP_GITHUB_LIVE_PUSH_REPOSITORY is not set to owner/name")
	}
	read, err := client.Token(ctx, installation, name, Read)
	if err != nil {
		t.Fatal(err)
	}
	write, err := client.Token(ctx, installation, name, Write)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if out, err := liveGit(t, dir, read.Value, "clone", "--depth", "1", CloneURL(name), "work"); err != nil {
		t.Fatalf("clone with the read token: %v\n%s", err, out)
	}
	if out, err := liveGit(t, dir, "not-a-token", "clone", "--depth", "1", CloneURL(name), "refused"); err == nil {
		t.Fatalf("the sandbox cloned without a token, so it proves nothing about one:\n%s", out)
	}
	work := filepath.Join(dir, "work")
	suffix := make([]byte, 4)
	rand.Read(suffix) //nolint:errcheck
	branch := fmt.Sprintf("hl/live-check/%x", suffix)
	if err := os.WriteFile(filepath.Join(work, "live-check.txt"), []byte("pushed by the opt-in live check\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"checkout", "-q", "-b", branch}, {"add", "live-check.txt"}, {"commit", "-q", "-m", "live check"}} {
		if out, err := liveGit(t, work, "", args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if out, err := liveGit(t, work, read.Value, "push", "origin", branch); err == nil {
		liveGit(t, work, write.Value, "push", "origin", "--delete", branch) //nolint:errcheck
		t.Fatalf("the read token pushed a branch:\n%s", out)
	}
	if out, err := liveGit(t, work, write.Value, "push", "origin", branch); err != nil {
		t.Fatalf("push with the write token: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := liveGit(t, work, write.Value, "push", "origin", "--delete", branch); err != nil {
			t.Errorf("delete branch %s from the sandbox: %v\n%s", branch, err, out)
		}
	})
	if out, err := liveGit(t, work, read.Value, "ls-remote", "--heads", "origin", branch); err != nil || !strings.Contains(out, branch) {
		t.Fatalf("the pushed branch is not on the remote: %v\n%s", err, out)
	}
}
