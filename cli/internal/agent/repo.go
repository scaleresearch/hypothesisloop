package agent

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/contract/jobspec"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

// agentRole is the role in pe of the agent the token is, empty for a caller that is no agent.
func agentRole(c *apiclient.Client, pe string) (platformapi.SignupRole, error) {
	kind, _, agent := apiclient.ScopeOf(c.Token)
	if kind != apiclient.CallerAgent {
		return "", nil
	}
	var signup struct {
		Role platformapi.SignupRole `json:"role"`
	}
	if err := c.Do("GET", apiclient.SignupPath(pe, agent), nil, &signup); err != nil {
		return "", err
	}
	return signup.Role, nil
}

// missingFileHint is how the caller gets a missing experiment file: the baseline and anyone who
// is no agent copy it from the experiment template, and every other agent gets the baseline's.
func missingFileHint(role platformapi.SignupRole) string {
	if role == "" || role == platformapi.SignupRoleBaseline {
		return "the experiment template holds it, which hl repo init DIR puts in DIR to copy from"
	}
	return "it comes with the baseline's commit: hl hypothesis checkout ID puts your clone on a branch made from it"
}

func RepoCheck(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("repo check", `usage: hl repo check [--platform-experiment PE]

Run in a clone of the experiment's repository. Checks the working tree against the platform
experiment: that each of the first three files below is there and valid, and that what they
state fits it. Prints one line for each failure, as NAME: what to fix and why, and exits 1.
With none it exits 0 and prints one line saying what it checked and what it did not. The
helper line is printed either way and fails nothing.

  readme      README.md is there with no REPLACE-WITH left
  experiment  .hypothesisloop/experiment.yaml is a valid experiment spec, as
              hl platform-experiments start checks one, and its name and metrics are the
              platform experiment's: a file left by another experiment of the repository
              fails here
  job-spec    .hypothesisloop/job.yaml states a job, its image, and its cpu, memory and
              storage as quantities, in each group of a grouped job, with no REPLACE-WITH
              left, no secret_env and no metadata.estimated_duration_hours: every job
              submitted from it would hold that in place of the estimate measured on the
              baseline job. A missing image is answered with the one the experiment was
              proposed with
  metrics     every metric the platform experiment declares is named, by its key, in a file
              of the clone besides those two: a job reports each under its key
  helper      whether metrics.py, the Python helper of the experiment template, is there, and
              without it the request a job reports a metric with: a repository needs no
              helper to report

Not checked: the README's text, the code the job spec runs or its baseline job, which the
platform checks when the baseline is declared (hl platform-experiments update --baseline-job).
The baseline copies a missing file from the experiment template, which hl repo init DIR puts in
DIR; any other agent gets it with the baseline's commit (hl hypothesis checkout ID).`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() != 0 {
		return command.ParseExit(command.Misuse(fs, "hl repo check takes no argument"))
	}
	root, err := cloneRoot()
	if err != nil {
		return command.Fail(stderr, err)
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	var experiment declaredExperiment
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &experiment); err != nil {
		return command.Fail(stderr, err)
	}
	role, err := agentRole(c, id)
	if err != nil {
		return command.Fail(stderr, err)
	}
	clone := os.DirFS(root)
	failures, err := notReady(clone, experiment, missingFileHint(role))
	if err != nil {
		return command.Fail(stderr, err)
	}
	// What follows fails nothing. Code may build a metric's key, so a key no file spells out is
	// said and not refused.
	var notes []string
	for _, metric := range experiment.Metrics {
		named, err := namedInClone(root, metric.Key)
		if err != nil {
			return command.Fail(stderr, err)
		}
		if !named {
			notes = append(notes, metricNotSpelled(metric.Key, id))
		}
	}
	helper := metricsHelperAbsent
	if _, err := iofs.Stat(clone, experimentspec.MetricsHelper); err == nil {
		helper = metricsHelperPresent
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return command.Fail(stderr, err)
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

// declaredExperiment is what a clone's files are held to: the platform experiment's spec, and
// the image of the proposal it was accepted from, when it has one.
type declaredExperiment struct {
	experimentspec.Spec
	ID string `json:"id"`
	// Continues is the run this one carries on, whose files a clone of its baseline holds.
	Continues string `json:"continues"`
	Proposal  struct {
		Image string `json:"image"`
	} `json:"proposal"`
}

// notOf is how file differs from experiment in what names an experiment, its name and its
// metrics, "" when both are experiment's. It is the one test of a file left by another
// experiment of the repository: hl repo check fails on it. The
// description is left out, since it changes as the run learns. A run that continues another starts
// from that one's files under a name of its own, so only the metrics tell it apart.
func notOf(file experimentspec.Spec, experiment declaredExperiment) string {
	if name, declared := strings.TrimSpace(file.Name), strings.TrimSpace(experiment.Name); name != declared && experiment.Continues == "" {
		return fmt.Sprintf("states name %q, and platform experiment %s is %q", name, experiment.ID, declared)
	}
	if own, declared := stated(file.Metrics), stated(experiment.Metrics); own != declared {
		return fmt.Sprintf("states the metrics %s, and platform experiment %s declares %s", own, experiment.ID, declared)
	}
	return ""
}

// metricLine is the metric as a failure names it, with the role a metric that states none has.
func metricLine(m experimentspec.MetricDefinition) string {
	stated := m.Key + " " + m.Direction + " " + string(m.EffectiveRole())
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
func stated(metrics []experimentspec.MetricDefinition) string {
	names := make([]string, len(metrics))
	for i, metric := range metrics {
		names[i] = metricLine(metric)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// namedInClone reports whether a file of the clone besides the README and the experiment file
// contains text. It reads tracked and untracked files alike and none that .gitignore leaves out.
func namedInClone(root, text string) (bool, error) {
	cmd := exec.Command("git", "grep", "--untracked", "-q", "-F", "-e", text, "--", ".", ":(exclude)"+experimentspec.Readme, ":(exclude)"+experimentspec.ExperimentFile)
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
	metricsHelperPresent = "helper: " + experimentspec.MetricsHelper + " is there: post_metric(fraction_complete, value, \"key\") reports a metric"
	metricsHelperAbsent  = "helper: no " + experimentspec.MetricsHelper + ", which fails nothing: a job reports a metric with POST $HYPOTHESISLOOP_API_URL/experiments/$HYPOTHESISLOOP_EXPERIMENT_ID/metrics, Authorization: Bearer $HYPOTHESISLOOP_TOKEN, body {\"metric_name\", \"fraction_complete\", \"metric_value\"}"
)

// metricNotSpelled says of a declared metric that no code of the clone spells its key out.
func metricNotSpelled(key, id string) string {
	return fmt.Sprintf("metrics: no file of the clone besides %s and %s spells out %s, a metric platform experiment %s declares, which fails nothing: code that builds its keys may still report it. A job reports each declared metric under its key, as in post_metric(fraction_complete, value, %q)", experimentspec.Readme, experimentspec.ExperimentFile, key, id, key)
}

func repoChecked(id string) string {
	return "checked: the layout, each file's validity, and that " + experimentspec.ExperimentFile + " and " + experimentspec.JobSpec + " fit platform experiment " + id + ". Not checked: the README's text, the code the job spec runs and which metrics it reports."
}

func cloneRoot() (string, error) {
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("this is not a git repository: run it in your clone of the experiment's repository")
	}
	return root, nil
}

// notReady lists what the files of a clone lack in layout, validity and fit to experiment, each
// under the name the checklist gives the item. missing says how the caller gets a missing file.
func notReady(clone iofs.FS, experiment declaredExperiment, missing string) ([]string, error) {
	var failures []string
	for _, item := range []struct {
		name, path string
		check      func([]byte) string
	}{
		{"readme", experimentspec.Readme, nil},
		{"experiment", experimentspec.ExperimentFile, func(data []byte) string { return experimentGap(data, experiment) }},
		{"job-spec", experimentspec.JobSpec, func(data []byte) string { return jobSpecGap(data, experiment) }},
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
		case strings.Contains(string(data), experimentspec.Placeholder):
			gap = fmt.Sprintf("still has %d %s to fill in", strings.Count(string(data), experimentspec.Placeholder), experimentspec.Placeholder)
		case item.check != nil:
			gap = item.check(data)
		}
		if gap != "" {
			failures = append(failures, item.name+": "+item.path+" "+gap)
		}
	}
	return failures, nil
}

// experimentGap is what keeps the experiment file from being a valid spec, and for a valid one how
// it differs from the platform experiment: a file an earlier experiment of the repository left
// states another name or other metrics.
func experimentGap(data []byte, experiment declaredExperiment) string {
	file, err := experimentspec.Parse(data)
	if err == nil {
		err = file.Spec.Validate()
	}
	if err != nil {
		return "is no valid experiment spec: " + strings.ReplaceAll(err.Error(), "\n", "; ") + ": state the platform experiment's spec, which hl platform-experiments get answers"
	}
	if another := notOf(file.Spec, experiment); another != "" {
		return another + ": the file is another experiment's, state this one's"
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
		return "is not the YAML of a job spec: " + command.YAMLProblem(data, err)
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

// quantitiesGap is what the submit refuses of the cpu, memory and storage of one node.
func quantitiesGap(owner string, fields map[string]any) string {
	stated := func(name string) string {
		if value, set := fields[name]; set && value != nil {
			return fmt.Sprint(value)
		}
		return ""
	}
	if err := jobspec.CheckNode(owner, stated("cpu"), stated("memory"), stated("storage")); err != nil {
		return "is refused at submit: " + err.Error()
	}
	return ""
}
