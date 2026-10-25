package domain

import (
	"bytes"
	"regexp"
	"testing"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec/templatetest"
)

// The experiment template's experiment file, once its placeholders are filled in, is a spec the
// create takes, and so is every optional key it comments out.
func TestTheTemplateExperimentFileIsASpecOnceFilledIn(t *testing.T) {
	filled := templatetest.FillExperiment("matmul-throughput", "tflops")
	for label, text := range map[string]string{
		"placeholders filled":                            filled,
		"placeholders filled, every optional key stated": templatetest.Uncomment(filled),
	} {
		if n := experimentspec.Unfilled([]byte(text)); n != 0 {
			t.Fatalf("%s: %d placeholders left", label, n)
		}
		file, err := experimentspec.Parse([]byte(text))
		if err != nil {
			t.Fatalf("%s: parse: %v", label, err)
		}
		if err := file.Spec.Validate(); err != nil {
			t.Fatalf("%s: validate: %v", label, err)
		}
		if file.Name != "matmul-throughput" || len(file.Metrics) == 0 || file.Metrics[0].Key != "tflops" || file.BudgetAcceleratorHours != 10 || file.MaxAgents != 8 || file.DurationHours != 24 {
			t.Fatalf("%s: spec = %+v, want what was filled in", label, file.Spec)
		}
		models := []string{file.CoordinatorModel}
		for _, entry := range file.Agents {
			models = append(models, entry.Model)
		}
		for _, model := range models {
			if model == "" {
				continue
			}
			if _, err := ParseModelRef(model); err != nil {
				t.Fatalf("%s: model %q: %v", label, model, err)
			}
		}
	}
	all, err := experimentspec.Parse([]byte(templatetest.Uncomment(filled)))
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Metrics) != 4 || len(all.Agents) != 2 || len(all.Stages) != 2 || all.CoordinatorModel == "" || all.AgentImage == "" || all.Placement.Branch == "" {
		t.Fatalf("every optional key stated: %+v, want each one the template comments out", all)
	}
}

// The experiment template's job spec, once filled in, decodes with no unknown key into a job the
// submit takes, and so does each shape it comments out: env, several nodes and groups.
func TestTheTemplateJobSpecIsAJobOnceFilledIn(t *testing.T) {
	filled := templatetest.FillJob()
	all := templatetest.Uncomment(filled)
	// groups replaces num_nodes and the per-node fields, so each shape is stated on its own.
	nodes := regexp.MustCompile(`(?ms)^  groups:.*`).ReplaceAllString(all, "")
	grouped := regexp.MustCompile(`(?m)^  (num_nodes|accelerator_count|cpu|memory|storage):.*\n`).ReplaceAllString(all, "")
	for label, tc := range map[string]struct {
		text string
		want func(JobSpec) bool
	}{
		"placeholders filled": {filled, func(j JobSpec) bool {
			return len(j.Command) == 2 && j.Command[1] == "train.py" && j.AcceleratorCount == 1 && j.CPU == "2"
		}},
		"env, num_nodes, max_retries and topology stated": {nodes, func(j JobSpec) bool {
			return j.Env["SEED"] == "0" && j.NumNodes == 2 && j.MaxRetries != nil && *j.MaxRetries == 1 && j.Topology != nil
		}},
		"env, max_retries, topology and groups stated": {grouped, func(j JobSpec) bool {
			return len(j.Groups) == 2 && j.NumNodes == 0 && j.MaxRetries != nil && *j.MaxRetries == 1
		}},
	} {
		if n := experimentspec.Unfilled([]byte(tc.text)); n != 0 {
			t.Fatalf("%s: %d placeholders left", label, n)
		}
		var file struct {
			Job JobSpec `yaml:"job"`
		}
		decoder := yaml.NewDecoder(bytes.NewReader([]byte(tc.text)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&file); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
		for _, validate := range []func(JobSpec) error{ValidateTopology, ValidateGroups, ValidateEnv, ValidateCommand} {
			if err := validate(file.Job); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
		}
		if !tc.want(file.Job) {
			t.Fatalf("%s: job = %+v, want what the file states", label, file.Job)
		}
	}
}
