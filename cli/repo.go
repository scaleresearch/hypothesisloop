package main

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/repotemplate"
)

var repoCommands = []subcommand{
	{"init", "add the experiment files this clone lacks", repoInit},
	{"check", "check that this clone holds the experiment files and each is valid", repoCheck},
}

const repoFiles = `  README.md                         what is measured, how one trial runs, what to vary, what must not change
  .hypothesisloop/experiment.yaml   the experiment: name, description (the brief researchers read), metrics
  .hypothesisloop/job.yaml          the base job spec, submitted unedited for every trial
  metrics.py                        how a job reports a metric: post_metric(fraction_complete, value, "key")
  .gitignore                        bytecode caches, virtual environments and editor files, kept out of a commit`

func repoInit(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("repo init", `usage: hl repo init

Run in a clone of the experiment's repository, on a branch under hl/PE/ and never the default
branch (a baseline agent's is hl/PE/baseline). Adds the files every
experiment repository holds, where the clone has none at that path:

`+repoFiles+`

A file that is there is kept as it is, and nothing is committed. A kept file may describe an
earlier experiment of this repository: read it, keep what fits yours and rewrite the rest. Each
added file marks what to fill in with REPLACE-WITH or a commented-out example. Fill them in, then
git add and commit on that branch: hl job submit pushes it. hl repo check says what is left.`, stderr)
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return parseExit(misuse(fs, "hl repo init takes no argument"))
	}
	root, err := cloneRoot()
	if err != nil {
		return fail(stderr, err)
	}
	files := repotemplate.Files()
	kept := false
	for _, path := range sortedPaths(files) {
		target := filepath.Join(root, filepath.FromSlash(path))
		if _, err := os.Lstat(target); err == nil {
			fmt.Fprintf(stdout, "kept   %s\n", path)
			kept = true
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(stderr, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fail(stderr, err)
		}
		if err := os.WriteFile(target, []byte(files[path]), 0o644); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "added  %s\n", path)
	}
	if kept {
		fmt.Fprintln(stdout, earlierExperiment)
	}
	return 0
}

const earlierExperiment = "A file that was already there may describe an earlier experiment of this repository, which hl cannot tell: read it, keep what fits yours and rewrite the rest."

func repoCheck(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("repo check", `usage: hl repo check

Run in a clone of the experiment's repository. Checks two things in the working tree: the
layout, that each of the first three files below is there, and each file's validity. Prints one
line for each failure, as NAME: what to fix, and exits 1. With none it exits 0 and prints one
line saying what it checked and what it did not. The metrics line is printed either way and
fails nothing, and so does the estimate line of a job spec that states one.

  readme      README.md is there with no REPLACE-WITH left
  experiment  .hypothesisloop/experiment.yaml states name, description and metrics, one of
              them ranked
  job-spec    .hypothesisloop/job.yaml states a job and its storage as a quantity, in each
              group of a grouped job, with no REPLACE-WITH left and no secret_env
  metrics     whether metrics.py, the Python helper hl repo init supplies, is there, and
              without it the request a job reports a metric with: a repository needs no
              helper to report
  estimate    printed when .hypothesisloop/job.yaml states metadata.estimated_duration_hours:
              every job submitted from it is given that, and with none stated the estimate
              the platform measured on the baseline job applies

Not checked: that the files describe this experiment. Those of an earlier experiment in the
same repository pass, so read them against the brief. Nor the code the job spec runs or its
baseline job, which the platform checks when the baseline is declared
(hl platform-experiments update --baseline-job).
hl repo init adds a missing file.`, stderr)
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return parseExit(misuse(fs, "hl repo check takes no argument"))
	}
	root, err := cloneRoot()
	if err != nil {
		return fail(stderr, err)
	}
	clone := os.DirFS(root)
	failures, err := notReady(clone)
	if err != nil {
		return fail(stderr, err)
	}
	metrics := metricsHelperAbsent
	if _, err := iofs.Stat(clone, repotemplate.MetricsHelper); err == nil {
		metrics = metricsHelperPresent
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fail(stderr, err)
	}
	notes := []string{metrics}
	if estimate := statedEstimate(clone); estimate != "" {
		notes = append(notes, estimate)
	}
	if len(failures) > 0 {
		fmt.Fprintln(stdout, strings.Join(append(failures, notes...), "\n"))
		return 1
	}
	fmt.Fprintln(stdout, strings.Join(notes, "\n"))
	fmt.Fprintln(stdout, repoChecked)
	return 0
}

// statedEstimate notes a job spec that states an estimate, empty for one that states none: every
// job submitted from it holds that estimate in place of the one the platform measured.
func statedEstimate(clone iofs.FS) string {
	var file struct {
		Metadata struct {
			Estimate *float64 `yaml:"estimated_duration_hours"`
		} `yaml:"metadata"`
	}
	data, err := iofs.ReadFile(clone, repotemplate.JobSpec)
	if err != nil || yaml.Unmarshal(data, &file) != nil || file.Metadata.Estimate == nil {
		return ""
	}
	return fmt.Sprintf("estimate: %s states metadata.estimated_duration_hours %g, which fails nothing: every job submitted from it is given that in place of the estimate the platform measured on the baseline job. Leave it out and the measured estimate applies; --estimated-duration-hours states one for a single submit", repotemplate.JobSpec, *file.Metadata.Estimate)
}

const (
	metricsHelperPresent = "metrics: " + repotemplate.MetricsHelper + " is there: post_metric(fraction_complete, value, \"key\") reports a metric"
	metricsHelperAbsent  = "metrics: no " + repotemplate.MetricsHelper + ", which fails nothing: a job reports a metric with POST $HYPOTHESISLOOP_API_URL/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics, Authorization: Bearer $HYPOTHESISLOOP_TOKEN, body {\"metric_name\", \"fraction_complete\", \"metric_value\"}"
)

const repoChecked = "checked: layout and file validity (" + repotemplate.Readme + ", " + repotemplate.ExperimentFile + ", " + repotemplate.JobSpec + "). Not checked: that the files describe this experiment."

func cloneRoot() (string, error) {
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("this is not a git repository: run it in your clone of the experiment's repository")
	}
	return root, nil
}

func sortedPaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// notReady lists what the files of a clone lack in layout and validity, each under the name the
// checklist gives the item.
func notReady(clone iofs.FS) ([]string, error) {
	var failures []string
	for _, item := range []struct {
		name, path string
		check      func([]byte) string
	}{
		{"readme", repotemplate.Readme, nil},
		{"experiment", repotemplate.ExperimentFile, experimentGap},
		{"job-spec", repotemplate.JobSpec, jobSpecGap},
	} {
		data, err := iofs.ReadFile(clone, item.path)
		var gap string
		switch {
		case errors.Is(err, iofs.ErrNotExist):
			gap = "is missing: hl repo init adds it"
		case err != nil:
			return nil, err
		case strings.TrimSpace(string(data)) == "":
			gap = "is empty"
		case strings.Contains(string(data), repotemplate.Placeholder):
			gap = fmt.Sprintf("still has %d %s to fill in", strings.Count(string(data), repotemplate.Placeholder), repotemplate.Placeholder)
		case item.check != nil:
			gap = item.check(data)
		}
		if gap != "" {
			failures = append(failures, item.name+": "+item.path+" "+gap)
		}
	}
	return failures, nil
}

func experimentGap(data []byte) string {
	var file struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Metrics     []struct {
			Key  string `yaml:"key"`
			Role string `yaml:"role"`
		} `yaml:"metrics"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return "is not the YAML of an experiment: " + yamlProblem(data, err)
	}
	var missing []string
	if strings.TrimSpace(file.Name) == "" {
		missing = append(missing, "name")
	}
	if strings.TrimSpace(file.Description) == "" {
		missing = append(missing, "description")
	}
	ranked := false
	for _, metric := range file.Metrics {
		if metric.Key == "" {
			return "has a metric with no key"
		}
		ranked = ranked || metric.Role == "" || metric.Role == "ranking"
	}
	if len(file.Metrics) == 0 {
		missing = append(missing, "metrics")
	}
	switch {
	case len(missing) > 0:
		return "states no " + strings.Join(missing, ", ") + ": fill it in, uncommenting the example"
	case !ranked:
		return "has no ranking metric: one metric is the goal, with role ranking and a direction"
	}
	return ""
}

func jobSpecGap(data []byte) string {
	var file struct {
		Job map[string]any `yaml:"job"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return "is not the YAML of a job spec: " + yamlProblem(data, err)
	}
	if len(file.Job) == 0 {
		return "states no job: hl job submit --help shows the shape"
	}
	if _, set := file.Job["secret_env"]; set {
		return "sets job.secret_env: a committed file holds no secret, pass --secret-env NAME at submit"
	}
	groups, _ := file.Job["groups"].([]any)
	if len(groups) == 0 {
		return storageGap("job.storage", file.Job["storage"])
	}
	for i, group := range groups {
		fields, _ := group.(map[string]any)
		if gap := storageGap(fmt.Sprintf("job.groups[%d].storage", i), fields["storage"]); gap != "" {
			return gap
		}
	}
	return ""
}

// storageGap is the submit's rule for storage: stated, and as a quantity.
func storageGap(field string, storage any) string {
	switch storage {
	case nil, "":
		return "states no " + field + ": state the scratch space one node of the job writes, as in storage: 4Gi"
	case "max":
		return "states " + field + " as max: storage is a quantity, as in storage: 4Gi, and never a share of the node"
	}
	return ""
}
