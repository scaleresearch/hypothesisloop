package main

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// fillCodeRef sets an empty metadata.code_ref to the commit at HEAD of the git repository in dir:
// "<origin url>@<sha>" once it is pushed to origin, and in a repository with no origin, which is
// what an agent of a platform experiment without a repository works in, the address the platform
// keeps the commit at once hl code push stored it.
func fillCodeRef(c *apiclient.Client, metadata map[string]any, dir string) error {
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
		return fillPlatformCodeRef(c, metadata, sha)
	}
	url, err := git(dir, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	pushed, err := git(dir, "for-each-ref", "--contains", "HEAD", "refs/remotes/origin")
	if err != nil {
		return err
	}
	if pushed == "" {
		return fmt.Errorf("HEAD %s is not on origin: git push, then submit again", sha)
	}
	metadata["code_ref"] = url + "@" + sha
	return nil
}

func fillPlatformCodeRef(c *apiclient.Client, metadata map[string]any, sha string) error {
	agent, _ := metadata["agent_id"].(string)
	experiment, _ := metadata["platform_experiment_id"].(string)
	if agent == "" || experiment == "" {
		return errors.New("metadata.code_ref is empty and this repository has no origin: the platform keeps such code per agent and platform experiment, so give --agent and --platform-experiment, or set code_ref")
	}
	stored, err := codeStored(c, experiment, agent, sha)
	if err != nil {
		return err
	}
	if !stored {
		return fmt.Errorf("HEAD %s is not stored with the platform: hl code push, then submit again", sha)
	}
	metadata["code_ref"] = codeRef(c.BaseURL, experiment, agent, sha)
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
		return "", errors.New("uncommitted changes to tracked files: commit and push (git push, or hl code push with no repository), then submit again (to vary only hypothesis or env, leave the spec as committed and pass --hypothesis and --env)")
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
