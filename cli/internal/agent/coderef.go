package agent

import (
	"cmp"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
)

// fillCodeRef sets an empty metadata.code_ref to the commit at HEAD of the git repository in dir,
// "<origin url>@<sha>", and metadata.branch to the branch HEAD is on, which it pushes to origin.
// The branch is one of the platform experiment's: nothing is pushed from any other, and a job
// with no hypothesis is the baseline's and runs on the baseline's branch or one beside it.
func fillCodeRef(metadata map[string]any, dir string) error {
	if v := metadata["code_ref"]; v != nil && v != "" {
		return nil
	}
	sha, err := committedHead(dir)
	if err != nil {
		return err
	}
	remotes, err := git(dir, "remote")
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(remotes), "origin") {
		return errors.New("metadata.code_ref is empty and this repository has no origin: a job runs a commit of the platform experiment's repository, so work in your clone of it (hl platform-experiments get names repository_url), or set code_ref")
	}
	url, err := git(dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return err
	}
	pe, _ := metadata["platform_experiment_id"].(string)
	if pe == "" {
		return errors.New("no platform experiment is named, so there is no branch to submit from: pass --platform-experiment PE")
	}
	// Empty on a detached HEAD, which is on no branch.
	branch, _ := git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	hypothesis, _ := metadata["hypothesis_id"].(string)
	switch {
	case hypothesis == "" && !gitrefs.IsBaselineAgentBranch(pe, branch):
		return fmt.Errorf("no hypothesis is named and HEAD is not on %s: only a baseline agent's job runs without one, on that branch (git checkout -b %s) or, to check its code as an executor would, on %s-check. Every other job names its hypothesis with --hypothesis ID", gitrefs.BaselineBranch(pe), gitrefs.BaselineBranch(pe), gitrefs.BaselineBranch(pe))
	case !gitrefs.IsExperimentBranch(pe, branch):
		on := "detached"
		if branch != "" {
			on = "on " + branch
		}
		return fmt.Errorf("HEAD is %s, not on a branch under %s: hl hypothesis checkout %s puts you on your branch for this hypothesis; commit there and submit again", on, gitrefs.BranchPrefix(pe), cmp.Or(hypothesis, "ID"))
	}
	if _, err := git(dir, "push", "--quiet", "origin", "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return err
	}
	metadata["code_ref"] = url + "@" + sha
	metadata["branch"] = branch
	return nil
}

// committedHead is the commit at HEAD of the git repository in dir, refused when a tracked file
// differs from it: what a job is given must be what was looked at.
func committedHead(dir string) (string, error) {
	if _, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errors.New("metadata.code_ref is empty and this is not a git repository: submit from your clone, or set code_ref")
		}
		return "", err
	}
	dirty, err := git(dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", errors.New("uncommitted changes to tracked files: commit, then submit again, which pushes your branch (to vary only hypothesis or env, leave the spec as committed and pass --hypothesis and --env)")
	}
	return git(dir, "rev-parse", "HEAD")
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runBest is the run's best job on its first ranking metric, as a submit's answer names it.
type runBest struct {
	JobID   string  `json:"job_id"`
	Metric  string  `json:"metric"`
	Value   float64 `json:"value"`
	CodeRef string  `json:"code_ref"`
	Branch  string  `json:"branch"`
}

// notOnBest says when the commit of codeRef, job jobID's, does not contain best's, as the git repository in
// dir tells, fetching best's branch, or else its commit, from origin when it lacks it. Empty
// when it does contain it.
func notOnBest(dir, jobID, codeRef string, best runBest) string {
	commit, bestCommit := commitOf(codeRef), commitOf(best.CodeRef)
	contains, err := isAncestor(dir, bestCommit, commit)
	if err != nil {
		for _, ref := range []string{best.Branch, bestCommit} {
			if ref == "" {
				continue
			}
			if _, fetchErr := git(dir, "fetch", "--quiet", "origin", ref); fetchErr != nil {
				continue
			}
			if contains, err = isAncestor(dir, bestCommit, commit); err == nil {
				break
			}
		}
	}
	named := fmt.Sprintf("the run's best on %s is %s (%g), commit %s", best.Metric, best.JobID, best.Value, bestCommit)
	if best.Branch != "" {
		named += " on " + best.Branch
	}
	switch {
	case err != nil:
		return fmt.Sprintf("%s; whether this job's commit contains it could not be told here: %s", named, err)
	case contains:
		return ""
	}
	fetch := cmp.Or(best.Branch, bestCommit)
	return fmt.Sprintf("%s, and this job's commit does not contain it. The job is queued all the same. If it was meant to build on the best, cancel it (hl job cancel %s), bring that commit in (git fetch origin %s, then git rebase or git merge %s) and submit again", named, jobID, fetch, bestCommit)
}

// commitOf is the commit of a code_ref, <repository url>@<sha>.
func commitOf(codeRef string) string {
	return codeRef[strings.LastIndex(codeRef, "@")+1:]
}

// isAncestor reports whether ancestor is commit or an ancestor of it in the git repository in
// dir, an error when git cannot tell, as for a commit it does not have.
func isAncestor(dir, ancestor, commit string) (bool, error) {
	if ancestor == "" || commit == "" {
		return false, errors.New("a code_ref names no commit")
	}
	_, err := git(dir, "merge-base", "--is-ancestor", ancestor, commit)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	}
	return false, err
}
