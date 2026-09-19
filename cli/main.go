// hl — the same platform API an agent uses, as commands a human can type.
//
// Terminology (three levels, do not conflate them):
//   - platform experiment (`hl platform-experiments`, `--platform-experiment`): the research
//     program agents/humans sign up for. domain.PlatformExperiment.
//   - hypothesis (`hl hypothesis`): a registered claim scoped to one platform experiment.
//     domain.Hypothesis. Can carry comments (comment.new events) and findings (finding.new)
//     attached by the jobs that test it.
//   - job (`hl job`, `--experiment`/`--id`): a single run submitted against a hypothesis.
//     Confusingly, the Go type backing this is domain.Experiment — its own doc comment calls it
//     "an agent-submitted job within a platform experiment" — so the API/event kinds and the
//     `experiment_id` query param say "experiment" while this CLI's subcommand and flag names say
//     "job" for the same row. One platform experiment holds many hypotheses; one hypothesis can
//     have many jobs run against it.
//
// A human participant is a first-class Agent row like any other (domain.Agent.Kind) that competes
// exactly like an AI agent — there is no role to pick, only this identity and, for a job, whatever
// hyperparameters it chooses. This tool does not re-implement the API, it just calls it. There is
// no second schema for "what a job looks like" here — `hl job submit` takes a YAML file shaped
// exactly like /explore's POST /experiments body, because hand-encoding that shape as a second set
// of flags is the kind of silently-drifting copy of a contract this codebase explicitly avoids
// elsewhere. YAML rather than JSON because a human is meant to write and edit this file by hand.
//
//	hl register --id jane --name "Jane Doe" --kind human
//	hl signup --platform-experiment pe-123 --agent jane
//	hl signup --platform-experiment pe-123 --agent bot-1 --quota-tier guaranteed
//	hl hypothesis submit --agent jane --platform-experiment pe-123 --text "..." --builds-on root
//	hl hypothesis query 'MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN h.id, h.text' --param pe=pe-123
//	hl job submit --agent jane job.yaml
//	hl job list --agent jane
//	hl job list --platform-experiment pe-123
//	hl watch --experiment exp-1 --until 'status in COMPLETED,FAILED,EVICTED'
//	hl watch --platform-experiment pe-123 --kinds hypothesis.new,hypothesis.status,finding.new,comment.new
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	const usage = `usage: hl <command> ...

  register              --id ID --kind agent|human
  signup                --platform-experiment PE --agent A
  platform-experiments  [list] | get --id PE | create | start | update
  hypothesis            submit | link | comment | status | list | get | query
  job                   submit | list [--agent A] [--needs-summary] | status | metrics | summary
  watch                 --experiment JOB | --platform-experiment PE [--agent A] [--until ...]

Run hl <command> --help for its flags.`
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, usage)
		return 0
	}

	apiURL := os.Getenv("API_URL")
	client := apiclient.New(apiURL)

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "register":
		return cmdRegister(client, rest, stdout, stderr)
	case "signup":
		return cmdSignup(client, rest, stdout, stderr)
	case "platform-experiments":
		return cmdPlatformExperiments(client, rest, stdout, stderr)
	case "hypothesis":
		return cmdHypothesis(client, rest, stdout, stderr)
	case "job":
		return cmdJob(client, rest, stdout, stderr)
	case "watch":
		return cmdWatch(apiURL, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hl: unknown command %q\n", cmd)
		return 2
	}
}

func printJSON(stdout io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hl: encoding response: %s\n", err)
		return
	}
	fmt.Fprintln(stdout, string(data))
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "hl: %s\n", err)
	return 1
}

// --- register ----------------------------------------------------------------------------------

func cmdRegister(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "stable unique agent id (required)")
	name := fs.String("name", "", "human-readable display name; defaults to id")
	kind := fs.String("kind", "human", "agent or human")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		fmt.Fprintln(stderr, "hl: register: --id is required")
		return 2
	}
	body := map[string]string{"id": *id, "name": *name, "kind": *kind}
	if body["name"] == "" {
		body["name"] = *id
	}
	var out any
	if err := c.Do("POST", "/agents", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// --- signup --------------------------------------------------------------------------------------

func cmdSignup(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("signup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (required)")
	agent := fs.String("agent", "", "agent id (required)")
	quotaTier := fs.String("quota-tier", "", "override this signup's guaranteed-vs-burst-only split "+
		"(\"guaranteed\" or \"burst_only\"); omit to use the platform experiment's default_quota_tier")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *pe == "" || *agent == "" {
		fmt.Fprintln(stderr, "hl: signup: --platform-experiment and --agent are required")
		return 2
	}
	if *quotaTier != "" && *quotaTier != "guaranteed" && *quotaTier != "burst_only" {
		fmt.Fprintln(stderr, "hl: signup: --quota-tier must be \"guaranteed\" or \"burst_only\", got "+*quotaTier)
		return 2
	}
	body := map[string]string{"agent_id": *agent}
	if *quotaTier != "" {
		body["quota_tier"] = *quotaTier
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+*pe+"/signup", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// --- platform-experiments -------------------------------------------------------------------------

func cmdPlatformExperiments(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "create":
			return cmdPlatformExperimentsCreate(c, args[1:], stdout, stderr)
		case "start":
			return cmdPlatformExperimentsStart(c, args[1:], stdout, stderr)
		case "update":
			return cmdPlatformExperimentsUpdate(c, args[1:], stdout, stderr)
		case "get":
			return cmdPlatformExperimentsGet(c, args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("platform-experiments", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 20, "max results")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var out any
	if err := c.Do("GET", fmt.Sprintf("/platform-experiments?limit=%d", *limit), nil, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// cmdPlatformExperimentsCreate handles `hl platform-experiments create [FILE|-]`. FILE is a YAML
// file shaped like the POST /platform-experiments body — see
// controlplane/settings/examples/experiment.yaml for the full DSL reference.
func cmdPlatformExperimentsCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("platform-experiments create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: hl platform-experiments create [FILE|-]

FILE is a YAML file shaped exactly like the POST /platform-experiments body. Minimal shape:

  name: my-platform-experiment
  description: what this research program is testing
  budget_accelerator_hours: 10
  max_agents: 5
  report_interval_seconds: 10
  metrics:
    - key: val_accuracy
      direction: maximize

To create several platform experiments in one call, use an experiments: list -- fields set
alongside the list (e.g. a shared budget_accelerator_hours) default onto every item that
doesn't set its own:

  budget_accelerator_hours: 10
  experiments:
    - name: my-first-platform-experiment
      description: ...
    - name: my-second-platform-experiment
      description: ...

A bare top-level YAML list of mappings works the same way, without shared defaults.

See controlplane/settings/examples/experiment.yaml for the full DSL reference.`)
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	path := "-"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	items, err := readYAMLItems(path, "experiments")
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	results := make([]any, 0, len(items))
	for _, body := range items {
		if _, ok := body["starts_at"]; !ok {
			body["starts_at"] = "0001-01-01T00:00:00Z"
		}
		if _, ok := body["ends_at"]; !ok {
			body["ends_at"] = "0001-01-01T00:00:00Z"
		}
		var out any
		if err := c.Do("POST", "/platform-experiments", body, &out); err != nil {
			return fail(stderr, err)
		}
		results = append(results, out)
	}
	if len(results) == 1 {
		printJSON(stdout, results[0])
	} else {
		printJSON(stdout, results)
	}
	return 0
}

// cmdPlatformExperimentsUpdate handles `hl platform-experiments update --id ID [FILE|-]`. FILE is
// a YAML file shaped exactly like the create body (see controlplane/settings/examples/
// experiment.yaml) — while the platform experiment is still open, every field including metrics
// is editable; once running, only name and description are.
func cmdPlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("platform-experiments update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "platform experiment id (required)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: hl platform-experiments update --id ID [FILE|-]

FILE is shaped exactly like 'hl platform-experiments create's body (see
controlplane/settings/examples/experiment.yaml). While the platform experiment is still open,
every field including metrics is editable; once running, only name and description may change
and any other field that differs from the live value is rejected, so update from the file the
experiment was created with (or set the named fields to the live values).`)
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		fmt.Fprintln(stderr, "hl: platform-experiments update: --id is required")
		return 2
	}
	path := "-"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	body, err := readYAMLBody(path)
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	if _, ok := body["starts_at"]; !ok {
		body["starts_at"] = "0001-01-01T00:00:00Z"
	}
	if _, ok := body["ends_at"]; !ok {
		body["ends_at"] = "0001-01-01T00:00:00Z"
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+*id, body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("platform-experiments get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "platform experiment id (required)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		fmt.Fprintln(stderr, "hl: platform-experiments get: --id is required")
		return 2
	}
	var out any
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(*id), nil, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("platform-experiments start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "platform experiment id (required)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		fmt.Fprintln(stderr, "hl: platform-experiments start: --id is required")
		return 2
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+*id+"/start", map[string]any{}, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// --- job -------------------------------------------------------------------------------------------

func cmdJob(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: hl job <submit|list|status|metrics|summary> ...\n\nRun hl job <command> --help for its flags."
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "submit":
		fs := flag.NewFlagSet("job submit", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agent := fs.String("agent", "", "stamped onto metadata.agent_id (required)")
		fs.Usage = func() {
			fmt.Fprintln(stderr, `usage: hl job submit --agent AGENT [FILE|-]

FILE is a YAML file shaped exactly like the POST /experiments body — id, metadata, job.
metadata.hypothesis_id is REQUIRED: every job tests a specific, previously-registered
hypothesis (see 'hl hypothesis submit'), there is no free-text/no-hypothesis path. An empty
metadata.code_ref is filled with <origin url>@<HEAD sha> of the git repository in the current
directory; that repository must have no uncommitted changes to tracked files and HEAD pushed to
origin. Minimal shape:

  id: job-123
  metadata:
    platform_experiment_id: pe-123
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
    project_id: my-project
    objective: "maximize val_accuracy"
  job:
    image: ...
    accelerator_type: ...
    accelerator_count: 1
    ...

See controlplane/settings/examples/experiment-submission.yaml for the full DSL reference.`)
		}
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		if *agent == "" {
			fmt.Fprintln(stderr, "hl: job submit: --agent is required")
			return 2
		}
		path := "-"
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		}
		body, err := readYAMLBody(path)
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["agent_id"] = *agent
		if err := fillCodeRef(metadata, ""); err != nil {
			return fail(stderr, fmt.Errorf("job submit: %w", err))
		}
		body["metadata"] = metadata
		var out any
		if err := c.Do("POST", "/experiments", body, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	case "list":
		fs := flag.NewFlagSet("job list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agent := fs.String("agent", "", "narrow to one agent; omit to see every agent's jobs")
		pe := fs.String("platform-experiment", "", "narrow to one platform experiment")
		needsSummary := fs.Bool("needs-summary", false, "only finished jobs still missing a summary, the set that blocks your next submit")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		q := url.Values{}
		if *agent != "" {
			q.Set("agent", *agent)
		}
		if *pe != "" {
			q.Set("platform_experiment_id", *pe)
		}
		if *needsSummary {
			q.Set("needs_summary", "true")
		}
		var out any
		if err := c.Do("GET", "/experiments?"+q.Encode(), nil, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	case "status":
		return jobGet(c, "job status", "", rest, stdout, stderr)
	case "metrics":
		return jobGet(c, "job metrics", "/metrics", rest, stdout, stderr)
	case "summary":
		fs := flag.NewFlagSet("job summary", flag.ContinueOnError)
		fs.SetOutput(stderr)
		id := fs.String("id", "", "job id (required)")
		text := fs.String("text", "", "what changed, what the metric did, what it means (required)")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		if *id == "" || *text == "" {
			fmt.Fprintln(stderr, "hl: job summary: --id and --text are required")
			return 2
		}
		var out any
		if err := c.Do("POST", "/experiments/"+url.PathEscape(*id)+"/summary", map[string]string{"summary": *text}, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	default:
		fmt.Fprintf(stderr, "hl: job: unknown subcommand %q\n", sub)
		return 2
	}
}

// jobGet prints GET /experiments/{id}<suffix>.
func jobGet(c *apiclient.Client, name, suffix string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "job id (required)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		fmt.Fprintf(stderr, "hl: %s: --id is required\n", name)
		return 2
	}
	var out any
	if err := c.Do("GET", "/experiments/"+url.PathEscape(*id)+suffix, nil, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func readYAMLBody(path string) (map[string]any, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var body map[string]any
	if err := yaml.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if body == nil {
		return nil, fmt.Errorf("expected a YAML mapping at the top level, got nothing")
	}
	return body, nil
}

// readYAMLItems reads FILE (or stdin for "-") and returns one map per item to submit, so a single
// file can describe either one item (the historical shape) or many. Three top-level shapes are
// accepted:
//
//   - a bare mapping: one item, exactly like readYAMLBody.
//   - a bare list of mappings: each element is one item.
//   - a mapping with a listKey (e.g. "hypotheses") holding a list of mappings: each element is one
//     item, and every *other* top-level key is a default merged into any item that does not set
//     that key itself -- e.g. a shared platform_experiment_id/agent_id above a list of ideas.
func readYAMLItems(path, listKey string) ([]map[string]any, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	switch v := doc.(type) {
	case []any:
		return toMapSlice(v)
	case map[string]any:
		if list, ok := v[listKey].([]any); ok {
			items, err := toMapSlice(list)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", listKey, err)
			}
			defaults := make(map[string]any, len(v))
			for k, val := range v {
				if k != listKey {
					defaults[k] = val
				}
			}
			for _, item := range items {
				for k, val := range defaults {
					if _, exists := item[k]; !exists {
						item[k] = val
					}
				}
			}
			return items, nil
		}
		if len(v) == 0 {
			return nil, fmt.Errorf("expected a YAML mapping or list at the top level, got nothing")
		}
		return []map[string]any{v}, nil
	default:
		return nil, fmt.Errorf("expected a YAML mapping or list at the top level")
	}
}

func toMapSlice(list []any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d: expected a YAML mapping, got %T", i, e)
		}
		out = append(out, m)
	}
	return out, nil
}

// --- watch -----------------------------------------------------------------------------------------

func cmdWatch(apiURL string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	experiment := fs.String("experiment", os.Getenv("HYPOTHESISLOOP_EXPERIMENT_ID"), "watch one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "watch a whole platform experiment: quota, stage boundaries, agent cuts, hypothesis/comment/finding events, platform-experiment status — everything but metric.point")
	agent := fs.String("agent", "", "narrow job and quota events to this agent")
	kinds := fs.String("kinds", "", "comma-separated event kinds to subscribe to instead of the default set (e.g. hypothesis.new,hypothesis.status,finding.new,comment.new,metric.point) — see GET /watch/kinds for the full vocabulary")
	showSubscription := fs.Bool("show-subscription", false, "ask the server what this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'")
	timeout := fs.Float64("timeout", 540.0, "give up after this many seconds")
	since := fs.Int64("since", 0, "resume from the largest cursor printed by an earlier watch: replays from it, the events at it included, before going live")
	urlFlag := fs.String("url", "", "platform API base URL (defaults to $HYPOTHESISLOOP_API_URL, then $API_URL)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	watchURL := *urlFlag
	if watchURL == "" {
		watchURL = os.Getenv("HYPOTHESISLOOP_API_URL")
	}
	if watchURL == "" {
		watchURL = apiURL
	}

	return watchclient.Run(watchclient.Options{
		URL:                watchURL,
		Experiment:         *experiment,
		PlatformExperiment: *platformExperiment,
		Agent:              *agent,
		Kinds:              *kinds,
		ShowSubscription:   *showSubscription,
		Until:              *until,
		Timeout:            time.Duration(*timeout * float64(time.Second)),
		Since:              *since,
		Stdout:             stdout,
		Stderr:             stderr,
	})
}

// parseExit is the exit code for a flag parse error: 0 for an explicit -h or --help.
func parseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
