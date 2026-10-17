package people

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

// experimentTemplateURL is the public repository every experiment's repository starts from. A
// variable so that a test clones a local repository in its place.
var experimentTemplateURL = "https://github.com/scaleresearch/hypothesisloop-experiment-template.git"

func RepoInit(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("repo init", `usage: hl repo init DIR

Clones the experiment template, `+experimentTemplateURL+`, into DIR, a
directory that does not exist or is empty, as a git repository of its own with no commit and no
remote. It holds the files every experiment's repository holds:

  .hypothesisloop/experiment.yaml   the experiment spec, with its project_id, repository and branch
  .hypothesisloop/job.yaml          the base job spec, submitted unedited for every trial
  .hypothesisloop/Dockerfile        the image jobs run in, built FROM a base image (hl image)
  pyproject.toml, uv.lock           the Python dependencies the image installs with uv
  README.md                         what is measured, how one trial runs, what to vary, what must not change
  metrics.py                        how a job reports a metric: post_metric(fraction_complete, value, "key")
  .gitignore                        bytecode caches, virtual environments and editor files

Fill in what is marked REPLACE-WITH and the experiment spec, and hl platform-experiments start DIR
creates the experiment.`, stderr)
	dir, err := command.ParseOne(fs, args, "DIR", "")
	if err != nil {
		return command.ParseExit(err)
	}
	if err := gitRun("clone", "--quiet", "--depth=1", experimentTemplateURL, dir); err != nil {
		return command.Fail(stderr, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return command.Fail(stderr, err)
	}
	if err := gitRun("-C", dir, "init", "--quiet"); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintf(stdout, "put the experiment template in %s: fill in what is marked REPLACE-WITH and %s, then hl platform-experiments start %s\n", dir, experimentspec.ExperimentFile, dir)
	return 0
}

func gitRun(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return err
}
