// Package templatetest holds, for tests, the files of the experiment template repository a new
// experiment's repository starts from. template/ mirrors README.md, .hypothesisloop/experiment.yaml
// and .hypothesisloop/job.yaml of hypothesisloop-experiment-template: a change there is copied here.
package templatetest

import (
	"embed"
	"io/fs"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

//go:embed all:template
var files embed.FS

// Files is the template as a repository holds it, at the paths experimentspec names.
func Files() fs.FS {
	sub, err := fs.Sub(files, "template")
	if err != nil {
		panic(err)
	}
	return sub
}

// Read is the template's file at path.
func Read(path string) string {
	data, err := fs.ReadFile(Files(), path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// FillExperiment is the template's experiment file with each placeholder filled in, for an
// experiment named name and ranked on metric, maximized.
func FillExperiment(name, metric string) string {
	return strings.NewReplacer(
		"REPLACE-WITH-A-FEW-WORDS", name,
		"REPLACE-WITH the text", "Maximize "+metric+". The text",
		"REPLACE-WITH-THE-RANKED-METRIC", metric,
		"REPLACE-WITH-MAXIMIZE-OR-MINIMIZE", "maximize",
		"budget_accelerator_hours: REPLACE-WITH-HOURS", "budget_accelerator_hours: 10",
		"REPLACE-WITH-A-COUNT", "8",
		"duration_hours: REPLACE-WITH-HOURS", "duration_hours: 24",
	).Replace(Read(experimentspec.ExperimentFile))
}

// FillJob is the template's job spec with each placeholder filled in.
func FillJob() string {
	return strings.NewReplacer(
		"REPLACE-WITH-THE-SCRIPT-OF-ONE-TRIAL", "train.py",
		"REPLACE-WITH-AN-ACCELERATOR-TYPE-HL-CAPACITY-LISTS", "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
	).Replace(Read(experimentspec.JobSpec))
}

var readmePlaceholder = regexp.MustCompile(experimentspec.Placeholder + `[-A-Z]*`)

// FillReadme is the template's README with each placeholder filled in.
func FillReadme() string {
	return readmePlaceholder.ReplaceAllString(Read(experimentspec.Readme), "filled in")
}

var commentedKey = regexp.MustCompile(`(?m)^([ \t]*)# ?([ \t]*(- |[A-Za-z_]+:))`)

// Uncomment turns each commented-out key of a template file into a stated one and leaves the
// prose comments as they are.
func Uncomment(text string) string {
	return commentedKey.ReplaceAllString(text, "$1$2")
}
