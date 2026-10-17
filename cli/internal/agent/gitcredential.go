package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const gitCredentialHelp = `usage: hl git-credential get|store|erase

git's credential helper for the platform's git proxy, which serves the repository your platform
experiment names at its git_url (hl platform-experiments get). git runs it, not you, and where it
is set up git push, git fetch and hl job submit need nothing more:

  git config --global credential.helper '!hl git-credential'
  git config --global credential.useHttpPath true
  git config --global url.<git_url>.insteadOf <repository_url>

with HL_GIT_URL set to git_url. On get, for that address, it hands git your own platform token,
HL_TOKEN, as the password: the proxy takes it, decides what you may fetch and push, and reaches
the git host itself, so no credential of the git host is ever handed to you. For any other
address it prints nothing and git carries on without it. Nothing is stored, so store and erase do
nothing.`

func GitCredential(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return credentialHelper(c, os.Getenv("HL_GIT_URL"), args, os.Stdin, stdout, stderr)
}

// credentialHelper speaks git's credential helper protocol: attributes as key=value lines on
// stdin, and for get the credential the same way on stdout. gitURL is the proxy's address of the
// repository, the one address it answers for.
func credentialHelper(c *apiclient.Client, gitURL string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprintln(stdout, gitCredentialHelp)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, gitCredentialHelp)
		return 2
	}
	// git tells every helper about store and erase, and may add operations: one that is not get
	// is not for this helper.
	if args[0] != "get" {
		return 0
	}
	asked := map[string]string{}
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		if scanner.Text() == "" {
			break
		}
		if key, value, ok := strings.Cut(scanner.Text(), "="); ok {
			asked[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "hl: git-credential: reading what git asked: %s\n", err)
		return 1
	}
	proxy, err := url.Parse(gitURL)
	if gitURL == "" || err != nil || proxy.Host == "" {
		fmt.Fprintf(stderr, "hl: git-credential: HL_GIT_URL %q is not the git_url of your platform experiment, the one address this answers for\n", gitURL)
		return 1
	}
	// git with credential.useHttpPath states all three, so an address without one is typed by
	// hand or comes from a git that is not set up.
	for _, key := range []string{"protocol", "host", "path"} {
		if asked[key] == "" {
			fmt.Fprintf(stderr, "hl: git-credential: no %s in the address asked about. git calls this itself with protocol, host and path, and git push needs no setup: run nothing by hand. A git that leaves the path out lacks credential.useHttpPath true\n", key)
			return 1
		}
	}
	repository := func(path string) string { return strings.TrimSuffix(strings.Trim(path, "/"), ".git") }
	if asked["protocol"] != proxy.Scheme || !strings.EqualFold(asked["host"], proxy.Host) || repository(asked["path"]) != repository(proxy.Path) {
		return 0
	}
	if c.Token == "" {
		fmt.Fprintln(stderr, "hl: git-credential: HL_TOKEN is not set, and the git proxy takes your platform token")
		return 1
	}
	fmt.Fprintf(stdout, "username=hl\npassword=%s\n", c.Token)
	return 0
}
