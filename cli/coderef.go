package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// fillCodeRef sets an empty metadata.code_ref to "<origin url>@<HEAD sha>" of the git repository
// in dir. Outside a repository it leaves code_ref as it is.
func fillCodeRef(metadata map[string]any, dir string) error {
	if v := metadata["code_ref"]; v != nil && v != "" {
		return nil
	}
	if _, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	}
	dirty, err := git(dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if dirty != "" {
		return errors.New("uncommitted changes to tracked files: commit and git push, then submit again")
	}
	sha, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
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
