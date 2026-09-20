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
// Attaching a repository needs a person's authorisation code from a browser and has no check here.
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
	if _, err := client.Token(ctx, 1, 1, Read); !errors.Is(err, ErrInstallationGone) {
		t.Errorf("a token for an installation that is not this App's: got = %v, want %v", err, ErrInstallationGone)
	}
	client.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	if _, err := client.Token(ctx, 1, 1, Read); !errors.Is(err, ErrAppCredentials) {
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
	jwt, err := client.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	var covering struct {
		ID int64 `json:"id"`
	}
	if err := client.call(ctx, http.MethodGet, "/repos/"+name+"/installation", jwt, nil, &covering); err != nil || covering.ID != installation {
		t.Fatalf("the installation covering the repository = %d, %v, want %d", covering.ID, err, installation)
	}

	// The whole installation, to learn the repository's id and find a private sibling.
	type listing struct {
		Repositories []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
		} `json:"repositories"`
	}
	var whole struct {
		Token string `json:"token"`
	}
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	if err := client.call(ctx, http.MethodPost, path, jwt, map[string]any{"permissions": map[string]string{"metadata": "read"}}, &whole); err != nil {
		t.Fatalf("mint a metadata token for the installation: %v", err)
	}
	var all listing
	if err := client.call(ctx, http.MethodGet, "/installation/repositories?per_page=100", whole.Token, nil, &all); err != nil {
		t.Fatal(err)
	}
	var repository, sibling int64
	for _, repo := range all.Repositories {
		switch {
		case strings.EqualFold(repo.FullName, name):
			repository = repo.ID
		case repo.Private && sibling == 0:
			sibling = repo.ID
		}
	}
	if repository == 0 {
		t.Fatalf("the installation does not list the repository among its %d", len(all.Repositories))
	}

	token, err := client.Token(ctx, installation, repository, Read)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	var reached listing
	if err := client.call(ctx, http.MethodGet, "/installation/repositories", token.Value, nil, &reached); err != nil {
		t.Fatalf("list what the token reaches: %v", err)
	}
	if len(reached.Repositories) != 1 || reached.Repositories[0].ID != repository {
		t.Fatalf("the token reaches %d repositories, want the one asked for", len(reached.Repositories))
	}
	var read struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.call(ctx, http.MethodGet, "/repos/"+name, token.Value, nil, &read); err != nil || read.ID != repository || read.DefaultBranch == "" {
		t.Fatalf("the token reading its own repository: id %d, default branch %q, %v", read.ID, read.DefaultBranch, err)
	}
	if sibling == 0 {
		t.Log("the installation has no second private repository to try the token on")
	} else if err := client.call(ctx, http.MethodGet, "/repositories/"+strconv.FormatInt(sibling, 10), token.Value, nil, &read); !errors.Is(err, errNotFound) {
		t.Errorf("the token reading a private sibling on the same installation: got = %v, want not found", err)
	}

	if _, err := client.Token(ctx, installation, 1, Read); !errors.Is(err, ErrRepositoryNotInstalled) {
		t.Errorf("a token for a repository outside the installation: got = %v, want %v", err, ErrRepositoryNotInstalled)
	}
	if _, err := client.File(ctx, installation, repository, "", "a-path-no-repository-holds.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("a file the repository does not hold: got = %v, want %v", err, ErrFileNotFound)
	}
	if _, err := client.File(ctx, installation, repository, "", "README.md"); err != nil && !errors.Is(err, ErrFileNotFound) {
		t.Errorf("reading README.md at the default branch: %v", err)
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
	jwt, err := client.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Token string `json:"token"`
	}
	_, repoName, _ := strings.Cut(name, "/")
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	body := map[string]any{"repositories": []string{repoName}, "permissions": map[string]string{"metadata": "read"}}
	if err := client.call(ctx, http.MethodPost, path, jwt, body, &probe); err != nil {
		t.Fatalf("mint a metadata token for the sandbox: %v", err)
	}
	var repo struct {
		ID int64 `json:"id"`
	}
	if err := client.call(ctx, http.MethodGet, "/repos/"+name, probe.Token, nil, &repo); err != nil {
		t.Fatalf("read the sandbox repository: %v", err)
	}
	read, err := client.Token(ctx, installation, repo.ID, Read)
	if err != nil {
		t.Fatal(err)
	}
	write, err := client.Token(ctx, installation, repo.ID, Write)
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
