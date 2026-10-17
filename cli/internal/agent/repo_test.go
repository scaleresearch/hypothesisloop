package agent

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A job spec with no image names the one its experiment was proposed with in hl repo check. A
// submit of it is the platform's to refuse (image_required).
func TestAJobThatNamesNoImageIsToldTheImageOfItsExperiment(t *testing.T) {
	clone := cloneOf(t, proposed)
	write(t, clone, experimentspec.JobSpec, "job:\n  command: [\"python\", \"bench.py\"]\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n")
	want := "states no job.image: every job names the image it runs in, and platform experiment pe-1 was proposed with image: torch"
	if code, out := repo(t, RepoCheck); code != 1 || !strings.Contains(out, "job-spec: "+experimentspec.JobSpec+" "+want) {
		t.Fatalf("hl repo check = %d, want %q in\n%s", code, want, out)
	}
}

func repo(t *testing.T, f command.Func) (int, string) {
	t.Helper()
	code, stdout, stderr := clitest.Output(t, f)
	return code, stdout + stderr
}

// emptyClone is a repository with nothing in it, of platform experiment pe-1 as the platform
// answers it: named sweep and ranked on val_accuracy.
func emptyClone(t *testing.T) string {
	t.Helper()
	return cloneOf(t, emptyExperiment)
}

const emptyExperiment = `{"id":"pe-1","name":"sweep","description":"the brief\n","metrics":[{"key":"val_accuracy","direction":"maximize"}],` +
	`"budget_accelerator_hours":2,"max_agents":3,"duration_hours":4}`

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
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/platform-experiments/pe-1":
			_, _ = io.WriteString(w, experiment)
		case r.Method == "GET" && r.URL.Path == "/platform-experiments/pe-1/signups/ag-1":
			_, _ = io.WriteString(w, `{"agent_id":"ag-1","role":"`+role+`"}`)
		default:
			t.Errorf("hl repo asked %s %s, want only the platform experiment and the agent's signup", r.Method, r.URL.Path)
		}
	}))
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/ag-1"))
	clone := t.TempDir()
	clitest.MustGit(t, clone, "init", "-q")
	t.Chdir(clone)
	return clone
}

// proposed is platform experiment pe-1 as accepted from a planner's proposal: a name with a
// colon in it, a brief of several lines, a gate, and the image and the type its jobs run on.
const proposed = `{"id":"pe-1","budget_accelerator_hours":2,"max_agents":3,"duration_hours":4,"name":"ResNet-18: throughput\n","description":"GOAL: images per second.\n\nBASELINE\n  config: fp32\n  measured: not yet established\n",
"metrics":[{"key":"images_per_s","direction":"maximize","role":"ranking"},{"key":"top1_agreement","direction":"maximize","role":"constraint","bound":0.99}],
"proposal":{"accelerator_type":"nvidia.com/gpu.product=RTX-4090","image":"torch","cpu":"8","memory":"32Gi","metric_reason":"Throughput is what the goal asks for.\n","measurement":"Images per second over 200 batches.\n"}}`

// hl repo check sent researchers to copy a missing file the baseline writes. It says how the
// caller's role gets the file.
func TestRepoCheckSaysHowTheCallersRoleGetsAMissingFile(t *testing.T) {
	cloneAs(t, emptyExperiment, "competitor")
	if code, out := repo(t, RepoCheck); code != 1 || strings.Contains(out, "experiment template") ||
		!strings.Contains(out, "job-spec: .hypothesisloop/job.yaml is missing: it comes with the baseline's commit: hl hypothesis checkout ID") {
		t.Errorf("hl repo check by a researcher = %d, want the missing file sent to checkout:\n%s", code, out)
	}
	cloneAs(t, emptyExperiment, "baseline")
	if code, out := repo(t, RepoCheck); code != 1 || !strings.Contains(out, "job-spec: .hypothesisloop/job.yaml is missing: the experiment template holds it, which hl repo init DIR puts in DIR") {
		t.Errorf("hl repo check by the baseline = %d, want the missing file sent to the experiment template:\n%s", code, out)
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
	filledExperiment = "name: sweep\ndescription: |\n  the brief\nmetrics:\n  - key: val_accuracy\n    direction: maximize\nbudget_accelerator_hours: 2\nmax_agents: 3\nduration_hours: 4\n"
	filledJobSpec    = "job:\n  image: registry/job:1\n  command: [\"python\", \"train.py\"]\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n"
)

// The unfilled files of the experiment template: a README and a job spec with what is left to
// fill in marked, and the metrics helper.
func writeUnfilled(t *testing.T, clone string) {
	t.Helper()
	write(t, clone, experimentspec.Readme, "# "+experimentspec.Placeholder+"-THE-EXPERIMENT-NAME\n")
	write(t, clone, experimentspec.JobSpec, "job:\n  image: registry/job:1\n  command: [\""+experimentspec.Placeholder+"-THE-COMMAND-OF-ONE-TRIAL\"]\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n")
	write(t, clone, experimentspec.MetricsHelper, "def post_metric(fraction_complete, value, key): ...\n")
}

func TestRepoCheckFindsTheTopOfTheCloneFromASubdirectory(t *testing.T) {
	clone := emptyClone(t)
	writeUnfilled(t, clone)
	// It finds the top of the clone from any directory in it.
	sub := filepath.Join(clone, "src")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if code, out := repo(t, RepoCheck); code != 1 || !strings.Contains(out, "job-spec: "+experimentspec.JobSpec+" still has") {
		t.Fatalf("hl repo check from a subdirectory = %d\n%s", code, out)
	}
}

func TestRepoCheckNamesWhatAClonesFilesLack(t *testing.T) {
	clone := emptyClone(t)
	code, out := repo(t, RepoCheck)
	for _, name := range []string{"readme: README.md is missing", "experiment: " + experimentspec.ExperimentFile + " is missing", "job-spec: " + experimentspec.JobSpec + " is missing", metricsHelperAbsent} {
		if code != 1 || !strings.Contains(out, name) {
			t.Fatalf("an empty clone: hl repo check = %d, want %q in\n%s", code, name, out)
		}
	}
	writeUnfilled(t, clone)
	write(t, clone, experimentspec.ExperimentFile, filledExperiment)
	code, out = repo(t, RepoCheck)
	for _, name := range []string{"readme: README.md still has", "job-spec: " + experimentspec.JobSpec + " still has"} {
		if code != 1 || !strings.Contains(out, name) || strings.Contains(out, "experiment: ") {
			t.Fatalf("the unfilled template: hl repo check = %d, want %q in\n%s", code, name, out)
		}
	}
	write(t, clone, "README.md", "# sweep\n")
	write(t, clone, experimentspec.ExperimentFile, filledExperiment)
	write(t, clone, experimentspec.JobSpec, filledJobSpec)
	// The files are whole and no code spells out the metric the experiment ranks on. A harness
	// that built its keys, f"val_{name}", was failed for that and given a dummy literal to pass:
	// the check says it and fails nothing.
	write(t, clone, "train.py", "post_metric(1.0, accuracy, f\"val_{name}\")\n")
	if code, out := repo(t, RepoCheck); code != 0 || out != metricNotSpelled("val_accuracy", "pe-1")+"\n"+metricsHelperPresent+"\n"+repoChecked("pe-1")+"\n" || !strings.Contains(out, "which fails nothing") {
		t.Fatalf("a clone whose code spells out no declared metric: hl repo check = %d, want it said and passed in\n%s", code, out)
	}
	write(t, clone, "train.py", "post_metric(1.0, accuracy, \"val_accuracy\")\n")
	// A prepared repository, or one in another language, reports through the platform's request
	// and holds no helper: it passes, and is told the request.
	if err := os.Remove(filepath.Join(clone, experimentspec.MetricsHelper)); err != nil {
		t.Fatal(err)
	}
	if code, out := repo(t, RepoCheck); code != 0 || out != metricsHelperAbsent+"\n"+repoChecked("pe-1")+"\n" || !strings.Contains(out, "/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics") {
		t.Fatalf("a clone with no metrics helper: hl repo check = %d\n%s", code, out)
	}
	write(t, clone, experimentspec.MetricsHelper, "def post_metric(fraction_complete, value, key): ...\n")
	// A baseline agent read "ready" of another experiment's files and went on with them: a pass
	// says what was checked and what was not, in one line, and never calls the repository ready.
	if code, out := repo(t, RepoCheck); code != 0 || out != metricsHelperPresent+"\n"+repoChecked("pe-1")+"\n" || strings.Contains(out, "ready") ||
		!strings.Contains(out, "\nchecked: the layout, each file's validity, and that") || !strings.HasSuffix(out, "fit platform experiment pe-1. Not checked: the README's text, the code the job spec runs and which metrics it reports.\n") {
		t.Fatalf("a filled clone: hl repo check = %d\n%s", code, out)
	}
	for name, tc := range map[string]struct{ path, content, want string }{
		// `hl repo check` passed a repository whose files described a compression benchmark,
		// left by an earlier experiment of the repository.
		"an earlier experiment's name":    {experimentspec.ExperimentFile, strings.Replace(filledExperiment, "sweep", "compression", 1), "experiment: " + experimentspec.ExperimentFile + ` states name "compression", and platform experiment pe-1 is "sweep": the file is another experiment's, state this one's`},
		"an earlier experiment's metrics": {experimentspec.ExperimentFile, strings.Replace(filledExperiment, "val_accuracy\n    direction: maximize", "ratio\n    direction: minimize\n    role: ranking", 1), "states the metrics ratio minimize ranking, and platform experiment pe-1 declares val_accuracy maximize ranking"},
		// A job.yaml stated estimated_duration_hours: 0.05 and all twelve jobs of a run were
		// given that in place of the estimate the platform measured.
		"an estimate in the committed spec": {experimentspec.JobSpec, "metadata:\n  estimated_duration_hours: 0.05\n" + filledJobSpec, "states metadata.estimated_duration_hours 0.05: every job submitted from this file would hold that"},
		"free text with a colon in it":      {experimentspec.ExperimentFile, "name: sweep: wide\ndescription: d\nmetrics: []\n", "experiment: " + experimentspec.ExperimentFile + " is no valid experiment spec: not YAML: line 1, column 12: mapping values are not allowed in this context, where the line reads: name: sweep: wide"},
		"no ranking metric":                 {experimentspec.ExperimentFile, strings.Replace(filledExperiment, "direction: maximize", "direction: maximize\n    role: constraint\n    bound: 0.5", 1), `at least one metric must have role "ranking"`},
		"no budget":                         {experimentspec.ExperimentFile, strings.Replace(filledExperiment, "budget_accelerator_hours: 2\n", "", 1), "budget_accelerator_hours is required"},
		"a job spec that is not YAML":       {experimentspec.JobSpec, "job:\n  image: a\n command: b\n", "job-spec: " + experimentspec.JobSpec + " is not the YAML of a job spec: line 3, column 2: did not find expected key, where the line reads: command: b"},
		"a job spec with no job":            {experimentspec.JobSpec, "metadata:\n  theory: t\n", "states no job"},
		// A spec with no image passed three checks, was submitted and sat in the queue.
		"a job spec with no image":       {experimentspec.JobSpec, "job:\n  cpu: 2\n  memory: 8Gi\n  storage: 4Gi\n", "states no job.image: every job names the image it runs in, a job_image of hl capacity"},
		"a job spec with no cpu":         {experimentspec.JobSpec, "job:\n  image: a\n  memory: 8Gi\n  storage: 4Gi\n", "job.cpu is required: state the cores one node of the job uses, as in cpu: 2"},
		"a job spec with no memory":      {experimentspec.JobSpec, "job:\n  image: a\n  cpu: 2\n  storage: 4Gi\n", "job.memory is required"},
		"a job spec with no storage":     {experimentspec.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n", "job.storage is required"},
		"memory as a share of the node":  {experimentspec.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: max\n  storage: 4Gi\n", `job.memory is "max"`},
		"storage as a share of the node": {experimentspec.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n  storage: max\n", `job.storage is "max"`},
		"a group with no memory":         {experimentspec.JobSpec, "job:\n  image: a\n  groups:\n    - name: w\n      cpu: 2\n      memory: 8Gi\n      storage: 4Gi\n    - name: ps\n      cpu: 1\n", "job.groups[1].memory is required"},
		"a negative storage":             {experimentspec.JobSpec, "job:\n  image: a\n  cpu: 2\n  memory: 8Gi\n  storage: -4Gi\n", `job.storage: quantity "-4Gi" is negative`},
		"a secret in the committed spec": {experimentspec.JobSpec, filledJobSpec + "  secret_env:\n    TOKEN: x\n", "sets job.secret_env"},
	} {
		write(t, clone, experimentspec.ExperimentFile, filledExperiment)
		write(t, clone, experimentspec.JobSpec, filledJobSpec)
		write(t, clone, tc.path, tc.content)
		if code, out := repo(t, RepoCheck); code != 1 || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 2 || !strings.HasSuffix(out, metricsHelperPresent+"\n") {
			t.Errorf("%s: hl repo check = %d, want only %q in\n%s", name, code, tc.want, out)
		}
	}
}

func TestRepoCheckIsRefusedOutsideAClone(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	t.Chdir(t.TempDir())
	if code, out := repo(t, RepoCheck); code == 0 || !strings.Contains(out, "this is not a git repository") {
		t.Errorf("hl repo check outside a clone = %d\n%s", code, out)
	}
}
