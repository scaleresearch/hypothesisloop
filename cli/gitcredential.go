package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const gitCredentialHelp = `usage: hl git-credential get|store|erase

git's credential helper for the repository your platform experiment names. git runs it, not you,
and where it is set up git push, git fetch and hl job submit need nothing more:

  git config --global credential.helper '!hl git-credential'
  git config --global credential.useHttpPath true

On get it sends the platform the address git asked about. For the experiment's repository, and its
Git LFS address, the platform answers with the credential for that one repository and this hands
it to git. Nothing is stored, so store and erase do nothing. For any other host or repository the
platform has none, this prints nothing and git carries on without it. useHttpPath is what makes
git state the repository: an address without protocol, host or path, as git sends without it
and as one typed by hand may be, is refused with one line and nothing is handed out.`

// cmdGitCredential speaks git's credential helper protocol: attributes as key=value lines on
// stdin, and for get the credential the same way on stdout.
func cmdGitCredential(c *apiclient.Client, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	// git with credential.useHttpPath states all three, so an address without one is typed by
	// hand or comes from a git that is not set up.
	for _, key := range []string{"protocol", "host", "path"} {
		if asked[key] == "" {
			fmt.Fprintf(stderr, "hl: git-credential: no %s in the address asked about. git calls this itself with protocol, host and path, and git push needs no setup: run nothing by hand. A git that leaves the path out lacks credential.useHttpPath true\n", key)
			return 1
		}
	}
	var credential struct {
		Username  string    `json:"username"`
		Password  string    `json:"password"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	address := map[string]string{"protocol": asked["protocol"], "host": asked["host"], "path": asked["path"]}
	err := c.Do("POST", "/git-credentials", address, &credential)
	var refused *apiclient.HTTPError
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		return 0
	}
	if err != nil {
		return fail(stderr, err)
	}
	if credential.Username == "" || credential.Password == "" {
		fmt.Fprintln(stderr, "hl: git-credential: the platform answered without a username or a password")
		return 1
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", credential.Username, credential.Password)
	if !credential.ExpiresAt.IsZero() {
		fmt.Fprintf(stdout, "password_expiry_utc=%d\n", credential.ExpiresAt.Unix())
	}
	return 0
}
