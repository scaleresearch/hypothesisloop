package main

import (
	"io"
	"net/http"
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

// emptyClone is a repository with nothing in it, of platform experiment pe-1 as the platform
// answers it: named sweep and ranked on val_accuracy.
func emptyClone(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/platform-experiments/pe-1" {
			t.Errorf("hl repo asked %s %s, want only the platform experiment", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"pe-1","name":"sweep","metrics":[{"key":"val_accuracy","direction":"maximize"}]}`)
	}))
	t.Setenv(platformExperimentEnv, "pe-1")
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
	filledJobSpec    = "job:\n  image: registry/job:1\n  command: [\"python\", \"train.py\"]\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n"
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
	// The files are whole and no code reports the metric the experiment ranks on.
	want := "metrics: no file of the clone besides README.md and " + repotemplate.ExperimentFile + " names val_accuracy, a metric platform experiment pe-1 declares"
	if code, out := repo(t, "check"); code != 1 || !strings.HasPrefix(out, want) || strings.Count(out, "\n") != 2 {
		t.Fatalf("a clone whose code names no declared metric: hl repo check = %d, want only %q in\n%s", code, want, out)
	}
	write(t, clone, "train.py", "post_metric(1.0, accuracy, \"val_accuracy\")\n")
	// A prepared repository, or one in another language, reports through the platform's request
	// and holds no helper: it passes, and is told the request.
	if err := os.Remove(filepath.Join(clone, repotemplate.MetricsHelper)); err != nil {
		t.Fatal(err)
	}
	if code, out := repo(t, "check"); code != 0 || out != metricsHelperAbsent+"\n"+repoChecked("pe-1")+"\n" || !strings.Contains(out, "/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics") {
		t.Fatalf("a clone with no metrics helper: hl repo check = %d\n%s", code, out)
	}
	repo(t, "init")
	// A baseline agent read "ready" of another experiment's files and went on with them: a pass
	// says what was checked and what was not, in one line, and never calls the repository ready.
	if code, out := repo(t, "check"); code != 0 || out != metricsHelperPresent+"\n"+repoChecked("pe-1")+"\n" || strings.Contains(out, "ready") ||
		!strings.Contains(out, "\nchecked: the layout, each file's validity, and that") || !strings.HasSuffix(out, "fit platform experiment pe-1. Not checked: the README's text and the code the job spec runs.\n") {
		t.Fatalf("a filled clone: hl repo check = %d\n%s", code, out)
	}
	for name, tc := range map[string]struct{ path, content, want string }{
		// `hl repo check` passed a repository whose files described a compression benchmark,
		// straight after `hl repo init` had kept all three.
		"an earlier experiment's name":    {repotemplate.ExperimentFile, strings.Replace(filledExperiment, "sweep", "compression", 1), "experiment: " + repotemplate.ExperimentFile + ` states name "compression", and platform experiment pe-1 is "sweep": the file may be an earlier experiment's`},
		"an earlier experiment's metrics": {repotemplate.ExperimentFile, strings.Replace(filledExperiment, "val_accuracy\n    direction: maximize", "ratio\n    direction: minimize\n    role: ranking", 1), "states the metrics ratio minimize ranking, and platform experiment pe-1 declares val_accuracy maximize ranking"},
		// A job.yaml stated estimated_duration_hours: 0.05 and all twelve jobs of a run were
		// given that in place of the estimate the platform measured.
		"an estimate in the committed spec": {repotemplate.JobSpec, "metadata:\n  estimated_duration_hours: 0.05\n" + filledJobSpec, "states metadata.estimated_duration_hours 0.05: every job submitted from this file would hold that"},
		"free text with a colon in it":      {repotemplate.ExperimentFile, "name: sweep: wide\ndescription: d\nmetrics: []\n", "experiment: " + repotemplate.ExperimentFile + " is not the YAML of an experiment: line 1, column 12: mapping values are not allowed in this context, where the line reads: name: sweep: wide"},
		"no ranking metric":                 {repotemplate.ExperimentFile, "name: sweep\ndescription: d\nmetrics:\n  - key: gate\n    role: constraint\n", "has no ranking metric"},
		"a job spec that is not YAML":       {repotemplate.JobSpec, "job:\n  image: a\n command: b\n", "job-spec: " + repotemplate.JobSpec + " is not the YAML of a job spec: line 3, column 2: did not find expected key, where the line reads: command: b"},
		"a job spec with no job":            {repotemplate.JobSpec, "metadata:\n  theory: t\n", "states no job"},
		"a job spec with no cpu":            {repotemplate.JobSpec, "job:\n  image: a\n  memory: 8Gi\n  storage: 4Gi\n", "states no job.cpu: state the cores one node of the job uses, as in cpu: 2"},
		"a job spec with no memory":         {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  storage: 4Gi\n", "states no job.memory"},
		"a job spec with no storage":        {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n", "states no job.storage"},
		"memory as a share of the node":     {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: max\n  storage: 4Gi\n", "states job.memory as max"},
		"storage as a share of the node":    {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n  storage: max\n", "states job.storage as max"},
		"a group with no memory":            {repotemplate.JobSpec, "job:\n  image: a\n  groups:\n    - name: w\n      cpu: 2\n      memory: 8Gi\n      storage: 4Gi\n    - name: ps\n      cpu: 1\n", "states no job.groups[1].memory"},
		"a secret in the committed spec":    {repotemplate.JobSpec, filledJobSpec + "  secret_env:\n    TOKEN: x\n", "sets job.secret_env"},
	} {
		write(t, clone, repotemplate.ExperimentFile, filledExperiment)
		write(t, clone, repotemplate.JobSpec, filledJobSpec)
		write(t, clone, tc.path, tc.content)
		if code, out := repo(t, "check"); code != 1 || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 2 || !strings.HasSuffix(out, metricsHelperPresent+"\n") {
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
