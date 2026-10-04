package repotemplate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Run 4's baseline COPYed a vendored crate into its environment, which the platform builds with
// no build context. The template says so and is a file the platform builds.
func TestTheEnvironmentFileSaysItIsBuiltAloneAndTheCodeComesByTheClone(t *testing.T) {
	file := Files()[Environment]
	if err := domain.ValidateEnvironmentFile(file); err != nil {
		t.Fatalf("the template: %v", err)
	}
	if !strings.Contains(file, "from this file alone") || !strings.Contains(file, "clones its commit") {
		t.Errorf("the template says nothing of how it is built:\n%s", file)
	}
}

// The helper read its job's id at import, so a harness that imported it could not even be
// started on an agent's machine: a baseline agent built a stand-in module to get past it.
func TestTheMetricsHelperImportsOutsideAJobAndSaysWhyItReportsNothingThere(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MetricsHelper), []byte(Files()[MetricsHelper]), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(code string) (string, error) {
		cmd := exec.Command(python, "-c", code)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("import metrics"); err != nil {
		t.Fatalf("import metrics outside a job: %v\n%s", err, out)
	}
	out, err := run("import metrics\nmetrics.post_metric(1.0, 0.5, 'acc')")
	if err == nil || !strings.Contains(out, "RuntimeError: post_metric: $HYPOTHESISLOOP_API_URL, $HYPOTHESISLOOP_EXPERIMENT_ID, $HYPOTHESISLOOP_TOKEN not set, so this process is no job of the platform") || !strings.Contains(out, "hl job submit") {
		t.Fatalf("post_metric outside a job = %v\n%s", err, out)
	}
}

// The job spec is submitted as it is once its placeholders are filled in, and a create with
// from_repo must not take an example for an experiment: the experiment file states nothing.
func TestTheTemplateIsAJobSpecToFillInAndAnExperimentFileThatStatesNothing(t *testing.T) {
	files := Files()
	if len(files) != 6 || !strings.Contains(files[Gitignore], "__pycache__/") {
		t.Fatalf("the template holds %d files, want README.md, the experiment file, the job spec, the environment, the metrics helper and a .gitignore for bytecode caches", len(files))
	}
	if !strings.HasPrefix(files[Environment], "ARG BASE\nFROM ${BASE}\n") {
		t.Fatalf("the environment begins %q, want the two lines the platform builds on", files[Environment])
	}
	// A baseline agent retyped this file by hand from a sample: the template supplies it.
	for _, said := range []string{"def post_metric(fraction: float, value: float, metric_name: str)", "/experiments/{job['HYPOTHESISLOOP_EXPERIMENT_ID']}/metrics", "HYPOTHESISLOOP_TOKEN"} {
		if !strings.Contains(files[MetricsHelper], said) || !strings.Contains(files[Readme], "`"+MetricsHelper+"`") {
			t.Fatalf("the metrics helper lacks %q or the README does not name it", said)
		}
	}
	var spec struct {
		Job map[string]any `yaml:"job"`
	}
	if err := yaml.Unmarshal([]byte(files[JobSpec]), &spec); err != nil || spec.Job["image"] == nil || spec.Job["command"] == nil {
		t.Fatalf("the job spec = %v, %v, want a job with an image and a command", spec.Job, err)
	}
	if spec.Job["cpu"] != 2 || spec.Job["memory"] != "8Gi" || spec.Job["storage"] != "4Gi" {
		t.Fatalf("the job spec states cpu %v, memory %v and storage %v, want each a quantity: a submit without one is refused", spec.Job["cpu"], spec.Job["memory"], spec.Job["storage"])
	}
	if image, _ := spec.Job["image"].(string); !strings.HasPrefix(image, Placeholder) {
		t.Fatalf("the job spec's image is %q, want a placeholder to fill in", image)
	}
	var experiment map[string]any
	if err := yaml.Unmarshal([]byte(files[ExperimentFile]), &experiment); err != nil || len(experiment) != 0 {
		t.Fatalf("the experiment file states %v, %v, want nothing", experiment, err)
	}
	for _, path := range []string{Readme, JobSpec} {
		if !strings.Contains(files[path], Placeholder) {
			t.Errorf("%s marks nothing to fill in", path)
		}
	}
	if !strings.Contains(files[Readme], JobSpec) || !strings.Contains(files[Readme], ExperimentFile) {
		t.Error("the README does not name the other two files")
	}
}

// Run 8's control job ran under bash -lc: the login shell's /etc/profile reset the PATH the
// environment's Dockerfile set with ENV, and cargo was not found.
func TestTheJobSpecRunsItsCommandInAShellThatKeepsTheImagesPath(t *testing.T) {
	var spec struct {
		Job struct {
			Command []string `yaml:"command"`
		} `yaml:"job"`
	}
	if err := yaml.Unmarshal([]byte(Files()[JobSpec]), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Job.Command) != 3 || spec.Job.Command[0] != "bash" || spec.Job.Command[1] != "-c" {
		t.Fatalf("the job spec's command is %q, want bash -c and a script", spec.Job.Command)
	}
}
