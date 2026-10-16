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

const (
	testGitURL   = "https://api.example.invalid/git/pe-1.git"
	testHLToken  = "hls_the_agents_own_platform_token"
	wantPlatform = "username=hl\npassword=" + testHLToken + "\n"
)

// noPlatform fails any call to the platform: the helper answers from what it holds.
func noPlatform(t *testing.T) {
	t.Helper()
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the helper called the platform: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Setenv("HL_TOKEN", testHLToken)
}

func gitCredential(t *testing.T, gitURL, operation, asked string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	client := apiclient.New(os.Getenv("API_URL"), os.Getenv("HL_TOKEN"))
	code = cmdGitCredential(client, gitURL, []string{operation}, strings.NewReader(asked), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestGitCredentialHandsTheProxyThePlatformToken(t *testing.T) {
	noPlatform(t)
	for name, input := range map[string]string{
		"the proxy's address":                    "protocol=https\nhost=api.example.invalid\npath=git/pe-1.git\n\n",
		"no .git and another casing of the host": "protocol=https\nhost=API.example.invalid\npath=git/pe-1\n",
		"capabilities and challenges around it":  "capability[]=authtype\nprotocol=https\nhost=api.example.invalid\npath=/git/pe-1.git/\nwwwauth[]=Basic realm=\"hypothesisloop\"\n\n",
	} {
		code, stdout, stderr := gitCredential(t, testGitURL, "get", input)
		if code != 0 || stdout != wantPlatform || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %q", name, code, stdout, stderr, wantPlatform)
		}
	}
}

// A submodule, a dependency or the git host itself must not be sent the platform token. git asks
// the helper about every host, and the helper says nothing for any address but the proxy's.
func TestGitCredentialAnswersOnlyForTheProxysAddress(t *testing.T) {
	noPlatform(t)
	for name, input := range map[string]string{
		"the git host":                "protocol=https\nhost=github.com\npath=Acme/Model.git\n\n",
		"another host":                "protocol=https\nhost=evil.example\npath=git/pe-1.git\n\n",
		"a host that only starts so":  "protocol=https\nhost=api.example.invalid.evil.example\npath=git/pe-1.git\n\n",
		"another port":                "protocol=https\nhost=api.example.invalid:8443\npath=git/pe-1.git\n\n",
		"plain http":                  "protocol=http\nhost=api.example.invalid\npath=git/pe-1.git\n\n",
		"another platform experiment": "protocol=https\nhost=api.example.invalid\npath=git/pe-2.git\n\n",
		"a path under the repository": "protocol=https\nhost=api.example.invalid\npath=git/pe-1.git/extra\n\n",
		"the proxy's root":            "protocol=https\nhost=api.example.invalid\npath=git\n\n",
	} {
		code, stdout, stderr := gitCredential(t, testGitURL, "get", input)
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want silence", name, code, stdout, stderr)
		}
	}
	// Without the whole address there is nothing to compare. A researcher typed one with no path
	// by hand, got no output and no help, and went on guessing.
	for name, tc := range map[string]struct{ input, lacks string }{
		"no path":           {"protocol=https\nhost=api.example.invalid\n\n", "path"},
		"an empty path":     {"protocol=https\nhost=api.example.invalid\npath=\n\n", "path"},
		"an empty protocol": {"protocol=\nhost=api.example.invalid\npath=git/pe-1.git\n\n", "protocol"},
		"no host":           {"protocol=https\npath=git/pe-1.git\n\n", "host"},
		"nothing asked":     {"\n", "protocol"},
	} {
		code, stdout, stderr := gitCredential(t, testGitURL, "get", tc.input)
		if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "hl: git-credential: no "+tc.lacks+" in the address") ||
			!strings.Contains(stderr, "git calls this itself") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want exit 1 and one line saying git calls it", name, code, stdout, stderr)
		}
	}
}

func TestGitCredentialWithoutTheProxysAddressOrATokenSaysSoAndHandsOutNothing(t *testing.T) {
	noPlatform(t)
	asked := "protocol=https\nhost=api.example.invalid\npath=git/pe-1.git\n\n"
	for _, gitURL := range []string{"", "not a url", "/git/pe-1.git"} {
		if code, stdout, stderr := gitCredential(t, gitURL, "get", asked); code != 1 || stdout != "" || !strings.Contains(stderr, "HL_GIT_URL") {
			t.Errorf("HL_GIT_URL %q: exit %d, stdout %q, stderr %q, want exit 1 naming HL_GIT_URL", gitURL, code, stdout, stderr)
		}
	}
	t.Setenv("HL_TOKEN", "")
	if code, stdout, stderr := gitCredential(t, testGitURL, "get", asked); code != 1 || stdout != "" || !strings.Contains(stderr, "HL_TOKEN") {
		t.Errorf("no token: exit %d, stdout %q, stderr %q, want exit 1 naming HL_TOKEN", code, stdout, stderr)
	}
}

func TestGitCredentialStoresAndErasesNothing(t *testing.T) {
	noPlatform(t)
	for _, operation := range []string{"store", "erase", "an-operation-git-adds-later"} {
		code, stdout, stderr := gitCredential(t, testGitURL, operation, "protocol=https\nhost=api.example.invalid\nusername=hl\npassword=secret\n\n")
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want silence", operation, code, stdout, stderr)
		}
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
// asks the helper and comes back with the platform token for the proxy's address and with nothing
// for the git host or another address.
func TestGitItselfTakesTheCredential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	noPlatform(t)
	t.Setenv("HL_GIT_URL", testGitURL)
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
	out, err := fill(testGitURL)
	if err != nil || !strings.Contains(out, "username=hl\n") || !strings.Contains(out, "password="+testHLToken+"\n") {
		t.Fatalf("git credential fill for the proxy's address: %v\n%s", err, out)
	}
	for _, address := range []string{"https://github.com/Acme/Model.git", "https://evil.example/git/pe-1.git"} {
		if out, err := fill(address); err == nil || strings.Contains(out, testHLToken) {
			t.Fatalf("git credential fill for %s got a credential:\n%s", address, out)
		}
	}
}
