package domain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func TestOnlyTheCodeUnreachableExitIsTheEnvironmentsFailure(t *testing.T) {
	if got := clusterapi.PhaseReasonOfExit(ExitCodeCodeUnreachable); got != PhaseReasonCodeUnreachable {
		t.Fatalf("exit %d is %q", ExitCodeCodeUnreachable, got)
	}
	for _, code := range []int{1, 2, 137} {
		if got := clusterapi.PhaseReasonOfExit(code); got != PhaseReasonContainerFailed {
			t.Fatalf("exit %d is %q", code, got)
		}
	}

	eviction, environment := PhaseDetail{Reason: PhaseReasonCodeUnreachable, Message: "command exited with code 75: hl-clone: no answer\nmore"}.EvictionOfFailure()
	if !environment || eviction != "code_unreachable: command exited with code 75: hl-clone: no answer" || !IsInfrastructureFault(eviction) {
		t.Fatalf("got %q %v, infrastructure %v", eviction, environment, IsInfrastructureFault(eviction))
	}
	if eviction, environment := (PhaseDetail{Reason: PhaseReasonCodeUnreachable}).EvictionOfFailure(); !environment || eviction != EvictionCodeUnreachable {
		t.Fatalf("got %q %v", eviction, environment)
	}
	for _, reason := range []string{PhaseReasonContainerFailed, PhaseReasonOOMKilled, ""} {
		if eviction, environment := (PhaseDetail{Reason: reason, Message: "boom"}).EvictionOfFailure(); environment {
			t.Fatalf("%q is the environment's failure: %q", reason, eviction)
		}
	}
}

// hl-clone is the one program that ends with ExitCodeCodeUnreachable, and only where the
// repository does not answer: a commit a repository that answers does not hold is the job's own.
func TestHLCloneExitsWithTheCodeUnreachableCodeOnlyWhereTheRepositoryDoesNotAnswer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "images", "job", "hl-clone"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sha := strings.Repeat("a", 40)

	cases := map[string]struct {
		url  string
		code int
		says string
	}{
		"no answer":         {"file://" + filepath.Join(t.TempDir(), "no-such-repository"), ExitCodeCodeUnreachable, "does not answer"},
		"commit not pushed": {"file://" + origin, 1, "the commit is not in it"},
	}
	for name, c := range cases {
		out, err := exec.Command("sh", script, c.url+"@"+sha, filepath.Join(t.TempDir(), "code")).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != c.code || !strings.Contains(string(out), c.says) {
			t.Errorf("%s: %v, want exit %d saying %q:\n%s", name, err, c.code, c.says, out)
		}
	}
}

// Reporting a failure must not by itself condemn a job that still has retries left, and a create
// the cluster refused is tried again; a job whose own configuration cannot start is ended.
func TestOnlyReasonsThatNeverHealEndAJobEarly(t *testing.T) {
	for _, r := range []string{PhaseReasonContainerFailed, PhaseReasonOOMKilled, PhaseReasonCreateFailed} {
		if NeverSelfHealsPhaseReasons[r] {
			t.Errorf("%s must not be in NeverSelfHealsPhaseReasons: it would evict before retries run", r)
		}
	}
	if !NeverSelfHealsPhaseReasons[PhaseReasonConfigError] {
		t.Errorf("%s is not one the control plane ends a job for", PhaseReasonConfigError)
	}
}
