package repotemplate

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// The job spec is submitted as it is once its placeholders are filled in, and a create with
// from_repo must not take an example for an experiment: the experiment file states nothing.
func TestTheTemplateIsAJobSpecToFillInAndAnExperimentFileThatStatesNothing(t *testing.T) {
	files := Files()
	if len(files) != 5 || !strings.Contains(files[Gitignore], "__pycache__/") {
		t.Fatalf("the template holds %d files, want README.md, the experiment file, the job spec, the metrics helper and a .gitignore for bytecode caches", len(files))
	}
	// A baseline agent retyped this file by hand from a sample: the template supplies it.
	for _, said := range []string{"def post_metric(fraction: float, value: float, metric_name: str)", "/experiments/{EXP_ID}/metrics", "HYPOTHESISLOOP_TOKEN"} {
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
	// A baseline agent told that a job without an image runs in the platform's own left it out
	// on a cluster that runs the image a job names: the template names one on every cluster.
	image, _ := spec.Job["image"].(string)
	for _, said := range []string{"REQUIRED on every cluster", "job image exactly as `hl capacity` shows it", "full registry\n  # address", "uv pip install PACKAGE"} {
		if !strings.HasPrefix(image, Placeholder) || !strings.Contains(files[JobSpec], said) || strings.Contains(files[JobSpec], "Delete this line") {
			t.Fatalf("the job spec's image is %q and its comments lack %q or tell to leave the image out", image, said)
		}
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
