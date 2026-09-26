package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

var codeCommands = []subcommand{
	{"push", "store your committed HEAD with the platform, prints its code_ref", codePush},
	{"fetch", "download the bundle a platform code_ref names; hl-clone runs it", codeFetch},
}

// maxCodeBytes is domain.MaxAgentCodeBytes: what the platform keeps of one commit.
const maxCodeBytes = 32 << 20

// A code_ref of a platform experiment that has no repository: the address the platform keeps
// the commit's bundle at, with @sha in place of /sha. Only the path says so. The host is
// whichever address the writer reached the API at, and a reader uses its own.
var platformCodeRef = regexp.MustCompile(`/platform-experiments/([^/@]+)/agents/([^/@]+)/code@([0-9a-f]{40})$`)

func codePath(platformExperimentID, agentID, sha string) string {
	return "/platform-experiments/" + platformExperimentID + "/agents/" + agentID + "/code/" + sha
}

func codeRef(apiURL, platformExperimentID, agentID, sha string) string {
	return strings.TrimRight(apiURL, "/") + "/platform-experiments/" + platformExperimentID + "/agents/" + agentID + "/code@" + sha
}

// codeStored reports whether the platform holds the agent's bundle of sha.
func codeStored(c *apiclient.Client, platformExperimentID, agentID, sha string) (bool, error) {
	_, err := c.DoBytes(http.MethodGet, codePath(platformExperimentID, agentID, sha), nil)
	var answered *apiclient.HTTPError
	if errors.As(err, &answered) && answered.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func codePush(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("code push", `usage: hl code push

For a platform experiment that has no repository: stores the commit at HEAD of the git
repository in the current directory with the platform, as one git bundle with its history, and
prints the code_ref a job of that commit carries. hl job submit fills that code_ref in by
itself once the commit is pushed, and hl-clone fetches it, in a job and on your own machine.
Tracked files must have no uncommitted changes. At most 32 MiB: keep data and build output out
of git. With a repository, git push is what you want and this is not.`, stderr)
	agent := fs.String("agent", "", "whose code it is (default $AGENT_ID)")
	pe := fs.String("platform-experiment", "", "default $PLATFORM_EXPERIMENT_ID")
	if _, err := parseOperands(fs, args); err != nil {
		return parseExit(err)
	}
	writer, err := writingAgent(c, *agent)
	if err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	experiment := orEnv(*pe, platformExperimentEnv)
	if writer == "" || experiment == "" {
		return parseExit(misuse(fs, "--agent and --platform-experiment are required (or $AGENT_ID and $PLATFORM_EXPERIMENT_ID)"))
	}
	sha, err := committedHead("")
	if err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	dir, err := os.MkdirTemp("", "hl-code-")
	if err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "code.bundle")
	if _, err := git("", "bundle", "create", "-q", file, "HEAD"); err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	bundle, err := os.ReadFile(file)
	if err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	if len(bundle) > maxCodeBytes {
		return fail(stderr, fmt.Errorf("code push: the repository is %d MiB as a bundle and the platform keeps at most %d MiB of a commit: take data, checkpoints and build output out of git (and out of its history)", len(bundle)>>20, maxCodeBytes>>20))
	}
	if _, err := c.DoBytes(http.MethodPut, codePath(experiment, writer, sha), bundle); err != nil {
		return fail(stderr, fmt.Errorf("code push: %w", err))
	}
	fmt.Fprintln(stdout, codeRef(c.BaseURL, experiment, writer, sha))
	return 0
}

func codeFetch(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("code fetch", `usage: hl code fetch CODE_REF FILE

Writes to FILE the git bundle the platform keeps for CODE_REF, the code_ref of a job in a
platform experiment that has no repository. hl-clone runs this and checks the commit out; use
hl-clone. The bundle comes from the API this command was given ($API_URL), whatever address
the code_ref was written with.`, stderr)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) != 2 {
		return parseExit(misuse(fs, "CODE_REF and FILE are required"))
	}
	named := platformCodeRef.FindStringSubmatch(operands[0])
	if named == nil {
		return fail(stderr, fmt.Errorf("code fetch: %q is not a code_ref the platform keeps (.../platform-experiments/PE/agents/AGENT/code@SHA)", operands[0]))
	}
	bundle, err := c.DoBytes(http.MethodGet, codePath(named[1], named[2], named[3]), nil)
	if err != nil {
		return fail(stderr, fmt.Errorf("code fetch: %w", err))
	}
	if err := os.WriteFile(operands[1], bundle, 0o600); err != nil {
		return fail(stderr, fmt.Errorf("code fetch: %w", err))
	}
	return 0
}
