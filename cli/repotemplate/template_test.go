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
	if len(files) != 4 || !strings.Contains(files[Gitignore], "__pycache__/") {
		t.Fatalf("the template holds %d files, want README.md, the experiment file, the job spec and a .gitignore for bytecode caches", len(files))
	}
	var spec struct {
		Job map[string]any `yaml:"job"`
	}
	if err := yaml.Unmarshal([]byte(files[JobSpec]), &spec); err != nil || spec.Job["image"] == nil || spec.Job["command"] == nil {
		t.Fatalf("the job spec = %v, %v, want a job with an image and a command", spec.Job, err)
	}
	// A baseline agent told that a job without an image runs in the platform's own left it out
	// on a cluster that runs the image a job names: the template names one on every cluster.
	image, _ := spec.Job["image"].(string)
	for _, said := range []string{"REQUIRED on every cluster", "job image exactly as `hl capacity` shows it", "full registry\n  # address", "uv pip install --system"} {
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
