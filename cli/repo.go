package main

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/repotemplate"
)

var repoCommands = []subcommand{
	{"init", "write the experiment's files into this clone from what the platform holds", repoInit, everyone},
	{"check", "check that this clone holds the experiment files, each valid and fitting the platform experiment", repoCheck, everyone},
}

const repoFiles = `  .hypothesisloop/experiment.yaml   the experiment: name, description (the brief researchers read) and
                                    metrics, written whole from the platform experiment
  .hypothesisloop/job.yaml          the base job spec, submitted unedited for every trial, with the image,
                                    the accelerator_type and the cpu, memory and storage its proposal states
  .hypothesisloop/Dockerfile        system packages and toolchains
  README.md                         what is measured, how one trial runs, what to vary, what must not
                                    change, with the experiment's name and what its proposal says is measured
  metrics.py                        how a job reports a metric: post_metric(fraction_complete, value, "key")
  .gitignore                        bytecode caches, virtual environments and editor files, kept out of a commit`

func repoInit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("repo init", `usage: hl repo init [--platform-experiment PE]

Run in a clone of the experiment's repository, on a branch under hl/PE/ and never the default
branch (a baseline agent's is hl/PE/baseline). Writes the files every experiment repository
holds, with what the platform holds of the experiment filled in, so none of it is typed:

`+repoFiles+`

The experiment file is always the platform's. Of the others a file that is there is kept, unless
it is still the unfilled template or the clone's experiment file stated another experiment (a
name or metrics that are not this one's, the test hl repo check applies): those belong to no
work of yours and are replaced, and the old ones stay in git. A clone of the baseline of the run
this one continues keeps its files: of that run only other metrics would be another experiment.
What is left to write is marked REPLACE-WITH: the command of one trial and the README's account
of the harness. On a repository with no branch yet it first pushes an empty commit as main, so
that the default branch is main and no experiment's. Nothing else is committed: git add and
commit on your branch, and hl job submit pushes it. hl repo check says what is left.
Run again it changes nothing that is already written and says so, so it is safe to repeat. A
failure says which step failed and what the platform or git answered, and writes nothing.
Of the agents only the baseline runs it: every other one gets the files with the baseline's
commit (hl hypothesis checkout ID).`, stderr)
	pe := experimentFlag(fs)
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
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	if err := baselineOnly(c, id); err != nil {
		return fail(stderr, fmt.Errorf("repo init: %w", err))
	}
	var experiment declaredExperiment
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &experiment); err != nil {
		return fail(stderr, fmt.Errorf("repo init: reading platform experiment %s: %w", id, err))
	}
	started, err := startDefaultBranch(root)
	if err != nil {
		return fail(stderr, fmt.Errorf("repo init: %w. Nothing was written, and hl repo init can be run again", err))
	}
	if started {
		fmt.Fprintf(stdout, "pushed %s: the repository had no branch, and its default branch is no experiment's\n", defaultBranch)
	}
	another, err := anotherExperiment(root, experiment)
	if err != nil {
		return fail(stderr, fmt.Errorf("repo init: %w", err))
	}
	template := repotemplate.Files()
	files, err := experimentFiles(template, experiment)
	if err != nil {
		return fail(stderr, fmt.Errorf("repo init: %w", err))
	}
	written := started
	for _, path := range sortedPaths(files) {
		target := filepath.Join(root, filepath.FromSlash(path))
		existing, err := os.ReadFile(target)
		did := "replaced " + path + ": "
		switch {
		case errors.Is(err, os.ErrNotExist):
			did = "added    " + path
		case err != nil:
			return fail(stderr, err)
		case string(existing) == files[path]:
			did = ""
		case path == repotemplate.ExperimentFile:
			did += "it is written from platform experiment " + id
		case another != "" && (path == repotemplate.JobSpec || path == repotemplate.Readme):
			did += "the clone's experiment file " + another
		case string(existing) == template[path]:
			did += "it was the unfilled template"
		default:
			did = ""
		}
		if did == "" {
			fmt.Fprintf(stdout, "kept     %s\n", path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fail(stderr, err)
		}
		if err := os.WriteFile(target, []byte(files[path]), 0o644); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, did)
		written = true
	}
	if !written {
		fmt.Fprintf(stdout, "already initialised for platform experiment %s: every file is there, as written before or as your work left it, and nothing was changed. hl repo check says what is left\n", id)
		return 0
	}
	fmt.Fprintf(stdout, "initialised for platform experiment %s: git add and commit on your branch, and hl repo check says what is left\n", id)
	return 0
}

// baselineOnly refuses an agent that is not the baseline of platform experiment pe: the baseline
// writes the experiment's files once, and every other agent starts from its commit.
func baselineOnly(c *apiclient.Client, pe string) error {
	role, err := agentRole(c, pe)
	if err != nil {
		return err
	}
	if role != "" && role != "baseline" {
		return fmt.Errorf("only the baseline agent writes the experiment's files, and your role is %s. Your files come with the baseline's commit: "+
			"hl hypothesis checkout ID puts your clone on a branch made from it", role)
	}
	return nil
}

// agentRole is the role in pe of the agent the token is, empty for a caller that is no agent.
func agentRole(c *apiclient.Client, pe string) (string, error) {
	kind, _, agent := scopeOf(c.Token)
	if kind != callerAgent {
		return "", nil
	}
	var signup struct {
		Role string `json:"role"`
	}
	if err := c.Do("GET", signupPath(pe, agent), nil, &signup); err != nil {
		return "", err
	}
	return signup.Role, nil
}

// missingFileHint is how the caller gets a missing experiment file: hl repo init writes it for
// the baseline and anyone who is no agent, and every other agent gets the baseline's.
func missingFileHint(role string) string {
	if role == "" || role == "baseline" {
		return "hl repo init writes it"
	}
	return "it comes with the baseline's commit: hl hypothesis checkout ID puts your clone on a branch made from it"
}

// defaultBranch is the branch hl gives a repository that has none.
const defaultBranch = "main"

// startDefaultBranch gives an origin with no branch its default branch, an empty commit, and
// starts the branch HEAD is on from it when that has no commit either. The first branch pushed
// to an empty repository becomes its default one, which an experiment's branch must never be.
func startDefaultBranch(root string) (bool, error) {
	remotes, err := git(root, "remote")
	if err != nil || !slices.Contains(strings.Fields(remotes), "origin") {
		return false, err
	}
	heads, err := git(root, "ls-remote", "--heads", "origin")
	if err != nil {
		return false, originUnreached("reading the branches of origin", err)
	}
	if heads != "" {
		return false, nil
	}
	tree, err := git(root, "mktree")
	if err != nil {
		return false, err
	}
	commit, err := git(root, "commit-tree", tree, "-m", "Start the repository")
	if err != nil {
		return false, err
	}
	if _, err := git(root, "push", "--quiet", "origin", commit+":refs/heads/"+defaultBranch); err != nil {
		// Another init, or the platform, gave origin its first branch since it was read.
		if heads, again := git(root, "ls-remote", "--heads", "origin"); again == nil && heads != "" {
			return false, nil
		}
		return false, originUnreached("pushing an empty "+defaultBranch+" to origin", err)
	}
	if _, err := git(root, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if _, err := git(root, "update-ref", "HEAD", commit); err != nil {
			return false, err
		}
	}
	return true, nil
}

// originUnreached says which step of reaching origin failed, with what git answered. Where git
// got no credential its last line blames a username prompt, and the reason is what hl
// git-credential said above it.
func originUnreached(step string, err error) error {
	said := err.Error()
	if strings.Contains(said, "git-credential") {
		return fmt.Errorf("%s failed: git got no credential for origin, as hl git-credential says: %w", step, err)
	}
	return fmt.Errorf("%s failed: %w", step, err)
}

// anotherExperiment is how the experiment file of the clone at root states an experiment that
// is not this one, "" when it states this one, nothing, or is not there.
func anotherExperiment(root string, experiment declaredExperiment) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(repotemplate.ExperimentFile)))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var file statedExperiment
	if yaml.Unmarshal(data, &file) != nil || (strings.TrimSpace(file.Name) == "" && len(file.Metrics) == 0) {
		return "", nil
	}
	return file.notOf(experiment), nil
}

// cpuType is the one accelerator_type of work on CPU cores, which a job spec leaves out.
const cpuType = "hypothesisloop.dev/cpu=core"

// The marks of the template that hl repo init fills from the platform experiment.
const (
	imageMark       = "REPLACE-WITH-A-JOB-IMAGE-NAME-HL-CAPACITY-LISTS"
	acceleratorMark = "REPLACE-WITH-AN-ACCELERATOR-TYPE-HL-CAPACITY-LISTS"
	nameMark        = "REPLACE-WITH-THE-NAME-OF-THE-EXPERIMENT"
	measuredMark    = "REPLACE-WITH the ranked metric, why it is the goal, and the gate that makes a result wrong."
	// What the template's job spec asks for one node, which a proposal that states them replaces.
	cpuLine     = "  cpu: 2\n"
	memoryLine  = "  memory: 8Gi\n"
	storageLine = "  storage: 4Gi\n"
	// acceleratorLines is what the job spec says of its accelerator, which work on cores has none of.
	acceleratorLines = "  # Copied verbatim from `hl capacity`. Work that needs no accelerator deletes both lines, and\n" +
		"  # its `cpu`, a whole number, is then the cores it holds and pays for.\n" +
		"  accelerator_type: " + acceleratorMark + "\n" +
		"  accelerator_count: 1\n"
	onCores = "  # This experiment runs on CPU cores, so the job names no accelerator: its `cpu`, a whole\n" +
		"  # number, is the cores it holds and pays for.\n"
)

// experimentFiles is the template with what the platform holds of experiment written in: the
// experiment file whole, and in the job spec and the README what its proposal states. An
// experiment created with no proposal leaves those two as the template has them.
func experimentFiles(template map[string]string, experiment declaredExperiment) (map[string]string, error) {
	files := map[string]string{}
	for path, content := range template {
		files[path] = content
	}
	stated, err := yaml.Marshal(statedExperiment{Name: experiment.Name, Description: experiment.Description, Metrics: experiment.Metrics})
	if err != nil {
		return nil, err
	}
	files[repotemplate.ExperimentFile] = "# Platform experiment " + experiment.ID + " as the platform holds it, written by hl repo init. A create with\n" +
		"# from_repo reads this file at the head of the default branch.\n" + string(stated)
	fill := func(path, mark, value string) error {
		if value == "" {
			return nil
		}
		if !strings.Contains(files[path], mark) {
			return fmt.Errorf("the template's %s has no %q to fill in", path, mark)
		}
		files[path] = strings.Replace(files[path], mark, value, 1)
		return nil
	}
	scalar := func(value string) string {
		if value == "" {
			return ""
		}
		quoted, _ := yaml.Marshal(value)
		return strings.TrimSpace(string(quoted))
	}
	proposal := experiment.Proposal
	quantity := func(name, stated string) string {
		if stated == "" {
			return ""
		}
		return "  " + name + ": " + stated + "\n"
	}
	fills := []struct{ path, mark, value string }{
		{repotemplate.Readme, nameMark, strings.TrimSpace(experiment.Name)},
		{repotemplate.JobSpec, imageMark, scalar(proposal.Image)},
		{repotemplate.JobSpec, cpuLine, quantity("cpu", proposal.CPU)},
		{repotemplate.JobSpec, memoryLine, quantity("memory", proposal.Memory)},
		{repotemplate.JobSpec, storageLine, quantity("storage", proposal.Storage)},
		{repotemplate.Readme, measuredMark, strings.TrimSpace(strings.TrimSpace(proposal.MetricReason) + "\n\n" + strings.TrimSpace(proposal.Measurement))},
	}
	if proposal.AcceleratorType == cpuType {
		fills = append(fills, struct{ path, mark, value string }{repotemplate.JobSpec, acceleratorLines, onCores})
	} else {
		fills = append(fills, struct{ path, mark, value string }{repotemplate.JobSpec, acceleratorMark, scalar(proposal.AcceleratorType)})
	}
	for _, f := range fills {
		if err := fill(f.path, f.mark, f.value); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func repoCheck(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("repo check", `usage: hl repo check [--platform-experiment PE]

Run in a clone of the experiment's repository. Checks the working tree against the platform
experiment: that each of the first three files below is there and valid, and that what they
state fits it. Prints one line for each failure, as NAME: what to fix and why, and exits 1.
With none it exits 0 and prints one line saying what it checked and what it did not. The
helper line is printed either way and fails nothing.

  readme      README.md is there with no REPLACE-WITH left
  experiment  .hypothesisloop/experiment.yaml states name, description and metrics, one of
              them ranked, and the name and the metrics are the platform experiment's: a
              file left by another experiment of the repository fails here, and hl repo
              init replaces it
  job-spec    .hypothesisloop/job.yaml states a job, its image, and its cpu, memory and
              storage as quantities, in each group of a grouped job, with no REPLACE-WITH
              left, no secret_env and no metadata.estimated_duration_hours: every job
              submitted from it would hold that in place of the estimate measured on the
              baseline job. A missing image is answered with the one the experiment was
              proposed with
  metrics     every metric the platform experiment declares is named, by its key, in a file
              of the clone besides those two: a job reports each under its key
  helper      whether metrics.py, the Python helper hl repo init supplies, is there, and
              without it the request a job reports a metric with: a repository needs no
              helper to report

Not checked: the README's text, the code the job spec runs or its baseline job, which the
platform checks when the baseline is declared (hl platform-experiments update --baseline-job).
hl repo init writes a missing file for the baseline; any other agent gets it with the baseline's
commit (hl hypothesis checkout ID).`, stderr)
	pe := experimentFlag(fs)
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
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var experiment declaredExperiment
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &experiment); err != nil {
		return fail(stderr, err)
	}
	role, err := agentRole(c, id)
	if err != nil {
		return fail(stderr, err)
	}
	clone := os.DirFS(root)
	failures, err := notReady(clone, experiment, missingFileHint(role))
	if err != nil {
		return fail(stderr, err)
	}
	// What follows fails nothing. Code may build a metric's key, so a key no file spells out is
	// said and not refused.
	var notes []string
	for _, metric := range experiment.Metrics {
		named, err := namedInClone(root, metric.Key)
		if err != nil {
			return fail(stderr, err)
		}
		if !named {
			notes = append(notes, metricNotSpelled(metric.Key, id))
		}
	}
	helper := metricsHelperAbsent
	if _, err := iofs.Stat(clone, repotemplate.MetricsHelper); err == nil {
		helper = metricsHelperPresent
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fail(stderr, err)
	}
	notes = append(notes, helper)
	if len(failures) > 0 {
		fmt.Fprintln(stdout, strings.Join(append(failures, notes...), "\n"))
		return 1
	}
	fmt.Fprintln(stdout, strings.Join(notes, "\n"))
	fmt.Fprintln(stdout, repoChecked(id))
	return 0
}

// declaredExperiment is what a clone's files are held to and written from: the platform
// experiment's name, brief and metrics, and of the proposal it was accepted from, when it has
// one, what a job spec and a README repeat.
type declaredExperiment struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Metrics     []declaredMetric `json:"metrics"`
	// Continues is the run this one carries on, whose files a clone of its baseline holds.
	Continues string `json:"continues"`
	Proposal  struct {
		AcceleratorType string `json:"accelerator_type"`
		Image           string `json:"image"`
		CPU             string `json:"cpu"`
		Memory          string `json:"memory"`
		Storage         string `json:"storage"`
		MetricReason    string `json:"metric_reason"`
		Measurement     string `json:"measurement"`
	} `json:"proposal"`
}

type declaredMetric struct {
	Key              string   `json:"key" yaml:"key"`
	Direction        string   `json:"direction" yaml:"direction"`
	Role             string   `json:"role" yaml:"role,omitempty"`
	Bound            *float64 `json:"bound" yaml:"bound,omitempty"`
	TargetVsBaseline *float64 `json:"target_vs_baseline" yaml:"target_vs_baseline,omitempty"`
	Deterministic    bool     `json:"deterministic" yaml:"deterministic,omitempty"`
}

// statedExperiment is what the experiment file of a clone states.
type statedExperiment struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Metrics     []declaredMetric `yaml:"metrics"`
}

// notOf is how the file differs from experiment in what names an experiment, its name and its
// metrics, "" when both are experiment's. It is the one test of a file left by another
// experiment of the repository: hl repo check fails on it and hl repo init replaces by it. The
// description is the brief, which changes as the run learns. A run that continues another starts
// from that one's files under a name of its own, so only the metrics tell it apart.
func (file statedExperiment) notOf(experiment declaredExperiment) string {
	if name, declared := strings.TrimSpace(file.Name), strings.TrimSpace(experiment.Name); name != declared && experiment.Continues == "" {
		return fmt.Sprintf("states name %q, and platform experiment %s is %q", name, experiment.ID, declared)
	}
	if own, declared := stated(file.Metrics), stated(experiment.Metrics); own != declared {
		return fmt.Sprintf("states the metrics %s, and platform experiment %s declares %s", own, experiment.ID, declared)
	}
	return ""
}

// String is the metric as a failure names it, with the role a metric that states none has.
func (m declaredMetric) String() string {
	role := m.Role
	if role == "" {
		role = "ranking"
	}
	stated := m.Key + " " + m.Direction + " " + role
	if m.Bound != nil {
		stated += fmt.Sprintf(" bound %g", *m.Bound)
	}
	if m.TargetVsBaseline != nil {
		stated += fmt.Sprintf(" target_vs_baseline %g", *m.TargetVsBaseline)
	}
	if m.Deterministic {
		stated += " deterministic"
	}
	return stated
}

// stated is metrics as one sorted line, which two declarations of the same metrics share.
func stated(metrics []declaredMetric) string {
	names := make([]string, len(metrics))
	for i, metric := range metrics {
		names[i] = metric.String()
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// namedInClone reports whether a file of the clone besides the README and the experiment file
// contains text. It reads tracked and untracked files alike and none that .gitignore leaves out.
func namedInClone(root, text string) (bool, error) {
	cmd := exec.Command("git", "grep", "--untracked", "-q", "-F", "-e", text, "--", ".", ":(exclude)"+repotemplate.Readme, ":(exclude)"+repotemplate.ExperimentFile)
	cmd.Dir = root
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("git grep: %w", err)
	}
	return true, nil
}

const (
	metricsHelperPresent = "helper: " + repotemplate.MetricsHelper + " is there: post_metric(fraction_complete, value, \"key\") reports a metric"
	metricsHelperAbsent  = "helper: no " + repotemplate.MetricsHelper + ", which fails nothing: a job reports a metric with POST $HYPOTHESISLOOP_API_URL/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics, Authorization: Bearer $HYPOTHESISLOOP_TOKEN, body {\"metric_name\", \"fraction_complete\", \"metric_value\"}"
)

// metricNotSpelled says of a declared metric that no code of the clone spells its key out.
func metricNotSpelled(key, id string) string {
	return fmt.Sprintf("metrics: no file of the clone besides %s and %s spells out %s, a metric platform experiment %s declares, which fails nothing: code that builds its keys may still report it. A job reports each declared metric under its key, as in post_metric(fraction_complete, value, %q)", repotemplate.Readme, repotemplate.ExperimentFile, key, id, key)
}

func repoChecked(id string) string {
	return "checked: the layout, each file's validity, and that " + repotemplate.ExperimentFile + " and " + repotemplate.JobSpec + " fit platform experiment " + id + ". Not checked: the README's text, the code the job spec runs and which metrics it reports."
}

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

// notReady lists what the files of a clone lack in layout, validity and fit to experiment, each
// under the name the checklist gives the item. missing says how the caller gets a missing file.
func notReady(clone iofs.FS, experiment declaredExperiment, missing string) ([]string, error) {
	var failures []string
	for _, item := range []struct {
		name, path string
		check      func([]byte) string
	}{
		{"readme", repotemplate.Readme, nil},
		{"experiment", repotemplate.ExperimentFile, func(data []byte) string { return experimentGap(data, experiment) }},
		{"job-spec", repotemplate.JobSpec, func(data []byte) string { return jobSpecGap(data, experiment) }},
	} {
		data, err := iofs.ReadFile(clone, item.path)
		var gap string
		switch {
		case errors.Is(err, iofs.ErrNotExist):
			gap = "is missing: " + missing
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

// experimentGap is what the experiment file lacks, and for a whole one how it differs from the
// platform experiment: a file an earlier experiment of the repository left states another name
// or other metrics.
func experimentGap(data []byte, experiment declaredExperiment) string {
	var file statedExperiment
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
		return "states no " + strings.Join(missing, ", ") + ": hl repo init writes it from the platform experiment"
	case !ranked:
		return "has no ranking metric: one metric is the goal, with role ranking and a direction"
	}
	if another := file.notOf(experiment); another != "" {
		return another + ": the file is another experiment's, and hl repo init writes this one's"
	}
	return ""
}

// imageGap is what a job that names no image is told: the image of the experiment's proposal
// when it has one, the one source of it.
func imageGap(job map[string]any, experiment declaredExperiment) string {
	if image, _ := job["image"].(string); strings.TrimSpace(image) != "" {
		return ""
	}
	if experiment.Proposal.Image == "" {
		return "states no job.image: every job names the image it runs in, a job_image of hl capacity"
	}
	return fmt.Sprintf("states no job.image: every job names the image it runs in, and platform experiment %s was proposed with image: %s", experiment.ID, experiment.Proposal.Image)
}

func jobSpecGap(data []byte, experiment declaredExperiment) string {
	var file struct {
		Metadata struct {
			Estimate *float64 `yaml:"estimated_duration_hours"`
		} `yaml:"metadata"`
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
	if gap := imageGap(file.Job, experiment); gap != "" {
		return gap
	}
	if file.Metadata.Estimate != nil {
		return fmt.Sprintf("states metadata.estimated_duration_hours %g: every job submitted from this file would hold that in place of the estimate the platform measures on the baseline job. Leave it out, and state it for one submit with --estimated-duration-hours", *file.Metadata.Estimate)
	}
	groups, _ := file.Job["groups"].([]any)
	if len(groups) == 0 {
		return quantitiesGap("job", file.Job)
	}
	for i, group := range groups {
		fields, _ := group.(map[string]any)
		if gap := quantitiesGap(fmt.Sprintf("job.groups[%d]", i), fields); gap != "" {
			return gap
		}
	}
	return ""
}

// quantitiesGap is the submit's rule for cpu, memory and storage: each stated, and as a quantity.
func quantitiesGap(owner string, fields map[string]any) string {
	for _, quantity := range []struct{ name, meaning, example string }{
		{"cpu", "the cores one node of the job uses", "cpu: 2"},
		{"memory", "the memory one node of the job uses", "memory: 8Gi"},
		{"storage", "the scratch space one node of the job writes", "storage: 4Gi"},
	} {
		field := owner + "." + quantity.name
		switch fields[quantity.name] {
		case nil, "":
			return "states no " + field + ": state " + quantity.meaning + ", as in " + quantity.example
		case "max":
			return "states " + field + " as max: " + quantity.name + " is a quantity, as in " + quantity.example + ", and never a share of the node"
		}
	}
	return ""
}
