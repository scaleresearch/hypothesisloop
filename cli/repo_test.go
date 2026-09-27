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
	filledJobSpec    = "job:\n  image: registry/job:1\n  command: [\"python\", \"train.py\"]\n  cpu: 2\n  storage: 4Gi\n"
)

// An empty repository gets every file of the template, and one with code keeps what it has: a file that is
// there is never written, whatever it holds. Nothing is staged or committed.
func TestRepoInitAddsOnlyTheFilesTheCloneLacks(t *testing.T) {
	clone := emptyClone(t)
	write(t, clone, "README.md", "ours\n")
	write(t, clone, "train.py", "print(1)\n")
	code, out := repo(t, "init")
	if code != 0 || !strings.Contains(out, "kept   README.md") || !strings.Contains(out, "added  "+repotemplate.JobSpec) || !strings.Contains(out, "added  "+repotemplate.ExperimentFile) || !strings.Contains(out, "added  "+repotemplate.Gitignore) || !strings.Contains(out, "added  "+repotemplate.MetricsHelper) {
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
	// Two planners found another experiment's files here, were told "kept" and "ready" before
	// they had written a line, and had to work out for themselves that the files were not theirs.
	if !strings.Contains(out, "may describe an earlier experiment") {
		t.Fatalf("hl repo init kept a file and did not say whose it may be:\n%s", out)
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

func TestRepoCheckNamesWhatAClonesFilesLack(t *testing.T) {
	clone := emptyClone(t)
	code, out := repo(t, "check")
	for _, name := range []string{"readme: README.md is missing", "experiment: " + repotemplate.ExperimentFile + " is missing", "job-spec: " + repotemplate.JobSpec + " is missing", metricsHelperAbsent} {
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
	// A prepared repository, or one in another language, reports through the platform's request
	// and holds no helper: it passes, and is told the request.
	if err := os.Remove(filepath.Join(clone, repotemplate.MetricsHelper)); err != nil {
		t.Fatal(err)
	}
	if code, out := repo(t, "check"); code != 0 || out != metricsHelperAbsent+"\n"+repoChecked+"\n" || !strings.Contains(out, "/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics") {
		t.Fatalf("a clone with no metrics helper: hl repo check = %d\n%s", code, out)
	}
	repo(t, "init")
	// A baseline agent read "ready" of another experiment's files and went on with them: a pass
	// says what was checked and what was not, in one line, and never calls the repository ready.
	if code, out := repo(t, "check"); code != 0 || out != metricsHelperPresent+"\n"+repoChecked+"\n" || strings.Contains(out, "ready") ||
		!strings.Contains(out, "\nchecked: layout and file validity") || !strings.HasSuffix(out, "Not checked: that the files describe this experiment.\n") {
		t.Fatalf("a filled clone: hl repo check = %d\n%s", code, out)
	}
	for name, tc := range map[string]struct{ path, content, want string }{
		"free text with a colon in it":   {repotemplate.ExperimentFile, "name: sweep: wide\ndescription: d\nmetrics: []\n", "experiment: " + repotemplate.ExperimentFile + " is not the YAML of an experiment: line 1, column 12: mapping values are not allowed in this context, where the line reads: name: sweep: wide"},
		"no ranking metric":              {repotemplate.ExperimentFile, "name: sweep\ndescription: d\nmetrics:\n  - key: gate\n    role: constraint\n", "has no ranking metric"},
		"a job spec that is not YAML":    {repotemplate.JobSpec, "job:\n  image: a\n command: b\n", "job-spec: " + repotemplate.JobSpec + " is not the YAML of a job spec: line 3, column 2: did not find expected key, where the line reads: command: b"},
		"a job spec with no job":         {repotemplate.JobSpec, "metadata:\n  theory: t\n", "states no job"},
		"a job spec with no storage":     {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n", "states no job.storage"},
		"storage as a share of the node": {repotemplate.JobSpec, "job:\n  image: a\n  storage: max\n", "states job.storage as max"},
		"a group with no storage":        {repotemplate.JobSpec, "job:\n  image: a\n  groups:\n    - name: w\n      storage: 4Gi\n    - name: ps\n", "states no job.groups[1].storage"},
		"a secret in the committed spec": {repotemplate.JobSpec, filledJobSpec + "  secret_env:\n    TOKEN: x\n", "sets job.secret_env"},
	} {
		write(t, clone, repotemplate.ExperimentFile, filledExperiment)
		write(t, clone, repotemplate.JobSpec, filledJobSpec)
		write(t, clone, tc.path, tc.content)
		if code, out := repo(t, "check"); code != 1 || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 2 || !strings.HasSuffix(out, metricsHelperPresent+"\n") {
			t.Errorf("%s: hl repo check = %d, want only %q in\n%s", name, code, tc.want, out)
		}
	}
}

// A job.yaml stated estimated_duration_hours: 0.05 and all twelve jobs of a run were given that
// in place of the estimate the platform measured. The check notes a stated estimate, failing
// nothing, and the template states none.
func TestRepoCheckNotesAJobSpecThatStatesAnEstimate(t *testing.T) {
	clone := emptyClone(t)
	repo(t, "init")
	if data, err := os.ReadFile(filepath.Join(clone, repotemplate.JobSpec)); err != nil || statedEstimate(os.DirFS(clone)) != "" {
		t.Fatalf("the template's job spec states an estimate (%v):\n%s", err, data)
	}
	write(t, clone, "README.md", "# sweep\n")
	write(t, clone, repotemplate.ExperimentFile, filledExperiment)
	write(t, clone, repotemplate.JobSpec, "metadata:\n  estimated_duration_hours: 0.05\n"+filledJobSpec)
	code, out := repo(t, "check")
	want := "estimate: " + repotemplate.JobSpec + " states metadata.estimated_duration_hours 0.05, which fails nothing"
	if code != 0 || !strings.Contains(out, "\n"+want) || !strings.Contains(out, "Leave it out and the measured estimate applies") || !strings.HasSuffix(out, repoChecked+"\n") {
		t.Errorf("a job spec that states an estimate: hl repo check = %d, want a pass that notes it\n%s", code, out)
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

// A planner whose file had a colon in its first line was told no line, and spent six commands
// installing a parser to find it. Every file hl reads is refused with where and what it reads there.
func TestAFileThatIsNotYAMLIsRefusedWithItsLineColumnAndText(t *testing.T) {
	file := filepath.Join(t.TempDir(), "proposal.yaml")
	write(t, filepath.Dir(file), "proposal.yaml", "experiment:\n  name: SHA-256 throughput: on CPU\n")
	t.Setenv("HL_TOKEN", "hlk_x")
	t.Setenv("API_URL", "http://127.0.0.1:1")
	want := file + " is not valid YAML: line 2, column 27: mapping values are not allowed in this context, where the line reads: name: SHA-256 throughput: on CPU"
	for _, args := range [][]string{
		{"platform-experiments", "propose", "--platform-experiment", "pe-1", file},
		{"job", "submit", "--agent", "ag-1", file},
		{"hypothesis", "submit", "--agent", "ag-1", "--file", file},
	} {
		if code, _, stderr := hl(t, args...); code != 2 || strings.TrimSpace(stderr) != "hl: "+want {
			t.Errorf("hl %s = %d\n%s\nwant only\nhl: %s", strings.Join(args[:2], " "), code, stderr, want)
		}
	}
}
