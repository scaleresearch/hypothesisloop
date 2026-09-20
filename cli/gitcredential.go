package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const gitCredentialHelp = `usage: hl git-credential get|store|erase

git's credential helper for the repository attached to your project. git runs it, not you:

  git config --global credential.https://github.com.helper '!hl git-credential'
  git config --global credential.https://github.com.useHttpPath true

On get it sends the platform the address git asked about. For the project's repository, and its
Git LFS address, the platform mints a token for that call, good for about an hour and for the one
repository, and this hands it to git. Nothing is stored, so store and erase do nothing. For any
other host or repository the platform mints nothing, this prints nothing and git carries on
without it. useHttpPath is what makes git state the repository: without it nothing is asked.`

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
	// The token is for one repository on one host, so the platform is told which one git is
	// talking to and decides before it mints anything.
	for _, key := range []string{"protocol", "host", "path"} {
		if asked[key] == "" {
			fmt.Fprintf(stderr, "hl: git-credential: git did not state the %s. Set credential.https://github.com.useHttpPath to true so that it names the repository\n", key)
			return 1
		}
	}
	var credential struct {
		Username  string    `json:"username"`
		Password  string    `json:"password"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	query := url.Values{"protocol": {asked["protocol"]}, "host": {asked["host"]}, "path": {asked["path"]}}
	if err := c.Do("GET", "/git-credential?"+query.Encode(), nil, &credential); err != nil {
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
