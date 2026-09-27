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
  .gitignore                        bytecode caches, virtual environments and editor files, kept out of a commit`

func repoInit(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("repo init", `usage: hl repo init

Run in a clone of the experiment's repository, on your own branch. Adds the files every
experiment repository holds, where the clone has none at that path:

`+repoFiles+`

A file that is there is kept as it is, and nothing is committed. A kept file may describe an
earlier experiment of this repository: read it, keep what fits yours and rewrite the rest. Each
added file marks what to fill in with REPLACE-WITH or a commented-out example. Fill them in, then
git add, commit and push on your branch. hl repo check says what is left.`, stderr)
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
layout, that each file below is there, and each file's validity. Prints one line for each
failure, as NAME: what to fix, and exits 1. With none it exits 0 and prints one line saying what
it checked and what it did not.

  readme      README.md is there with no REPLACE-WITH left
  experiment  .hypothesisloop/experiment.yaml states name, description and metrics, one of
              them ranked
  job-spec    .hypothesisloop/job.yaml states a job, with no REPLACE-WITH left and no secret_env

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
	failures, err := notReady(os.DirFS(root))
	if err != nil {
		return fail(stderr, err)
	}
	if len(failures) > 0 {
		fmt.Fprintln(stdout, strings.Join(failures, "\n"))
		return 1
	}
	fmt.Fprintln(stdout, repoChecked)
	return 0
}

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
	return ""
}
