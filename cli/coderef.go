package main

import (
	"cmp"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// branchPrefix is what every branch of a platform experiment starts with, and baselineBranch
// the one its baseline is measured on.
func branchPrefix(platformExperiment string) string { return "hl/" + platformExperiment + "/" }

func baselineBranch(platformExperiment string) string {
	return branchPrefix(platformExperiment) + "baseline"
}

// ofBaselineAgent is whether a baseline agent's job may run on branch: the baseline branch, or
// one beside it, baseline-<name>, where the agent tries its code as a researcher would. Only a
// job of the baseline branch itself is declared the baseline.
func ofBaselineAgent(platformExperiment, branch string) bool {
	if branch == baselineBranch(platformExperiment) {
		return true
	}
	name, beside := strings.CutPrefix(branch, baselineBranch(platformExperiment)+"-")
	return beside && name != "" && !strings.ContainsAny(name, "/ \t\n")
}

// fillCodeRef sets an empty metadata.code_ref to the commit at HEAD of the git repository in dir,
// "<origin url>@<sha>", and metadata.branch to the branch HEAD is on, which it pushes to origin.
// The branch is one of the platform experiment's: nothing is pushed from any other, and a job
// with no hypothesis is the baseline's and runs on the baseline's branch alone, or a check
// (metadata.check), which runs on any of them.
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
	url, err := git(dir, "remote", "get-url", "origin")
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
	check, _ := metadata["check"].(bool)
	switch {
	case hypothesis == "" && !check && !ofBaselineAgent(pe, branch):
		return fmt.Errorf("no hypothesis is named and HEAD is not on %s: only a baseline agent's job runs without one, on that branch (git checkout -b %s) or, to check its code as a researcher would, on %s-check. Every other job names its hypothesis with --hypothesis ID", baselineBranch(pe), baselineBranch(pe), baselineBranch(pe))
	case !strings.HasPrefix(branch, branchPrefix(pe)) || branch == branchPrefix(pe):
		on := "detached"
		if branch != "" {
			on = "on " + branch
		}
		return fmt.Errorf("HEAD is %s, not on a branch under %s: hl hypothesis checkout %s puts you on your branch for this hypothesis; commit there and submit again", on, branchPrefix(pe), cmp.Or(hypothesis, "ID"))
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
