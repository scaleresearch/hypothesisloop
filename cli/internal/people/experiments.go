package people

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func PlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments start", `usage: hl platform-experiments start DIR

DIR is the tree of an experiment's repository: its code and its experiment file at
DIR/.hypothesisloop/experiment.yaml, which states the experiment and where it is: project_id,
repository and branch. The file is checked first, a bad one exiting 2 before anything is asked.
The platform creates the repository from the experiment template when it does not exist, then
git, with your own credentials, makes the branch hold DIR as git add -A would commit it,
DIR/.gitignore respected and DIR's own .git untouched:

  no such branch         DIR is pushed as the branch, in one commit
  the branch holds DIR   nothing is pushed
  anything else          exits 1 and changes nothing: name a branch that does not exist, or run
                         start in a clone of the branch

Nothing is ever force-pushed. Then the platform experiment the file states is created, and start
prints its id, what it waits for and, with $HL_UI_URL set, where the UI shows it.`, stderr)
	dir, err := command.ParseOne(fs, args, "DIR", "")
	if err != nil {
		return command.ParseExit(err)
	}
	file, err := experimentspec.Load(dir)
	if err == nil {
		err = file.Spec.Validate()
	}
	if err == nil {
		err = file.Placement.Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	var repository struct {
		Repository    string `json:"repository"`
		RepositoryURL string `json:"repository_url"`
	}
	if err := c.Do("POST", "/projects/"+url.PathEscape(file.ProjectID)+"/repositories", map[string]string{"repository": file.Repository}, &repository); err != nil {
		return command.Fail(stderr, err)
	}
	file.Repository = repository.Repository
	plan, err := planPush(dir, repository.RepositoryURL, file.Branch)
	if plan != nil {
		defer os.RemoveAll(plan.gitDir)
	}
	if err != nil {
		return command.Fail(stderr, err)
	}
	switch {
	case plan.other:
		return command.Fail(stderr, fmt.Errorf("branch %s of %s holds other code than %s: name a branch that does not exist, or run start in a clone of the branch", file.Branch, file.Repository, dir))
	case plan.files > 0:
		if err := plan.push(file.Name); err != nil {
			return command.Fail(stderr, err)
		}
		fmt.Fprintf(stdout, "pushed %d files of %s to %s as %s\n", plan.files, dir, file.Repository, file.Branch)
	default:
		fmt.Fprintf(stdout, "branch %s of %s already holds %s: pushed nothing\n", file.Branch, file.Repository, dir)
	}
	var created struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		StatusReason struct {
			Summary string `json:"summary"`
		} `json:"status_reason"`
	}
	if err := c.Do("POST", "/platform-experiments", file, &created); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintf(stdout, "created %s, %s: %s\n", created.ID, created.Status, created.StatusReason.Summary)
	if ui := os.Getenv("HL_UI_URL"); ui != "" {
		fmt.Fprintln(stdout, strings.TrimSuffix(ui, "/")+"/experiments/"+created.ID)
	}
	return 0
}

// pushPlan is what makes a branch of a repository hold a directory: its tree, written in a git dir
// of its own, and the commit of it to push when the branch does not exist.
type pushPlan struct {
	gitDir, remote, branch, tree string
	// files is how many files a push carries, 0 when the branch holds the tree already.
	files int
	// other is a branch that holds something else.
	other bool
}

// planPush writes the tree of dir, as git add -A would commit it, into a git dir of its own and
// compares it with branch of remote. The caller removes the plan's git dir.
func planPush(dir, remote, branch string) (*pushPlan, error) {
	work, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	gitDir, err := os.MkdirTemp("", "hl-start-")
	if err != nil {
		return nil, err
	}
	plan := &pushPlan{gitDir: gitDir, remote: remote, branch: branch}
	for _, step := range [][]string{{"init", "--quiet"}, {"add", "-A"}} {
		if _, err := plan.git(work, step...); err != nil {
			return plan, err
		}
	}
	if plan.tree, err = plan.git(work, "write-tree"); err != nil {
		return plan, err
	}
	listed, err := plan.git(work, "ls-files")
	if err != nil {
		return plan, err
	}
	ref := "refs/heads/" + branch
	heads, err := plan.git(work, "ls-remote", remote, ref)
	if err != nil {
		return plan, fmt.Errorf("reading the branches of %s: %w", remote, err)
	}
	if heads == "" {
		plan.files = strings.Count(listed, "\n") + 1
		return plan, nil
	}
	if _, err := plan.git(work, "fetch", "--quiet", "--depth=1", remote, ref); err != nil {
		return plan, fmt.Errorf("fetching %s of %s: %w", branch, remote, err)
	}
	fetched, err := plan.git(work, "rev-parse", "FETCH_HEAD^{tree}")
	plan.other = fetched != plan.tree
	return plan, err
}

// push commits the tree and pushes it as the branch. A branch made since planPush read none
// refuses it, as git refuses any push that is no fast-forward.
func (p *pushPlan) push(name string) error {
	commit, err := p.git("", "commit-tree", p.tree, "-m", "Start "+name)
	if err != nil {
		return err
	}
	if _, err := p.git("", "push", "--quiet", p.remote, commit+":refs/heads/"+p.branch); err != nil {
		return fmt.Errorf("pushing to %s: %w", p.remote, err)
	}
	return nil
}

// git runs git on the plan's git dir, with work as its work tree when it is set.
func (p *pushPlan) git(work string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+p.gitDir)
	if work != "" {
		cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+work)
		cmd.Dir = work
	}
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
	}
	return strings.TrimSpace(string(out)), err
}

func PlatformExperimentsReopen(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments reopen", `usage: hl platform-experiments reopen [--platform-experiment PE] --ends-at TIME [--coordinator-model M]

Staff only. Sets a closed platform experiment, whose run started, running again until TIME
(RFC 3339) as the same run: its hypotheses, jobs, branches, description, baseline and agents stay. Every
agent whose run ended only with the close is at work again, and so is a sandboxed coordinator, on
--coordinator-model when it is given. An agent that finished or was cut before the close stays as
it ended.`, stderr)
	endsAt := fs.String("ends-at", "", "when the run closes again, RFC 3339")
	model := fs.String("coordinator-model", "", "the model the sandboxed coordinator runs on from now (default the one it has)")
	pe := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	if *endsAt == "" {
		return command.ParseExit(command.Misuse(fs, "want --ends-at"))
	}
	end, err := time.Parse(time.RFC3339, *endsAt)
	if err != nil {
		return command.ParseExit(command.Misuse(fs, "--ends-at %q is no RFC 3339 time, such as 2026-10-12T18:00:00Z", *endsAt))
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	body := map[string]any{"ends_at": end.Format(time.RFC3339)}
	if *model != "" {
		body["coordinator_model"] = *model
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/reopen", body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	command.PrintJSON(stdout, out)
	return 0
}
