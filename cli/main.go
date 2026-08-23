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
//	hl hypothesis submit --agent jane --platform-experiment pe-123 --text "..."
//	hl job submit --agent jane job.yaml
//	hl job list --agent jane
//	hl watch --experiment exp-1 --until 'status in COMPLETED,FAILED,EVICTED'
//	hl watch --platform-experiment pe-123 --kinds hypothesis.new,hypothesis.status,finding.new,comment.new
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: hl <register|signup|platform-experiments|hypothesis|job|watch> ...")
		return 2
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
		return 2
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
		"(\"guaranteed\" or \"burst_only\"); omit to use the agent's kind default")
	if err := fs.Parse(args); err != nil {
		return 2
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
		}
	}
	fs := flag.NewFlagSet("platform-experiments", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 20, "max results")
	if err := fs.Parse(args); err != nil {
		return 2
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

See controlplane/settings/examples/experiment.yaml for the full DSL reference.`)
	}
	if err := fs.Parse(args); err != nil {
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
	if err := c.Do("POST", "/platform-experiments", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
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
every field including metrics is editable; once running, only name and description are amended,
the rest is ignored.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
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

func cmdPlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("platform-experiments start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "platform experiment id (required)")
	if err := fs.Parse(args); err != nil {
		return 2
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

// --- hypothesis ------------------------------------------------------------------------------------

func cmdHypothesis(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: hl hypothesis <submit|list> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "submit":
		fs := flag.NewFlagSet("hypothesis submit", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agent := fs.String("agent", "", "agent id (required unless set in --file)")
		pe := fs.String("platform-experiment", "", "platform experiment id (required unless set in --file)")
		text := fs.String("text", "", "hypothesis text (required unless set in --file)")
		file := fs.String("file", "", "YAML file shaped like the POST /hypotheses body (see controlplane/settings/examples/hypothesis.yaml); flags above override its fields")
		fs.Usage = func() {
			fmt.Fprintln(stderr, `usage: hl hypothesis submit --agent AGENT --platform-experiment PE --text TEXT
   or: hl hypothesis submit --file hypothesis.yaml [--agent AGENT] [--platform-experiment PE] [--text TEXT]

FILE is a YAML file shaped like the POST /hypotheses body. Minimal shape:

  agent_id: agent-123
  platform_experiment_id: pe-123
  text: "the hypothesis this agent is testing"

Any flag given on the command line overrides the corresponding field in FILE.
See controlplane/settings/examples/hypothesis.yaml for the full DSL reference.`)
		}
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		body := map[string]string{}
		if *file != "" {
			raw, err := readYAMLBody(*file)
			if err != nil {
				fmt.Fprintf(stderr, "hl: %s\n", err)
				return 2
			}
			for _, key := range []string{"agent_id", "platform_experiment_id", "text", "author"} {
				if v, ok := raw[key].(string); ok && v != "" {
					body[key] = v
				}
			}
		}
		if *agent != "" {
			body["agent_id"] = *agent
		}
		if *pe != "" {
			body["platform_experiment_id"] = *pe
		}
		if *text != "" {
			body["text"] = *text
		}
		if body["agent_id"] == "" || body["platform_experiment_id"] == "" || body["text"] == "" {
			fmt.Fprintln(stderr, "hl: hypothesis submit: agent_id, platform_experiment_id and text are required (via --agent/--platform-experiment/--text or --file)")
			return 2
		}
		var out any
		if err := c.Do("POST", "/hypotheses", body, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	case "list":
		fs := flag.NewFlagSet("hypothesis list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		pe := fs.String("platform-experiment", "", "platform experiment id (required)")
		agent := fs.String("agent", "", "narrow to one agent")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *pe == "" {
			fmt.Fprintln(stderr, "hl: hypothesis list: --platform-experiment is required")
			return 2
		}
		q := "?platform_experiment_id=" + *pe
		if *agent != "" {
			q += "&agent=" + *agent
		}
		var out any
		if err := c.Do("GET", "/hypotheses"+q, nil, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	default:
		fmt.Fprintf(stderr, "hl: hypothesis: unknown subcommand %q\n", sub)
		return 2
	}
}

// --- job -------------------------------------------------------------------------------------------

func cmdJob(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: hl job <submit|list|status> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "submit":
		fs := flag.NewFlagSet("job submit", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agent := fs.String("agent", "", "stamped onto metadata.agent_id (required)")
		fs.Usage = func() {
			fmt.Fprintln(stderr, `usage: hl job submit --agent AGENT [FILE|-]

FILE is a YAML file shaped exactly like the POST /experiments body — id, metadata, job.
metadata.hypothesis_id is REQUIRED: every job tests a specific, previously-registered
hypothesis (see 'hl hypothesis submit'), there is no free-text/no-hypothesis path. Minimal shape:

  id: job-123
  metadata:
    platform_experiment_id: pe-123
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
    project_id: my-project
    objective: "maximize val_accuracy"
    code_ref: git://...
  job:
    image: ...
    accelerator_type: ...
    accelerator_count: 1
    ...

See controlplane/settings/examples/experiment-submission.yaml for the full DSL reference.`)
		}
		if err := fs.Parse(rest); err != nil {
			return 2
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
		agent := fs.String("agent", "", "agent id (required)")
		pe := fs.String("platform-experiment", "", "narrow to one platform experiment")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *agent == "" {
			fmt.Fprintln(stderr, "hl: job list: --agent is required")
			return 2
		}
		q := "?agent=" + *agent
		if *pe != "" {
			q += "&platform_experiment_id=" + *pe
		}
		var out any
		if err := c.Do("GET", "/experiments"+q, nil, &out); err != nil {
			return fail(stderr, err)
		}
		printJSON(stdout, out)
		return 0
	case "status":
		fs := flag.NewFlagSet("job status", flag.ContinueOnError)
		fs.SetOutput(stderr)
		agent := fs.String("agent", "", "agent id (required)")
		id := fs.String("id", "", "job id (required)")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *agent == "" || *id == "" {
			fmt.Fprintln(stderr, "hl: job status: --agent and --id are required")
			return 2
		}
		var resp any
		if err := c.Do("GET", "/experiments?agent="+*agent+"&limit=200", nil, &resp); err != nil {
			return fail(stderr, err)
		}
		items := itemsOf(resp)
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if idVal, _ := m["id"].(string); idVal == *id {
				printJSON(stdout, m)
				return 0
			}
		}
		fmt.Fprintf(stderr, "hl: no job %q found for agent %q\n", *id, *agent)
		return 1
	default:
		fmt.Fprintf(stderr, "hl: job: unknown subcommand %q\n", sub)
		return 2
	}
}

// itemsOf normalizes a list response that may be a bare array or an {"items": [...]} envelope.
func itemsOf(resp any) []any {
	switch v := resp.(type) {
	case []any:
		return v
	case map[string]any:
		if items, ok := v["items"].([]any); ok {
			return items
		}
	}
	return nil
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

// --- watch -----------------------------------------------------------------------------------------

func cmdWatch(apiURL string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	experiment := fs.String("experiment", os.Getenv("HYPOTHESISLOOP_EXPERIMENT_ID"), "watch one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "watch a whole platform experiment: quota, stage boundaries, agent cuts, hypothesis/comment/finding events, platform-experiment status — everything but metric.point")
	agent := fs.String("agent", "", "narrow job and quota events to this agent")
	kinds := fs.String("kinds", "", "comma-separated event kinds to subscribe to instead of the default set (e.g. hypothesis.new,hypothesis.status,finding.new,comment.new,metric.point) — see GET /watch/kinds for the full vocabulary")
	showSubscription := fs.Bool("show-subscription", false, "ask the server what this connection is subscribed to")
	until := fs.String("until", "", "stop when true; one form only: 'status in COMPLETED,FAILED,EVICTED'")
	timeout := fs.Float64("timeout", 900.0, "give up after this many seconds")
	since := fs.Int64("since", 0, "replay from this cursor before going live")
	urlFlag := fs.String("url", "", "platform API base URL (defaults to $HYPOTHESISLOOP_API_URL, then $API_URL)")
	if err := fs.Parse(args); err != nil {
		return 2
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
