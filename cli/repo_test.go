package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/repotemplate"
)

func repo(t *testing.T, command string) (int, string) {
	t.Helper()
	code, stdout, stderr := hl(t, "repo", command)
	return code, stdout + stderr
}

func emptyClone(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	clone := t.TempDir()
	mustGit(t, clone, "init", "-q")
	t.Chdir(clone)
	return clone
}

func write(t *testing.T, clone, path, content string) {
	t.Helper()
	target := filepath.Join(clone, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	filledExperiment = "name: sweep\ndescription: |\n  the brief\nmetrics:\n  - key: val_accuracy\n    direction: maximize\n"
	filledJobSpec    = "job:\n  image: registry/job:1\n  command: [\"python\", \"train.py\"]\n  cpu: 2\n"
)

// An empty repository gets all three files, and one with code keeps what it has: a file that is
// there is never written, whatever it holds. Nothing is staged or committed.
func TestRepoInitAddsOnlyTheFilesTheCloneLacks(t *testing.T) {
	clone := emptyClone(t)
	write(t, clone, "README.md", "ours\n")
	write(t, clone, "train.py", "print(1)\n")
	code, out := repo(t, "init")
	if code != 0 || !strings.Contains(out, "kept   README.md") || !strings.Contains(out, "added  "+repotemplate.JobSpec) || !strings.Contains(out, "added  "+repotemplate.ExperimentFile) {
		t.Fatalf("hl repo init = %d\n%s", code, out)
	}
	if kept, _ := os.ReadFile(filepath.Join(clone, "README.md")); string(kept) != "ours\n" {
		t.Fatalf("README.md was overwritten with %q", kept)
	}
	added, _ := os.ReadFile(filepath.Join(clone, repotemplate.JobSpec))
	if string(added) != repotemplate.Files()[repotemplate.JobSpec] {
		t.Fatal("the job spec added is not the template's")
	}
	if staged := mustGit(t, clone, "status", "--porcelain"); strings.Contains(staged, "A ") {
		t.Fatalf("hl repo init staged files:\n%s", staged)
	}
	if code, out := repo(t, "init"); code != 0 || strings.Contains(out, "added") {
		t.Fatalf("a second hl repo init = %d\n%s", code, out)
	}
	// It finds the top of the clone from any directory in it.
	sub := filepath.Join(clone, "src")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if code, out := repo(t, "check"); code != 1 || !strings.Contains(out, "job-spec: "+repotemplate.JobSpec+" still has") {
		t.Fatalf("hl repo check from a subdirectory = %d\n%s", code, out)
	}
}

func TestRepoCheckNamesWhatKeepsACloneFromReady(t *testing.T) {
	clone := emptyClone(t)
	code, out := repo(t, "check")
	for _, name := range []string{"readme: README.md is missing", "experiment: " + repotemplate.ExperimentFile + " is missing", "job-spec: " + repotemplate.JobSpec + " is missing"} {
		if code != 1 || !strings.Contains(out, name) {
			t.Fatalf("an empty clone: hl repo check = %d, want %q in\n%s", code, name, out)
		}
	}
	repo(t, "init")
	code, out = repo(t, "check")
	for _, name := range []string{"readme: README.md still has", "experiment: " + repotemplate.ExperimentFile + " states no name, description, metrics", "job-spec: " + repotemplate.JobSpec + " still has"} {
		if code != 1 || !strings.Contains(out, name) {
			t.Fatalf("the unfilled template: hl repo check = %d, want %q in\n%s", code, name, out)
		}
	}
	write(t, clone, "README.md", "# sweep\n")
	write(t, clone, repotemplate.ExperimentFile, filledExperiment)
	write(t, clone, repotemplate.JobSpec, filledJobSpec)
	if code, out := repo(t, "check"); code != 0 || !strings.HasPrefix(out, "ready: ") {
		t.Fatalf("a filled clone: hl repo check = %d\n%s", code, out)
	}
	for name, tc := range map[string]struct{ path, content, want string }{
		"free text with a colon in it":   {repotemplate.ExperimentFile, "name: sweep\ndescription: the goal: accuracy\nmetrics: []\n", "experiment: " + repotemplate.ExperimentFile + " is not the YAML of an experiment: yaml: line 2:"},
		"no ranking metric":              {repotemplate.ExperimentFile, "name: sweep\ndescription: d\nmetrics:\n  - key: gate\n    role: constraint\n", "has no ranking metric"},
		"a job spec that is not YAML":    {repotemplate.JobSpec, "job:\n  image: a\n command: b\n", "job-spec: " + repotemplate.JobSpec + " is not the YAML of a job spec: yaml: line "},
		"a job spec with no job":         {repotemplate.JobSpec, "metadata:\n  theory: t\n", "states no job"},
		"a secret in the committed spec": {repotemplate.JobSpec, filledJobSpec + "  secret_env:\n    TOKEN: x\n", "sets job.secret_env"},
	} {
		write(t, clone, repotemplate.ExperimentFile, filledExperiment)
		write(t, clone, repotemplate.JobSpec, filledJobSpec)
		write(t, clone, tc.path, tc.content)
		if code, out := repo(t, "check"); code != 1 || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 1 {
			t.Errorf("%s: hl repo check = %d, want only %q in\n%s", name, code, tc.want, out)
		}
	}
}

func TestRepoCommandsAreRefusedOutsideAClone(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	t.Chdir(t.TempDir())
	for _, command := range []string{"init", "check"} {
		if code, out := repo(t, command); code == 0 || !strings.Contains(out, "this is not a git repository") {
			t.Errorf("hl repo %s outside a clone = %d\n%s", command, code, out)
		}
	}
}
