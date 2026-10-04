package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const testGitToken = "ghs_a_token_minted_for_this_test"

// platform answers POST /git-credentials as the control plane does for a platform experiment
// whose repository is Acme/Model: a token for that repository's address and its LFS address, 404
// for any other. It records each call with its body as a query string.
func platform(t *testing.T) *[]string {
	t.Helper()
	asked := &[]string{}
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		query := url.Values{}
		for key, value := range body {
			query.Set(key, value)
		}
		*asked = append(*asked, r.Method+" "+r.URL.Path+"?"+query.Encode())
		path := strings.ToLower(strings.Trim(query.Get("path"), "/"))
		w.Header().Set("Content-Type", "application/json")
		served := path == "acme/model" || path == "acme/model.git" || path == "acme/model.git/info/lfs"
		if r.Method != "POST" || r.URL.Path != "/git-credentials" || query.Get("protocol") != "https" || !strings.EqualFold(query.Get("host"), "github.com") || !served {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"reason":"not_found","message":"no git credential for that address"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"protocol":"https","host":"github.com","path":"Acme/Model.git","username":"x-access-token","password":"` + testGitToken + `","expires_at":"2099-01-01T01:00:00Z"}`)) //nolint:errcheck
	}))
	return asked
}

func gitCredential(t *testing.T, operation, asked string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	client := apiclient.New(os.Getenv("API_URL"), os.Getenv("HL_TOKEN"))
	code = cmdGitCredential(client, []string{operation}, strings.NewReader(asked), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestGitCredentialHandsGitAFreshTokenForTheProjectsRepository(t *testing.T) {
	asked := platform(t)
	for _, tc := range []struct{ name, input, query string }{
		{"the clone address", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n", "protocol=https&host=github.com&path=Acme%2FModel.git"},
		{"no .git and another casing", "protocol=https\nhost=GitHub.com\npath=acme/model\n", "protocol=https&host=GitHub.com&path=acme%2Fmodel"},
		{"capabilities and challenges around it", "capability[]=authtype\ncapability[]=state\nprotocol=https\nhost=github.com\npath=/Acme/Model.git/\nwwwauth[]=Basic realm=\"GitHub\"\n\n", "protocol=https&host=github.com&path=%2FAcme%2FModel.git%2F"},
		{"git lfs", "protocol=https\nhost=github.com\npath=Acme/Model.git/info/lfs\n\n", "protocol=https&host=github.com&path=Acme%2FModel.git%2Finfo%2Flfs"},
	} {
		code, stdout, stderr := gitCredential(t, "get", tc.input)
		want := "username=x-access-token\npassword=" + testGitToken + "\npassword_expiry_utc=4070912400\n"
		if code != 0 || stdout != want || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %q", tc.name, code, stdout, stderr, want)
		}
		wantQuery, _ := url.ParseQuery(tc.query)
		last, _ := url.Parse(strings.TrimPrefix((*asked)[len(*asked)-1], "POST "))
		if last.Path != "/git-credentials" || last.Query().Encode() != wantQuery.Encode() {
			t.Errorf("%s: asked the platform %q, want the address git stated: %s", tc.name, (*asked)[len(*asked)-1], tc.query)
		}
	}
	if len(*asked) != 4 {
		t.Fatalf("the platform was asked %d times for 4 gets: a token is minted per call and kept nowhere", len(*asked))
	}
}

// A submodule or a dependency on another server must not be sent the repository's token. git asks
// the helper about every host: the platform is told the address and has nothing for one that is
// not the repository's, and the helper then says nothing, so git goes on without a credential.
func TestGitCredentialAnswersOnlyForTheRepositoryThePlatformNames(t *testing.T) {
	asked := platform(t)
	for name, input := range map[string]string{
		"another host":               "protocol=https\nhost=evil.example\npath=Acme/Model.git\n\n",
		"a host that only starts so": "protocol=https\nhost=github.com.evil.example\npath=Acme/Model.git\n\n",
		"plain http":                 "protocol=http\nhost=github.com\npath=Acme/Model.git\n\n",
		"another repository":         "protocol=https\nhost=github.com\npath=Acme/Other.git\n\n",
		"a repository under it":      "protocol=https\nhost=github.com\npath=Acme/Model.git/extra\n\n",
	} {
		code, stdout, stderr := gitCredential(t, "get", input)
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want silence", name, code, stdout, stderr)
		}
	}
	reached := len(*asked)
	// Without the whole address there is nothing to ask about. A researcher typed one with no
	// path by hand, got no output and no help, and went on guessing.
	for name, tc := range map[string]struct{ input, lacks string }{
		"no path":           {"protocol=https\nhost=github.com\n\n", "path"},
		"an empty path":     {"protocol=https\nhost=github.com\npath=\n\n", "path"},
		"an empty protocol": {"protocol=\nhost=github.com\npath=Acme/Model.git\n\n", "protocol"},
		"no protocol":       {"host=github.com\npath=Acme/Model.git\n\n", "protocol"},
		"no host":           {"protocol=https\npath=Acme/Model.git\n\n", "host"},
		"nothing asked":     {"\n", "protocol"},
	} {
		code, stdout, stderr := gitCredential(t, "get", tc.input)
		if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "hl: git-credential: no "+tc.lacks+" in the address") ||
			!strings.Contains(stderr, "git calls this itself") || !strings.Contains(stderr, "git push needs no setup") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want exit 1 and one line saying git calls it", name, code, stdout, stderr)
		}
	}
	if len(*asked) != reached {
		t.Fatalf("an address git did not state in full reached the platform: %v", (*asked)[reached:])
	}
}

func TestGitCredentialStoresAndErasesNothing(t *testing.T) {
	got := platform(t)
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
		"token refused": {http.StatusUnauthorized, `{"error":"unauthenticated: job job-1 is not running"}`},
	} {
		setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body)) //nolint:errcheck
		}))
		code, stdout, stderr := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n")
		if code != 1 || stdout != "" || !strings.Contains(stderr, tc.body[10:30]) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want exit 1 with the platform's reason", name, code, stdout, stderr)
		}
	}
	// GitHub failing the mint is told as the platform's refusal of this one credential, with its
	// status and reason, and never as the platform being down.
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"reason":"github_failed","message":"GitHub failed the platform's call for GitHub App installation 7: it answered 500 \"\""}`)) //nolint:errcheck
	}))
	if code, stdout, stderr := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n"); code != 1 || stdout != "" ||
		stderr != "hl: git-credential: the platform handed out no credential for github.com/Acme/Model.git, HTTP 502 github_failed: GitHub failed the platform's call for GitHub App installation 7: it answered 500 \"\"\n" {
		t.Errorf("GitHub failing the mint: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// An agent's last turn pushes after the close: git shows the platform's one line, no HTTP dump.
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"reason":"run_over","message":"run closed, nothing more is pushed: platform experiment pe-1 is closed"}`)) //nolint:errcheck
	}))
	if code, stdout, stderr := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n"); code != 1 || stdout != "" ||
		stderr != "hl: git-credential: run closed, nothing more is pushed: platform experiment pe-1 is closed\n" {
		t.Errorf("a closed run: exit %d, stdout %q, stderr %q, want exit 1 and the platform's line", code, stdout, stderr)
	}
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"reason":"no_repository","message":"platform experiment pe-1 names no repository"}`)) //nolint:errcheck
	}))
	if code, stdout, stderr := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("a platform experiment with no repository: exit %d, stdout %q, stderr %q, want silence", code, stdout, stderr)
	}
	recordingServer(t, func(int, recorded) any { return map[string]any{"protocol": "https", "host": "github.com"} })
	if code, stdout, _ := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n"); code != 1 || stdout != "" {
		t.Errorf("an answer without a password: exit %d, stdout %q, want exit 1 and nothing for git", code, stdout)
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
	platform(t)
	helper := "!" + helperProcessEnv + "=1 " + os.Args[0] + " -test.run=TestGitCredentialHelperProcess --"
	fill := func(url string) (string, error) {
		cmd := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper="+helper,
			"-c", "credential.useHttpPath=true", "credential", "fill")
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
	if out, err := fill("https://github.com/Acme/Model.git/info/lfs"); err != nil || !strings.Contains(out, "password="+testGitToken+"\n") {
		t.Fatalf("git credential fill for the repository's LFS address: %v\n%s", err, out)
	}
	for _, address := range []string{"https://github.com/Acme/Other.git", "https://evil.example/Acme/Model.git"} {
		if out, err := fill(address); err == nil || strings.Contains(out, testGitToken) {
			t.Fatalf("git credential fill for %s got a credential:\n%s", address, out)
		}
	}
}
