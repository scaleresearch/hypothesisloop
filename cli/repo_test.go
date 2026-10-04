package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/cli/repotemplate"
)

// A job spec with no image names the one its experiment was proposed with in hl repo check. A
// submit of it is the platform's to refuse (image_required).
func TestAJobThatNamesNoImageIsToldTheImageOfItsExperiment(t *testing.T) {
	clone := cloneOf(t, proposed)
	repo(t, "init")
	write(t, clone, repotemplate.JobSpec, "job:\n  command: [\"python\", \"bench.py\"]\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n")
	want := "states no job.image: every job names the image it runs in, and platform experiment pe-1 was proposed with image: torch"
	if code, out := repo(t, "check"); code != 1 || !strings.Contains(out, "job-spec: "+repotemplate.JobSpec+" "+want) {
		t.Fatalf("hl repo check = %d, want %q in\n%s", code, want, out)
	}
}

func repo(t *testing.T, command string) (int, string) {
	t.Helper()
	code, stdout, stderr := hl(t, "repo", command)
	return code, stdout + stderr
}

// emptyClone is a repository with nothing in it, of platform experiment pe-1 as the platform
// answers it: named sweep and ranked on val_accuracy.
func emptyClone(t *testing.T) string {
	t.Helper()
	return cloneOf(t, emptyExperiment)
}

const emptyExperiment = `{"id":"pe-1","name":"sweep","description":"the brief\n","metrics":[{"key":"val_accuracy","direction":"maximize"}]}`

// cloneOf is a repository with nothing in it, of platform experiment pe-1 as the platform
// answers it with experiment, worked in by its baseline agent.
func cloneOf(t *testing.T, experiment string) string {
	t.Helper()
	return cloneAs(t, experiment, "baseline")
}

// cloneAs is cloneOf worked in by an agent of role.
func cloneAs(t *testing.T, experiment, role string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/platform-experiments/pe-1":
			_, _ = io.WriteString(w, experiment)
		case r.Method == "GET" && r.URL.Path == "/platform-experiments/pe-1/signups/ag-1":
			_, _ = io.WriteString(w, `{"agent_id":"ag-1","role":"`+role+`"}`)
		default:
			t.Errorf("hl repo asked %s %s, want only the platform experiment and the agent's signup", r.Method, r.URL.Path)
		}
	}))
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-1/ag-1"))
	clone := t.TempDir()
	mustGit(t, clone, "init", "-q")
	t.Chdir(clone)
	return clone
}

// proposed is platform experiment pe-1 as accepted from a planner's proposal: a name with a
// colon in it, a brief of several lines, a gate, and the image and the type its jobs run on.
const proposed = `{"id":"pe-1","name":"ResNet-18: throughput\n","description":"GOAL: images per second.\n\nBASELINE\n  config: fp32\n  measured: not yet established\n",
"metrics":[{"key":"images_per_s","direction":"maximize","role":"ranking"},{"key":"top1_agreement","direction":"maximize","role":"constraint","bound":0.99}],
"proposal":{"accelerator_type":"nvidia.com/gpu.product=RTX-4090","image":"torch","cpu":"8","memory":"32Gi","metric_reason":"Throughput is what the goal asks for.\n","measurement":"Images per second over 200 batches.\n"}}`

func read(t *testing.T, clone, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// In run 4 a researcher ran hl repo init on main before it had a branch, and the files it wrote
// stopped hl hypothesis checkout. The baseline writes the files; every other agent gets them with
// the baseline's commit.
func TestRepoInitIsRefusedToAnAgentOtherThanTheBaseline(t *testing.T) {
	clone := cloneAs(t, emptyExperiment, "competitor")
	code, out := repo(t, "init")
	if code != 1 || !strings.Contains(out, "only the baseline agent writes the experiment's files, and your role is competitor") || !strings.Contains(out, "hl hypothesis checkout ID") {
		t.Fatalf("hl repo init by a researcher = %d, want refused pointing at checkout:\n%s", code, out)
	}
	if entries, err := os.ReadDir(clone); err != nil || len(entries) != 1 {
		t.Errorf("the clone holds %v, %v, want nothing written besides .git", entries, err)
	}
}

// A baseline agent copied a 54-line brief into experiment.yaml by hand, broke the YAML on the
// colon in the experiment's name and committed placeholders: what the platform holds of the
// experiment is written by hl repo init, and only the command of a trial is left to type.
func TestRepoInitWritesWhatThePlatformHoldsOfTheExperiment(t *testing.T) {
	clone := cloneOf(t, proposed)
	if code, out := repo(t, "init"); code != 0 || strings.Count(out, "added    ") != 6 {
		t.Fatalf("hl repo init = %d\n%s", code, out)
	}
	var stated statedExperiment
	if err := yaml.Unmarshal([]byte(read(t, clone, repotemplate.ExperimentFile)), &stated); err != nil {
		t.Fatalf("the experiment file is not YAML: %v\n%s", err, read(t, clone, repotemplate.ExperimentFile))
	}
	if stated.Name != "ResNet-18: throughput\n" || !strings.HasSuffix(stated.Description, "measured: not yet established\n") || len(stated.Metrics) != 2 ||
		stated.Metrics[1].Role != "constraint" || *stated.Metrics[1].Bound != 0.99 {
		t.Fatalf("the experiment file states %+v", stated)
	}
	var spec struct {
		Job map[string]any `yaml:"job"`
	}
	if err := yaml.Unmarshal([]byte(read(t, clone, repotemplate.JobSpec)), &spec); err != nil || spec.Job["image"] != "torch" ||
		spec.Job["accelerator_type"] != "nvidia.com/gpu.product=RTX-4090" || spec.Job["accelerator_count"] != 1 {
		t.Fatalf("the job spec states %v, %v, want the proposal's image and accelerator_type", spec.Job, err)
	}
	// A planner stated what a node asks for in the prose of its brief, and the baseline agent
	// copied it into the job spec by hand. What the proposal states is written, and what it
	// leaves out stays as the template has it.
	if spec.Job["cpu"] != 8 || spec.Job["memory"] != "32Gi" || spec.Job["storage"] != "4Gi" {
		t.Fatalf("the job spec asks for cpu %v, memory %v, storage %v, want the proposal's 8 and 32Gi and the template's 4Gi", spec.Job["cpu"], spec.Job["memory"], spec.Job["storage"])
	}
	if left := strings.Count(read(t, clone, repotemplate.JobSpec), repotemplate.Placeholder); left != 1 {
		t.Fatalf("the job spec leaves %d %s, want only the command of one trial", left, repotemplate.Placeholder)
	}
	readme := read(t, clone, repotemplate.Readme)
	if !strings.HasPrefix(readme, "# ResNet-18: throughput\n") || !strings.Contains(readme, "Throughput is what the goal asks for.\n\nImages per second over 200 batches. A\ntrial reports the gate") {
		t.Fatalf("the README lacks the experiment's name or what its proposal says is measured:\n%s", readme)
	}
	// With the command and the README written and the code naming its metrics, the files pass.
	write(t, clone, repotemplate.JobSpec, strings.Replace(read(t, clone, repotemplate.JobSpec), "REPLACE-WITH-THE-COMMAND-OF-ONE-TRIAL", "python bench.py", 1))
	write(t, clone, repotemplate.Readme, "# ResNet-18: throughput\n")
	write(t, clone, "bench.py", "post_metric(1.0, rate, \"images_per_s\")\npost_metric(1.0, agreement, \"top1_agreement\")\n")
	if code, out := repo(t, "check"); code != 0 {
		t.Fatalf("hl repo check on the files hl repo init wrote = %d\n%s", code, out)
	}
	// A second init keeps the agent's work and the experiment file it wrote.
	if code, out := repo(t, "init"); code != 0 || strings.Count(out, "kept     ") != 6 {
		t.Fatalf("a second hl repo init = %d\n%s", code, out)
	}
}

// Work on cores names no accelerator, so the job spec of an experiment on the CPU type has none.
func TestRepoInitWritesAJobSpecWithNoAcceleratorForWorkOnCores(t *testing.T) {
	clone := cloneOf(t, strings.Replace(proposed, "nvidia.com/gpu.product=RTX-4090", cpuType, 1))
	repo(t, "init")
	var spec struct {
		Job map[string]any `yaml:"job"`
	}
	text := read(t, clone, repotemplate.JobSpec)
	if err := yaml.Unmarshal([]byte(text), &spec); err != nil || spec.Job["accelerator_type"] != nil || spec.Job["accelerator_count"] != nil || spec.Job["cpu"] != 8 ||
		!strings.Contains(text, "runs on CPU cores") || strings.Contains(text, "hl capacity`. Work that needs no accelerator") {
		t.Fatalf("the job spec for work on cores = %v, %v\n%s", spec.Job, err, text)
	}
}

// hl repo init kept the job spec and the README of an earlier experiment of the repository,
// and the platform creates a repository with the unfilled template committed: neither is the
// agent's work, so both are replaced, by the one test hl repo check fails such a file on.
func TestRepoInitReplacesTheFilesOfAnotherExperimentAndTheUnfilledTemplate(t *testing.T) {
	clone := cloneOf(t, proposed)
	write(t, clone, repotemplate.ExperimentFile, filledExperiment)
	write(t, clone, repotemplate.JobSpec, filledJobSpec)
	write(t, clone, repotemplate.Readme, "# sweep\n")
	write(t, clone, "train.py", "print(1)\n")
	code, out := repo(t, "init")
	for _, want := range []string{
		"replaced " + repotemplate.ExperimentFile + ": it is written from platform experiment pe-1",
		"replaced " + repotemplate.JobSpec + `: the clone's experiment file states name "sweep", and platform experiment pe-1 is "ResNet-18: throughput"`,
		"replaced " + repotemplate.Readme + ": the clone's experiment file states name",
		"added    " + repotemplate.MetricsHelper,
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("hl repo init = %d, want %q in\n%s", code, want, out)
		}
	}
	if spec := read(t, clone, repotemplate.JobSpec); !strings.Contains(spec, "image: torch") || read(t, clone, "train.py") != "print(1)\n" {
		t.Fatalf("the job spec was not replaced, or code was touched:\n%s", spec)
	}
	if staged := mustGit(t, clone, "status", "--porcelain"); strings.Contains(staged, "A ") {
		t.Fatalf("hl repo init staged files:\n%s", staged)
	}

	clone = cloneOf(t, proposed)
	for path, content := range repotemplate.Files() {
		write(t, clone, path, content)
	}
	code, out = repo(t, "init")
	for _, want := range []string{"replaced " + repotemplate.JobSpec + ": it was the unfilled template", "replaced " + repotemplate.Readme + ": it was the unfilled template",
		"kept     " + repotemplate.MetricsHelper, "kept     " + repotemplate.Gitignore} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("hl repo init over the unfilled template = %d, want %q in\n%s", code, want, out)
		}
	}
}

// The first branch pushed to an empty repository became its default branch: a run's repository
// had hl/pe-.../baseline for one and no main.
func TestRepoInitGivesAnEmptyRepositoryItsDefaultBranchFirst(t *testing.T) {
	clone := cloneOf(t, proposed)
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, clone, "init", "-q", "--bare", origin)
	mustGit(t, clone, "remote", "add", "origin", origin)
	mustGit(t, clone, "checkout", "-q", "-b", "hl/pe-1/baseline")
	for name, value := range map[string]string{"GIT_AUTHOR_NAME": "ag-1", "GIT_AUTHOR_EMAIL": "ag-1@t", "GIT_COMMITTER_NAME": "ag-1", "GIT_COMMITTER_EMAIL": "ag-1@t"} {
		t.Setenv(name, value)
	}
	code, out := repo(t, "init")
	if code != 0 || !strings.HasPrefix(out, "pushed main: the repository had no branch") {
		t.Fatalf("hl repo init on an empty repository = %d\n%s", code, out)
	}
	if heads := mustGit(t, origin, "for-each-ref", "--format=%(refname)", "refs/heads"); heads != "refs/heads/main" {
		t.Fatalf("origin has %q, want main alone", heads)
	}
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-q", "-m", "the harness")
	if parent, main := mustGit(t, clone, "rev-parse", "HEAD~1"), mustGit(t, origin, "rev-parse", "refs/heads/main"); parent != main {
		t.Fatalf("the baseline's branch starts from %s, want origin's main %s", parent, main)
	}
	if code, out := repo(t, "init"); code != 0 || strings.Contains(out, "pushed") {
		t.Fatalf("a second hl repo init = %d\n%s", code, out)
	}
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

// An empty repository gets every file of the template, and one with code keeps what it has: a
// file of the agent's own is never written. Nothing is staged or committed.
func TestRepoInitAddsOnlyTheFilesTheCloneLacks(t *testing.T) {
	clone := emptyClone(t)
	write(t, clone, "README.md", "ours\n")
	write(t, clone, "train.py", "print(1)\n")
	code, out := repo(t, "init")
	if code != 0 || !strings.Contains(out, "kept     README.md") || !strings.Contains(out, "added    "+repotemplate.JobSpec) || !strings.Contains(out, "added    "+repotemplate.ExperimentFile) || !strings.Contains(out, "added    "+repotemplate.Gitignore) || !strings.Contains(out, "added    "+repotemplate.MetricsHelper) {
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
	for _, name := range []string{"readme: README.md still has", "job-spec: " + repotemplate.JobSpec + " still has"} {
		if code != 1 || !strings.Contains(out, name) || strings.Contains(out, "experiment: ") {
			t.Fatalf("the unfilled template: hl repo check = %d, want %q in\n%s", code, name, out)
		}
	}
	write(t, clone, "README.md", "# sweep\n")
	write(t, clone, repotemplate.ExperimentFile, filledExperiment)
	write(t, clone, repotemplate.JobSpec, filledJobSpec)
	// The files are whole and no code spells out the metric the experiment ranks on. A harness
	// that built its keys, f"val_{name}", was failed for that and given a dummy literal to pass:
	// the check says it and fails nothing.
	write(t, clone, "train.py", "post_metric(1.0, accuracy, f\"val_{name}\")\n")
	if code, out := repo(t, "check"); code != 0 || out != metricNotSpelled("val_accuracy", "pe-1")+"\n"+metricsHelperPresent+"\n"+repoChecked("pe-1")+"\n" || !strings.Contains(out, "which fails nothing") {
		t.Fatalf("a clone whose code spells out no declared metric: hl repo check = %d, want it said and passed in\n%s", code, out)
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
		!strings.Contains(out, "\nchecked: the layout, each file's validity, and that") || !strings.HasSuffix(out, "fit platform experiment pe-1. Not checked: the README's text, the code the job spec runs and which metrics it reports.\n") {
		t.Fatalf("a filled clone: hl repo check = %d\n%s", code, out)
	}
	for name, tc := range map[string]struct{ path, content, want string }{
		// `hl repo check` passed a repository whose files described a compression benchmark,
		// straight after `hl repo init` had kept all three.
		"an earlier experiment's name":    {repotemplate.ExperimentFile, strings.Replace(filledExperiment, "sweep", "compression", 1), "experiment: " + repotemplate.ExperimentFile + ` states name "compression", and platform experiment pe-1 is "sweep": the file is another experiment's, and hl repo init writes this one's`},
		"an earlier experiment's metrics": {repotemplate.ExperimentFile, strings.Replace(filledExperiment, "val_accuracy\n    direction: maximize", "ratio\n    direction: minimize\n    role: ranking", 1), "states the metrics ratio minimize ranking, and platform experiment pe-1 declares val_accuracy maximize ranking"},
		// A job.yaml stated estimated_duration_hours: 0.05 and all twelve jobs of a run were
		// given that in place of the estimate the platform measured.
		"an estimate in the committed spec": {repotemplate.JobSpec, "metadata:\n  estimated_duration_hours: 0.05\n" + filledJobSpec, "states metadata.estimated_duration_hours 0.05: every job submitted from this file would hold that"},
		"free text with a colon in it":      {repotemplate.ExperimentFile, "name: sweep: wide\ndescription: d\nmetrics: []\n", "experiment: " + repotemplate.ExperimentFile + " is not the YAML of an experiment: line 1, column 12: mapping values are not allowed in this context, where the line reads: name: sweep: wide"},
		"no ranking metric":                 {repotemplate.ExperimentFile, "name: sweep\ndescription: d\nmetrics:\n  - key: gate\n    role: constraint\n", "has no ranking metric"},
		"a job spec that is not YAML":       {repotemplate.JobSpec, "job:\n  image: a\n command: b\n", "job-spec: " + repotemplate.JobSpec + " is not the YAML of a job spec: line 3, column 2: did not find expected key, where the line reads: command: b"},
		"a job spec with no job":            {repotemplate.JobSpec, "metadata:\n  theory: t\n", "states no job"},
		// A spec with no image passed three checks, was submitted and sat in the queue.
		"a job spec with no image":       {repotemplate.JobSpec, "job:\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n", "states no job.image: every job names the image it runs in, a job_image of hl capacity"},
		"a job spec with no cpu":         {repotemplate.JobSpec, "job:\n  image: a\n  memory: 8Gi\n  storage: 4Gi\n", "states no job.cpu: state the cores one node of the job uses, as in cpu: 2"},
		"a job spec with no memory":      {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  storage: 4Gi\n", "states no job.memory"},
		"a job spec with no storage":     {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n", "states no job.storage"},
		"memory as a share of the node":  {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: max\n  storage: 4Gi\n", "states job.memory as max"},
		"storage as a share of the node": {repotemplate.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n  storage: max\n", "states job.storage as max"},
		"a group with no memory":         {repotemplate.JobSpec, "job:\n  image: a\n  groups:\n    - name: w\n      cpu: 2\n      memory: 8Gi\n      storage: 4Gi\n    - name: ps\n      cpu: 1\n", "states no job.groups[1].memory"},
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
