package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

// CredentialHelperName is the name hl answers as a docker credential helper under: podman and
// docker run docker-credential-<helper>, and the agents' image links that name to hl.
const CredentialHelperName = "docker-credential-hl"

// The refusals of POST /platform-experiments/{id}/registry-credentials hl acts on: no registry on
// the deployment, and the two it waits out, the org's repository being created on its first use
// and the registry briefly not answering.
const (
	reasonNoRegistry          = "no_registry"
	reasonRegistryPreparing   = "registry_preparing"
	reasonRegistryUnavailable = "registry_unavailable"
)

var (
	preparingPoll  = 10 * time.Second
	preparingLimit = 5 * time.Minute
)

// pushLogin is a fresh push login of platform experiment pe, waited for while the registry
// prepares or does not answer.
func pushLogin(c *apiclient.Client, pe string) (*platformapi.RegistryCredential, error) {
	deadline := time.Now().Add(preparingLimit)
	for {
		var out platformapi.RegistryCredential
		err := c.Do("POST", "/platform-experiments/"+url.PathEscape(pe)+"/registry-credentials", nil, &out)
		if err == nil {
			return &out, nil
		}
		if reason, _ := command.RefusalOf(err); (reason != reasonRegistryPreparing && reason != reasonRegistryUnavailable) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(preparingPoll)
	}
}

func ImageRepository(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("image repository", `usage: hl image repository [--platform-experiment PE]

Where the platform experiment's images are pushed: tag an image <repository>:<short commit>-<label>,
push it with podman and record it with hl image set. Pushing needs no login: podman asks hl for a
fresh one. The first in an org waits a minute or two while its repository is created.`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if *pe = c.Experiment(*pe); *pe == "" || fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment is required, and nothing else"))
	}
	login, err := pushLogin(c, *pe)
	if err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintln(stdout, login.Repository)
	return 0
}

// credentialsNotFound is what a docker credential helper prints for a registry it has no login
// of, which its caller reads as none rather than as a failure.
const credentialsNotFound = "credentials not found in native keychain"

// CredentialHelper answers podman and docker as a docker credential helper: get prints a fresh
// push login of the caller's platform experiment for the platform's registry, and no login for
// any other, nor on a deployment that keeps no registry. It keeps nothing: list is empty, erase finds nothing and store is refused. An
// answer it cannot give is printed on stdout, where its caller reads it.
func CredentialHelper(c *apiclient.Client, args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stdout, "usage: %s get|list|store|erase\n", CredentialHelperName)
		return 2
	}
	switch args[0] {
	case "list":
		fmt.Fprintln(stdout, "{}")
		return 0
	case "erase":
		fmt.Fprintln(stdout, credentialsNotFound)
		return 1
	case "store":
		fmt.Fprintln(stdout, "hl keeps no registry logins: every use of the platform's registry gets a fresh one")
		return 1
	case "get":
	default:
		fmt.Fprintf(stdout, "usage: %s get|list|store|erase\n", CredentialHelperName)
		return 2
	}
	server, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(stdout, err)
		return 1
	}
	server = strings.TrimSpace(server)
	pe := c.Experiment("")
	if pe == "" {
		fmt.Fprintln(stdout, credentialsNotFound)
		return 1
	}
	login, err := pushLogin(c, pe)
	if reason, _ := command.RefusalOf(err); reason == reasonNoRegistry {
		fmt.Fprintln(stdout, credentialsNotFound)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stdout, err)
		return 1
	}
	if hostOf(server) != hostOf(login.Registry) {
		fmt.Fprintln(stdout, credentialsNotFound)
		return 1
	}
	_ = json.NewEncoder(stdout).Encode(struct {
		ServerURL string
		Username  string
		Secret    string
	}{server, login.Username, login.Password})
	return 0
}

// hostOf is the registry host a server address names, written with or without a scheme or path.
func hostOf(server string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	return strings.ToLower(host)
}
