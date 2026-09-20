package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const gitCredentialHelp = `usage: hl git-credential get|store|erase

git's credential helper for the repository attached to your project. git runs it, not you:

  git config --global credential.https://github.com.helper '!hl git-credential'
  git config --global credential.https://github.com.useHttpPath true

On get it asks the platform for a token minted for that call, good for about an hour and for the
one repository, and hands it to git. Nothing is stored, so store and erase do nothing. It answers
only for the repository the platform names: asked about any other host or repository it prints
nothing and git carries on without it.`

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
	var credential struct {
		Protocol  string    `json:"protocol"`
		Host      string    `json:"host"`
		Path      string    `json:"path"`
		Username  string    `json:"username"`
		Password  string    `json:"password"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.Do("GET", "/git-credential", nil, &credential); err != nil {
		return fail(stderr, err)
	}
	if credential.Host == "" || credential.Path == "" || credential.Username == "" || credential.Password == "" {
		fmt.Fprintln(stderr, "hl: git-credential: the platform answered without a host, a path, a username or a password")
		return 1
	}
	// The token is for one repository on one host. Handing it to whatever git happens to be
	// talking to would send it to a submodule's or a dependency's server.
	if asked["protocol"] != credential.Protocol || !strings.EqualFold(asked["host"], credential.Host) {
		fmt.Fprintf(stderr, "hl: git-credential: no credential for %s://%s: the platform holds one for %s://%s/%s only\n",
			asked["protocol"], asked["host"], credential.Protocol, credential.Host, credential.Path)
		return 0
	}
	if path, sent := asked["path"]; sent && !strings.EqualFold(repositoryPath(path), repositoryPath(credential.Path)) {
		fmt.Fprintf(stderr, "hl: git-credential: no credential for %s: the platform holds one for %s only\n", path, credential.Path)
		return 0
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", credential.Username, credential.Password)
	if !credential.ExpiresAt.IsZero() {
		fmt.Fprintf(stdout, "password_expiry_utc=%d\n", credential.ExpiresAt.Unix())
	}
	return 0
}

// repositoryPath is owner/name however git wrote it: with or without the slashes around it and
// the .git after it.
func repositoryPath(path string) string {
	return strings.TrimSuffix(strings.Trim(path, "/"), ".git")
}
