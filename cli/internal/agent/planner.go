package agent

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func PlatformExperimentsPropose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments propose", `usage: hl platform-experiments propose [--platform-experiment PE] [--check | --working] [FILE|-]

For the planner of a draft. A proposal defines the experiment and runs nothing: no job, no
code. --check runs the platform's whole check of FILE and stores nothing: it prints every
problem at once, or that the proposal passes. --working runs the same check and stores FILE as
the draft's working plan: the draft stays a draft, its baseline agent starts from it, and a later
--working or the proposal replaces it. Propose only the one you mean: a propose that passes opens
the experiment at once.
FILE is YAML of this shape, with every field shown required unless it says optional.
Every free-text value is a | block, a list item too (- |): unquoted text breaks on a colon or
a leading quote.

  goal_version: 1                      # the draft's goal_version you planned from: a proposal for
                                       # an older goal is refused with goal_changed
  experiment:
    name: |
      short title
    description: |
      the description, the only thing the baseline and the team read: the objective first, then
      the metric, gate, control, references and levers. How they are measured is the baseline's
      to choose. The platform adds the baseline job, its code_ref and values once declared.
    report_interval_seconds: 30        # how often a job is to report
    metrics:
      - key: val_accuracy              # one metric ranks: role ranking is the default. A job ranks
        direction: maximize            # by the last value it reports on it (or minimize), so a
                                       # harness's final report there is its result
        target_vs_baseline: 0.5        # optional: a goal the person stated as a change against
                                       # the baseline, 0.5 for +50%, -0.3 for 30% lower on a
                                       # metric to minimize. Left out when none was stated
        deterministic: true            # optional: the same code and data give the same value
                                       # on any machine and in every run (an exact count, size
                                       # or check, never a timing or a sampled estimate)
      - key: exact_match               # optional gate
        direction: maximize
        role: constraint
        bound: 1                       # a value the unchanged control holds by construction
      - key: other_val_accuracy        # optional: one per outside system the goal names, its own
        direction: maximize            # number apart from the control. Shown beside the standings,
        role: reference                # never ranked or gating
  accelerator_type: ...                # optional: an accelerator_type 'hl capacity' prints, copied verbatim.
                                       # Left out for work that needs no accelerator: the run is
                                       # on CPU cores (hypothesisloop.dev/cpu=core) and its
                                       # budget is core-hours. A refusal lists the valid types
  cpu: 8                               # required, like memory and storage: what one node of a job
  memory: 32Gi                         # asks for, as job.cpu, job.memory and job.storage state it.
  storage: 100Gi                       # the baseline writes them into job.yaml, so no description repeats them
  budget_accelerator_hours: 6          # positive: the cap on everything the run spends
  duration_hours: 4                    # the run, from its start: a time the goal states is this
  baseline_minutes: 30                 # what measuring the baseline needs from the proposal, from
                                       # planning.min_baseline_minutes (15) to
                                       # planning.baseline_limit_minutes
  agents:                              # the team, one entry per agent: executors, which run the
    - agent_type: hyperparameter-search # jobs and count toward max_agents, and researchers, which
      hyperparameters:                 # keep the graph, run no job and count toward nothing.
        direction: |                   # agent_type is optional: an agent type by id, left out the
          the approach this one explores: a mechanism, never a list of values to try, since
          its executor makes each claim under it a hypothesis of its own
    - agent_type: architecture-search  # built-in executor type, generalist; the others built in are
    - {}                               # hyperparameter-search, architecture-search and data-search,
    - agent_type: researcher           # and researcher for a researcher. --check with an id that is
                                       # none lists the valid ones. model is optional: a
                                       # <provider>/<model id> hl models lists, left out the agent
                                       # type's (a researcher's is the coordinator's). The direction
                                       # you give an agent goes in hyperparameters, any keys.
  metric_reason: |
    why this metric is the goal, in plain words
  measurement: |
    how it is measured, in plain words
  sizing_reason: |
    why this budget, duration and number of executors are the least that reach the goal,
    worded as the estimate it is
  out_of_scope:                        # optional, and so are risks and questions
    - |
      what you left out
  risks:
    - |
      what could make the result mislead
  questions:                           # each decision the goal left open, with the answer you
    - question: |                      # planned on: each answer is an assumption the run starts
        how big a win counts           # on, added to the description, which the person changes
      answer: |                        # if they want it otherwise
        1.5x the control's throughput, since the goal states no margin

A refusal names every problem at once and changes nothing: for a file that is no YAML the line
and what is wrong there, accelerator_unreachable with the types that are listed, and invalid_resources
for a cpu, memory or storage left out or no quantity. Without --check or --working, one that
passes opens the experiment at once and your work is done.`, stderr)
	pe := command.ExperimentFlag(fs)
	check := fs.Bool("check", false, "check the proposal and store nothing")
	working := fs.Bool("working", false, "check the proposal and store it as the draft's working plan")
	operands, err := command.ParseArgs(fs, args)
	if err != nil {
		return command.ParseExit(err)
	}
	if len(operands) > 1 {
		return command.ParseExit(command.Misuse(fs, "want at most one FILE, got %d operands", len(operands)))
	}
	if *check && *working {
		return command.ParseExit(command.Misuse(fs, "--check and --working: choose one"))
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	path := "-"
	if len(operands) == 1 {
		path = operands[0]
	}
	body, err := command.ReadYAMLBody(path)
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	quantitiesAsStrings(body)
	textsAsLists(body)
	path = "/platform-experiments/" + url.PathEscape(id) + "/proposal"
	switch {
	case *check:
		path += "?check=true"
	case *working:
		path += "?working=true"
	}
	var out any
	if err := c.Do("PUT", path, body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if *check {
		fmt.Fprintln(stdout, "the proposal passes every check; nothing is stored. Propose it without --check when it is the one you mean.")
		return 0
	}
	if *working {
		fmt.Fprintln(stdout, "stored as the working plan; the draft is still a draft. Propose without --working when it is the one you mean.")
		return 0
	}
	command.PrintJSON(stdout, out)
	return 0
}

// quantitiesAsStrings sends a proposal's cpu, memory and storage as the strings the API reads,
// where YAML reads cpu: 8 as a number.
func quantitiesAsStrings(body map[string]any) {
	for _, key := range []string{"cpu", "memory", "storage"} {
		switch v := body[key].(type) {
		case int, int64, uint64, float64:
			body[key] = fmt.Sprint(v)
		}
	}
}

// textsAsLists sends a proposal's out_of_scope or risks written as one | block as the one-item
// list the API reads. A planner was refused 422 "expected array" for questions: |.
func textsAsLists(body map[string]any) {
	for _, key := range []string{"out_of_scope", "risks"} {
		if text, ok := body[key].(string); ok {
			body[key] = []any{text}
		}
	}
}

func PlatformExperimentsDecline(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments decline", `usage: hl platform-experiments decline [--platform-experiment PE] --goal-version N --reason TEXT | --reason-file FILE|-

For the planner of a draft, when the goal cannot be made into an experiment: nothing measurable,
or nothing this platform can run. The reason is shown to the person who wrote the goal in place
of a proposal, so say in plain words why, and what would make it plannable. A proposal you made
before no longer stands. --goal-version is the draft's goal_version you answer: a decline of an
older goal is refused with goal_changed. The person may change the goal, which starts you again.`, stderr)
	pe := command.ExperimentFlag(fs)
	goalVersion := fs.Int("goal-version", 0, "the draft's goal_version the decline answers")
	reasonFlag := command.NewTextFlag(fs, "reason", "why no experiment is proposed")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	if *goalVersion < 1 {
		return command.ParseExit(command.Misuse(fs, "want --goal-version, the draft's goal_version"))
	}
	reason, err := reasonFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if strings.TrimSpace(reason) == "" {
		return command.ParseExit(command.Misuse(fs, "want --reason"))
	}
	id, err := command.NamedExperiment(fs, c, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/decline", map[string]any{"reason": reason, "goal_version": *goalVersion}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	command.PrintJSON(stdout, out)
	return 0
}
