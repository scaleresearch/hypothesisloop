package main

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
)

// createBody is the body of POST /platform-experiments: the experiment spec and where it is placed.
type createBody struct {
	experimentspec.Spec
	ProjectID         string     `json:"project_id"`
	Repository        string     `json:"repository,omitempty"`
	RepositoryAccount string     `json:"repository_account,omitempty"`
	Continues         string     `json:"continues,omitempty"`
	StartsAt          *time.Time `json:"starts_at,omitempty"`
}

// projectOf is the project a command names with --project, or $HL_PROJECT, a misuse with neither.
func projectOf(fs *flagSet, flagValue string) (string, error) {
	if project := orEnv(flagValue, projectEnv); project != "" {
		return project, nil
	}
	return "", misuse(fs, "a project is required: --project or $HL_PROJECT")
}

// readSpec is the valid experiment spec of path, or of stdin for "-".
func readSpec(path string) (experimentspec.Spec, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return experimentspec.Spec{}, fmt.Errorf("reading %s: %w", path, err)
	}
	spec, err := experimentspec.Parse(data)
	if err == nil {
		err = spec.Validate()
	}
	if err != nil {
		return spec, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

func cmdPlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments start", `usage: hl platform-experiments start --repository R [--project PROJECT] [--dry-run] DIR

DIR is the tree of an experiment's repository, as each directory of examples/ is: its code and
its spec at DIR/.hypothesisloop/experiment.yaml, which is checked first, a bad one exiting 2
before anything is asked. R is the repository the experiment works on: owner/name on GitHub,
which a GitHub account connected to the org covers, or on any other git host a name created
there beforehand. The platform checks it can reach R, and git, with your own credentials, makes
R's default branch hold DIR as git add -A would commit it, DIR/.gitignore respected and DIR's own
.git untouched:

  R has no branch      DIR is pushed as R's main, in one commit
  R's default branch   holds DIR already: nothing is pushed
  anything else        exits 1 and changes nothing: name an empty repository, or run start in a
                       clone of R

Nothing is ever force-pushed. Then the platform experiment the spec states is created on R, and
start prints its id, what it waits for and, with $HL_UI_URL set, where the UI shows it.
--dry-run runs every check and says what would be pushed, and changes nothing.
hl platform-experiments create FILE --repository R creates one on a repository that holds its
code already.`, stderr)
	project := fs.String("project", "", "the project it belongs to (default $HL_PROJECT)")
	repository := fs.String("repository", "", "the repository it works on")
	dryRun := fs.Bool("dry-run", false, "run every check and change nothing")
	dir, err := parseOne(fs, args, "DIR", "")
	if err != nil {
		return parseExit(err)
	}
	if *repository == "" {
		return parseExit(misuse(fs, "want --repository: the repository the experiment works on"))
	}
	spec, err := experimentspec.Load(dir)
	if err == nil {
		err = spec.Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	body := createBody{Spec: spec, Repository: *repository}
	if body.ProjectID, err = projectOf(fs, *project); err != nil {
		return parseExit(err)
	}
	var check struct {
		Repository    string `json:"repository"`
		RepositoryURL string `json:"repository_url"`
	}
	if err := c.Do("GET", "/projects/"+url.PathEscape(body.ProjectID)+"/repository-check?"+url.Values{"repository": {body.Repository}}.Encode(), nil, &check); err != nil {
		return fail(stderr, err)
	}
	body.Repository = check.Repository
	plan, err := planPush(dir, check.RepositoryURL)
	if plan != nil {
		defer os.RemoveAll(plan.gitDir)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if plan.other {
		return fail(stderr, fmt.Errorf("%s already holds other code; name an empty repository or run start in a clone of it", body.Repository))
	}
	if *dryRun {
		if plan.files > 0 {
			fmt.Fprintf(stdout, "would push %d files of %s to %s as main, and create platform experiment %q in project %s\n", plan.files, dir, body.Repository, spec.Name, body.ProjectID)
		} else {
			fmt.Fprintf(stdout, "%s already holds %s: would push nothing, and create platform experiment %q in project %s\n", body.Repository, dir, spec.Name, body.ProjectID)
		}
		fmt.Fprintln(stderr, "hl: platform-experiments start: --dry-run, nothing pushed or created")
		return 0
	}
	if plan.files > 0 {
		if err := plan.push(spec.Name); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "pushed %d files of %s to %s as main\n", plan.files, dir, body.Repository)
	} else {
		fmt.Fprintf(stdout, "%s already holds %s: pushed nothing\n", body.Repository, dir)
	}
	var created struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		StatusReason struct {
			Summary string `json:"summary"`
		} `json:"status_reason"`
	}
	if err := c.Do("POST", "/platform-experiments", body, &created); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "created %s, %s: %s\n", created.ID, created.Status, created.StatusReason.Summary)
	if ui := os.Getenv("HL_UI_URL"); ui != "" {
		fmt.Fprintln(stdout, strings.TrimSuffix(ui, "/")+"/experiments/"+created.ID)
	}
	return 0
}

// pushPlan is what makes a repository's default branch hold a directory: its tree, written in a
// git dir of its own, and the commit of it to push when the repository has no branch.
type pushPlan struct {
	gitDir, remote, tree string
	// files is how many files a push carries, 0 when the repository holds the tree already.
	files int
	// other is a repository that holds something else.
	other bool
}

// planPush writes the tree of dir, as git add -A would commit it, into a git dir of its own and
// compares it with the default branch of remote. The caller removes the plan's git dir.
func planPush(dir, remote string) (*pushPlan, error) {
	work, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	gitDir, err := os.MkdirTemp("", "hl-start-")
	if err != nil {
		return nil, err
	}
	plan := &pushPlan{gitDir: gitDir, remote: remote}
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
	refs, err := plan.git(work, "ls-remote", "--symref", remote)
	if err != nil {
		return plan, fmt.Errorf("reading the branches of %s: %w", remote, err)
	}
	branches, head := false, false
	for _, line := range strings.Split(refs, "\n") {
		ref, name, _ := strings.Cut(line, "\t")
		branches = branches || strings.HasPrefix(name, "refs/heads/")
		head = head || (name == "HEAD" && !strings.HasPrefix(ref, "ref: "))
	}
	switch {
	case !branches:
		plan.files = strings.Count(listed, "\n") + 1
		return plan, nil
	case !head:
		plan.other = true
		return plan, nil
	}
	if _, err := plan.git(work, "fetch", "--quiet", "--depth=1", remote, "HEAD"); err != nil {
		return plan, fmt.Errorf("fetching the default branch of %s: %w", remote, err)
	}
	fetched, err := plan.git(work, "rev-parse", "FETCH_HEAD^{tree}")
	plan.other = fetched != plan.tree
	return plan, err
}

// push commits the tree and pushes it as main, which a repository with no branch takes as its
// default one. A branch made since planPush read none refuses it, as git refuses any push that is
// no fast-forward.
func (p *pushPlan) push(name string) error {
	commit, err := p.git("", "commit-tree", p.tree, "-m", "Start "+name)
	if err != nil {
		return err
	}
	if _, err := p.git("", "push", "--quiet", p.remote, commit+":refs/heads/main"); err != nil {
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
