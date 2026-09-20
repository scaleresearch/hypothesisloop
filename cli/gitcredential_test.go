package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const testGitToken = "ghs_a_token_minted_for_this_test"

func credentialReply(int, recorded) any {
	return map[string]any{
		"protocol": "https", "host": "github.com", "path": "Acme/Model.git",
		"username": "x-access-token", "password": testGitToken, "expires_at": "2099-01-01T01:00:00Z",
	}
}

func gitCredential(t *testing.T, operation, asked string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	client := apiclient.New(os.Getenv("API_URL"), os.Getenv("HL_TOKEN"))
	code = cmdGitCredential(client, []string{operation}, strings.NewReader(asked), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestGitCredentialHandsGitAFreshTokenForTheProjectsRepository(t *testing.T) {
	got := recordingServer(t, credentialReply)
	for name, asked := range map[string]string{
		"host alone":                 "protocol=https\nhost=github.com\n\n",
		"with the path":              "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n",
		"the path in another casing": "protocol=https\nhost=GitHub.com\npath=acme/model\n",
		"capabilities first":         "capability[]=authtype\nprotocol=https\nhost=github.com\npath=/Acme/Model.git/\nwwwauth[]=Basic realm=\"GitHub\"\n\n",
	} {
		code, stdout, stderr := gitCredential(t, "get", asked)
		want := "username=x-access-token\npassword=" + testGitToken + "\npassword_expiry_utc=4070912400\n"
		if code != 0 || stdout != want || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %q", name, code, stdout, stderr, want)
		}
	}
	if len(*got) != 4 {
		t.Fatalf("the platform was asked %d times for 4 gets: a token is minted per call and kept nowhere", len(*got))
	}
	for _, request := range *got {
		if request.Method != "GET" || request.Path != "/git-credential" || request.Query != "" {
			t.Fatalf("request = %+v", request)
		}
	}
}

// A submodule or a dependency on another server must not be sent the repository's token.
func TestGitCredentialAnswersOnlyForTheRepositoryThePlatformNames(t *testing.T) {
	recordingServer(t, credentialReply)
	for name, asked := range map[string]string{
		"another host":               "protocol=https\nhost=evil.example\n\n",
		"a host that only starts so": "protocol=https\nhost=github.com.evil.example\npath=Acme/Model.git\n\n",
		"plain http":                 "protocol=http\nhost=github.com\npath=Acme/Model.git\n\n",
		"another repository":         "protocol=https\nhost=github.com\npath=Acme/Other.git\n\n",
		"a repository under it":      "protocol=https\nhost=github.com\npath=Acme/Model.git/extra\n\n",
		"nothing asked":              "\n",
	} {
		code, stdout, stderr := gitCredential(t, "get", asked)
		if code != 0 || stdout != "" || strings.Contains(stderr, testGitToken) || !strings.Contains(stderr, "no credential for") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want nothing for git and a line that says why", name, code, stdout, stderr)
		}
	}
}

func TestGitCredentialStoresAndErasesNothing(t *testing.T) {
	got := recordingServer(t, credentialReply)
	for _, operation := range []string{"store", "erase", "an-operation-git-adds-later"} {
		code, stdout, stderr := gitCredential(t, operation, "protocol=https\nhost=github.com\nusername=x-access-token\npassword=secret\n\n")
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want silence", operation, code, stdout, stderr)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("store and erase reached the platform: %+v", *got)
	}
}

func TestGitCredentialReportsARefusalAndPrintsNoCredential(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"nothing attached": {http.StatusNotFound, `{"error":"no repository is attached to this project"}`},
		"the run is over":  {http.StatusForbidden, `{"error":"forbidden: platform experiment pe-1 is closed, and no git credential is handed out for it"}`},
		"token refused":    {http.StatusUnauthorized, `{"error":"unauthenticated: job job-1 is not running"}`},
	} {
		setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body)) //nolint:errcheck
		}))
		code, stdout, stderr := gitCredential(t, "get", "protocol=https\nhost=github.com\n\n")
		if code != 1 || stdout != "" || !strings.Contains(stderr, tc.body[10:30]) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want exit 1 with the platform's reason", name, code, stdout, stderr)
		}
	}
	recordingServer(t, func(int, recorded) any { return map[string]any{"protocol": "https", "host": "github.com"} })
	if code, stdout, _ := gitCredential(t, "get", "protocol=https\nhost=github.com\n\n"); code != 1 || stdout != "" {
		t.Errorf("an answer without a password: exit %d, stdout %q, want exit 1 and nothing for git", code, stdout)
	}
}

func TestGitCredentialUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"git-credential"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: hl git-credential") {
		t.Fatalf("no operation: exit %d, stderr %q", code, stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"git-credential", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "credential.https://github.com.helper") {
		t.Fatalf("--help: exit %d, stdout %q", code, stdout.String())
	}
	stdout.Reset()
	if run([]string{"--help"}, &stdout, &stderr); !strings.Contains(stdout.String(), "git-credential") {
		t.Fatal("hl --help does not list git-credential")
	}
}

const helperProcessEnv = "HL_TEST_GIT_CREDENTIAL_HELPER"

// TestGitCredentialHelperProcess is the helper git runs in TestGitItselfTakesTheCredential: the
// test binary acting as hl.
func TestGitCredentialHelperProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) == "" {
		t.Skip("run by git as a credential helper")
	}
	os.Exit(run([]string{"git-credential", os.Args[len(os.Args)-1]}, os.Stdout, os.Stderr))
}

// The protocol is git's, so git is what checks it: configured as the images configure it, git
// asks the helper and comes back with the token for the project's repository and with nothing
// for another repository or host.
func TestGitItselfTakesTheCredential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	recordingServer(t, credentialReply)
	helper := "!" + helperProcessEnv + "=1 " + os.Args[0] + " -test.run=TestGitCredentialHelperProcess --"
	fill := func(url string) (string, error) {
		cmd := exec.Command("git", "-c", "credential.helper=", "-c", "credential.https://github.com.helper="+helper,
			"-c", "credential.https://github.com.useHttpPath=true", "credential", "fill")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_ASKPASS=")
		cmd.Stdin = strings.NewReader("url=" + url + "\n\n")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Logf("git credential fill %s: %s", url, stderr.String())
		}
		return string(out), err
	}
	out, err := fill("https://github.com/Acme/Model.git")
	if err != nil || !strings.Contains(out, "username=x-access-token\n") || !strings.Contains(out, "password="+testGitToken+"\n") {
		t.Fatalf("git credential fill for the project's repository: %v\n%s", err, out)
	}
	for _, url := range []string{"https://github.com/Acme/Other.git", "https://evil.example/Acme/Model.git"} {
		if out, err := fill(url); err == nil || strings.Contains(out, testGitToken) {
			t.Fatalf("git credential fill for %s got a credential:\n%s", url, out)
		}
	}
}
