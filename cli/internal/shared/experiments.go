package shared

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func PlatformExperimentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments list", `usage: hl platform-experiments list [--limit N] [--offset N] [--project PROJECT] [--shared[=false]]

One platform experiment per line as compact JSON, without description and summary
(hl platform-experiments get prints both). A closed one carries close_reason.
hl platform-experiments with no command is this list.`, stderr)
	limit := fs.Int("limit", 20, "page size (max 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	project := fs.String("project", "", "only this project's platform experiments (default $HL_PROJECT)")
	shared := fs.Bool("shared", false, "only the platform experiments the platform shows every account, or with --shared=false none of them. Without it both are listed, the shared ones marked \"shared\": true. Read those like your own, write nothing to them")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *offset != 0 {
		q.Set("offset", strconv.Itoa(*offset))
	}
	if p := command.OrEnv(*project, command.ProjectEnv); p != "" {
		q.Set("project_id", p)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "shared" {
			q.Set("shared", strconv.FormatBool(*shared))
		}
	})
	var out json.RawMessage
	header, err := c.DoHeader("GET", "/platform-experiments?"+q.Encode(), nil, &out)
	if err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLines(stdout, stderr, "platform-experiments list: none", out, "description", "summary"); err != nil {
		return command.Fail(stderr, err)
	}
	var shown []json.RawMessage
	_ = json.Unmarshal(out, &shown)
	command.PageNotice(stderr, "platform-experiments list", header, *offset, len(shown))
	return 0
}

// PlatformExperimentsUpdate handles `hl platform-experiments update [FIELDS.yaml]`. FIELDS.yaml
// holds keys of the experiment spec: while the platform experiment is still open, every field
// including metrics is editable; once running, only name, description, duration_hours and the
// baseline are.
func PlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments update", `usage: hl platform-experiments update [--platform-experiment PE] [--description TEXT | --description-file FILE|-] [--baseline-job JOB [--reason TEXT | --reason-file FILE|-]] [--name NAME] [FIELDS.yaml]

Changes the fields you name and no other. A coordinator's token changes the description and
declares or raises the baseline, nothing else.

--baseline-job JOB declares the baseline while the platform experiment is open. The platform
writes JOB, its code_ref and what it measured into a block of the description it keeps through
later changes, and starts the run in that request when a researcher has joined and no starts_at
is ahead. The line printed after says whether it started, with status_reason. Refusals:
no_baseline (JOB is no baseline agent's job here), baseline_not_completed,
baseline_not_on_branch (JOB ran a branch other than hl/PE/baseline),
baseline_without_finding (no summary filed), baseline_missing_metric, baseline_breaks_constraint
and budget_below_baseline (the budget does not hold one trial per researcher at JOB's
estimate); the last two are the owner's to change. baseline_environment_differs and
baseline_not_in_environment: JOB did not run in the environment the head of the baseline branch
declares in .hypothesisloop/Dockerfile.

--baseline-job JOB --reason TEXT raises the baseline while the run goes on, to a stronger
COMPLETED job of the experiment that ran the baseline's code unchanged in a better configuration
of the control, whoever ran it on whichever branch: its commit equals the current baseline's
code_ref and only its arguments or environment differ (baseline_code_differs names both commits
otherwise). TEXT says which configuration. JOB is held to the checks above but the agent's role and the branch, and must be
strictly better than the baseline on the first ranking metric (baseline_not_stronger). The
description's measured block is rewritten from JOB, the raise is kept in baseline_raises and the
record of the close, every agent is told (platform_experiment.baseline), and hl results sets the
standings against JOB from then on. Verdicts already given stay as recorded. Refused without
--reason (baseline_reason_required), with --reason while the experiment is open
(baseline_not_running: the baseline agent's job is declared then) and once it is closed.

FIELDS.yaml maps the other fields that change (budget_accelerator_hours, max_agents,
researchers, duration_hours, metrics, report_interval_seconds): any while it is open, only name,
description, duration_hours, max_agents and researchers once it runs. The result is checked like a create, so name and description stay
non-empty. Once it runs, max_agents below the researchers at work cuts the newest of them
(agent.cut with detail max_agents_lowered) and nobody takes their places; before that it is
409 max_agents_below_researchers, which says how many are signed up.`, stderr)
	pe := command.ExperimentFlag(fs)
	name := fs.String("name", "", "the new name")
	descriptionFlag := command.NewTextFlag(fs, "description", "the new description")
	baselineJob := fs.String("baseline-job", "", "the completed `job` that measured the baseline")
	reasonFlag := command.NewTextFlag(fs, "reason", "with --baseline-job on a running experiment: why the baseline is raised to it")
	operands, err := command.ParseArgs(fs, args)
	if err != nil {
		return command.ParseExit(err)
	}
	if len(operands) > 1 {
		return command.ParseExit(command.Misuse(fs, "want at most one FIELDS.yaml, got %d operands: the platform experiment is --platform-experiment", len(operands)))
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	description, err := descriptionFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	reason, err := reasonFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	body := map[string]any{}
	if len(operands) == 1 {
		if operands[0] == "-" {
			if err := fs.ClaimStdin("FIELDS.yaml -"); err != nil {
				return command.ParseExit(err)
			}
		}
		if body, err = command.ReadYAMLBody(operands[0]); err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
	}
	if *name != "" {
		body["name"] = *name
	}
	if description != "" {
		body["description"] = description
	}
	if *baselineJob != "" {
		body["baseline_job_id"] = *baselineJob
	}
	if reason != "" {
		body["baseline_reason"] = reason
	}
	if len(body) == 0 {
		return command.ParseExit(command.Misuse(fs, "nothing to change: name a field"))
	}
	var out map[string]any
	if err := c.Do("PATCH", "/platform-experiments/"+url.PathEscape(id), body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	command.PrintJSON(stdout, out)
	switch {
	case reason != "":
		fmt.Fprintf(stderr, "hl: platform-experiments update: the baseline is raised to job %v, every agent is told (platform_experiment.baseline), and results set the standings against it\n", out["baseline_job_id"])
	case *baselineJob != "":
		fmt.Fprintln(stderr, "hl: platform-experiments update: "+baselineDeclared(out))
	}
	return 0
}

// baselineDeclared says of the platform experiment a declaration answered whether its run
// started, and why it is in the status it is in.
func baselineDeclared(experiment map[string]any) string {
	started := "has not started"
	if experiment["status"] == "running" {
		started = "started"
	}
	reason, _ := experiment["status_reason"].(map[string]any)
	return fmt.Sprintf("the baseline is declared and the run %s (%v): %v", started, reason["reason"], reason["message"])
}

func PlatformExperimentsGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments get", `usage: hl platform-experiments get [--platform-experiment PE]

The platform experiment: its status, description, metrics, stages and current_stage, and in
consumed_accelerator_hours what it spent of its budget. While it is open, stages are the ones
declared. From the start they are the ones the run has: a single stage of its whole length
where fewer than 5 researchers joined, since a boundary cuts nobody from so few. One moment says when the platform
closes it if nobody does: planning_ends_at on a draft being planned, proposal_expires_at once
its planner answered, baseline_due_at while it is open, and ends_at, which is starts_at plus
duration_hours, once it runs. status_reason says where one that is not closed stands: reason
is the code, message the sentence, and waits_for whose move it is: owner, coordinator, or
platform when it is nobody's and the platform, an agent at work or the clock brings the next
state. The reasons:
  a draft: repository_creating, planning, revising, accepting, proposed, declined
  open, its baseline not declared: baseline_measuring, baseline_declaring, baseline_required,
    baseline_undeclared
  open, the declaration of the measured job refused: no_baseline, baseline_not_completed,
    baseline_not_on_branch,
    baseline_without_finding, baseline_missing_metric, baseline_breaks_constraint,
    budget_below_baseline
  open, its baseline declared: starts_at_ahead, no_researchers, budget_spent, starting
  running: agents_starting while no researcher's machine is up yet, then running
  running, every researcher finished, cut or stopped for good: researchers_done
An agent of the roster adds instance_starting (hl platform-experiments agents list).
Once the baseline is declared, baseline_branch and baseline_code_ref name the branch and commit
of its job, where its harness is: the default branch may hold none of it.
prior_runs lists every closed run of the same project and repository that ran, latest first,
with its best on its primary metric. Each is read as your own with --platform-experiment ID on
any read: its summary, results, jobs and hypotheses. continues names the one this run carries on, whose
baseline_code_ref is the commit this one's baseline starts from.
On an open one the message begins with your own next step, worded for your token: the command
a coordinator or a person runs, what a baseline agent measures, or that a researcher's jobs
are refused until the start. status_reason is printed ahead of the record with id and status,
and nothing is written beside the JSON, so the output parses whole.`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	var out map[string]any
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if job, _ := out["baseline_job_id"].(string); job != "" {
		var baseline struct {
			Branch  string `json:"branch"`
			CodeRef string `json:"code_ref"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(job), nil, &baseline); err != nil {
			return command.Fail(stderr, fmt.Errorf("reading the baseline job %s: %w", job, err))
		}
		out["baseline_branch"], out["baseline_code_ref"] = baseline.Branch, baseline.CodeRef
	}
	command.PrintLeading(stdout, out, "id", "status", "status_reason")
	return 0
}

func PlatformExperimentsHealth(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments health", `usage: hl platform-experiments health [--platform-experiment PE]

Where each agent of the platform experiment, and its sandboxed coordinator, last stood, one line
each as compact JSON, as the metrics store recorded it while the run was open or running:
first_job_seconds and first_completed_job_seconds from when it could first submit (absent
before there is one), jobs_in_flight, restarts of its machine over the whole run,
coordinator_messages sent to it, and for a sandboxed agent rate_limit_utilization, the share of
its model account's rate limit used, and while the model provider refuses its calls
rate_limited_until, when the limit resets: it waits until then and resumes. The coordinator's
line has role coordinator and no agent_id.`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	var out struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id)+"/health", nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLines(stdout, stderr, "platform-experiments health: nothing recorded yet", out.Agents); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func PlatformExperimentsClose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments close", `usage: hl platform-experiments close [--reason TEXT | --reason-file FILE|-] [--platform-experiment PE]

Ends the platform experiment for good: every job still running
is evicted, nothing more is accepted, and the standings are taken from the metrics reported. Its
summary can still be written afterwards. --reason says why: it is kept as close_message, written
in the platform's record of the close, and told to the coordinator and the agents with the close.`, stderr)
	reasonFlag := command.NewTextFlag(fs, "reason", "why it is closed, in a sentence or two")
	pe := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	reason, err := reasonFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	body := map[string]any{}
	if reason != "" {
		body["reason"] = reason
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/close", body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	command.PrintJSON(stdout, out)
	return 0
}

func PlatformExperimentsSummary(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments summary", `usage: hl platform-experiments summary [--platform-experiment PE] --text T | --text-file FILE|-

Writes the summary of the platform experiment: what the run
found and what the next one should do differently. Each write replaces the one before, so write
the whole text; hl platform-experiments get shows it as summary. It can be written while the
experiment runs and after it closed.`, stderr)
	summary := command.NewTextFlag(fs, "text", "the summary")
	fs.StringVar(summary.File, "file", "", "")
	pe := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	text, err := summary.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if strings.TrimSpace(text) == "" {
		return command.ParseExit(command.Misuse(fs, "want --text or --text-file"))
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": text}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	command.PrintJSON(stdout, out)
	return 0
}
