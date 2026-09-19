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
// elsewhere. Its --hypothesis and --env flags override, for one submission, the fields that change
// per job. YAML rather than JSON because a human is meant to write and edit this file by hand.
//
// --agent and --platform-experiment default to $AGENT_ID and $PLATFORM_EXPERIMENT_ID, which the
// experimentator harness sets in an agent's container.
//
//	hl register --id jane --name "Jane Doe" --kind human
//	hl signup --platform-experiment pe-123 --agent jane
//	hl signup --platform-experiment pe-123 --agent bot-1 --quota-tier guaranteed
//	hl hypothesis submit --agent jane --platform-experiment pe-123 --text "..." --builds-on root
//	hl hypothesis query 'MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN h.id, h.text' --platform-experiment pe-123
//	hl job submit --agent jane job.yaml
//	hl job submit --agent jane job.yaml --hypothesis hyp-1 --env LR=0.1
//	hl job status job-1
//	hl job list --platform-experiment pe-123
//	hl quota
//	hl watch
//	hl watch --experiment exp-1 --until 'status in COMPLETED,FAILED,EVICTED'
//	hl watch --platform-experiment pe-123 --all --follow
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	case "quota":
		return cmdQuota(client, rest, stdout, stderr)
	case "results":
		return cmdResults(client, rest, stdout, stderr)
	case "capacity":
		return cmdCapacity(client, rest, stdout, stderr)
	case "donation":
		return cmdDonation(client, rest, stdout, stderr)
	case "hypothesis":
		return cmdHypothesis(client, rest, stdout, stderr)
	case "job":
		return cmdJob(client, rest, stdout, stderr)
	case "watch":
		return cmdWatch(apiURL, rest, stdout, stderr)
	case "experiments", "experiment":
		fmt.Fprintln(stderr, "hl: an experiment is a job here: hl job list, hl job status ID, hl job submit FILE")
		return 2
	default:
		fmt.Fprintf(stderr, "hl: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
}

const usage = `usage: hl <command> ...

  register              --id ID --kind agent|human
  signup                [--platform-experiment PE] [--agent A]
  platform-experiments  [list] | get [PE] | create [FILE] | start PE | update PE [FILE]
  quota                 your accelerator-hours, used and granted; --all for every agent
  results               standings on the declared metrics
  capacity              live accelerators per cluster, available and total
  donation              list | request | fulfill ID | cancel ID
  hypothesis            submit | link | comment | status | list | get | query
  job                   submit | list | status | metrics | summary | logs | data | cost | cancel | stats
  watch                 [--agent A | --all] [--platform-experiment PE] | --experiment JOB [--until ...] [--follow]

An ID is a positional argument (hl job status ID); --id ID works too. --agent and
--platform-experiment default to $AGENT_ID and $PLATFORM_EXPERIMENT_ID, except where --agent
filters a list. Run hl <command> --help for its flags.`

const (
	agentEnv              = "AGENT_ID"
	platformExperimentEnv = "PLATFORM_EXPERIMENT_ID"
)

// orEnv is a flag's value, or the named environment variable when the flag was omitted.
func orEnv(flagValue, name string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(name)
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

// newFlags is a flag set whose message on -h, on a wrong flag and on any other misuse starts with
// the command's usage text.
func newFlags(name, usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, usage)
		printFlags(fs, stderr)
	}
	return fs
}

// printFlags lists the flags except the aliases, which are registered with an empty usage.
func printFlags(fs *flag.FlagSet, w io.Writer) {
	header := false
	fs.VisitAll(func(f *flag.Flag) {
		if f.Usage == "" {
			return
		}
		if !header {
			fmt.Fprintln(w, "\nflags:")
			header = true
		}
		name, help := flag.UnquoteUsage(f)
		if name != "" {
			name = " " + name
		}
		fmt.Fprintf(w, "  --%s%s\n      %s\n", f.Name, name, help)
	})
}

var errUsage = errors.New("usage")

// misuse reports a wrong invocation followed by the command's usage.
func misuse(fs *flag.FlagSet, format string, a ...any) error {
	fmt.Fprintf(fs.Output(), "hl: %s: %s\n\n", fs.Name(), fmt.Sprintf(format, a...))
	fs.Usage()
	return errUsage
}

// parseArgs parses flags placed before, between or after the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// parseOperands returns the positional arguments followed by the values of the alias flags, which
// take the same arguments so that a first guess at the form works: hl job status ID and --id ID.
func parseOperands(fs *flag.FlagSet, argv []string, aliases ...string) ([]string, error) {
	var flagged stringList
	for _, name := range aliases {
		fs.Var(&flagged, name, "")
	}
	positional, err := parseArgs(fs, argv)
	if err != nil {
		return nil, err
	}
	return append(positional, flagged...), nil
}

// parseOne is parseOperands for a command taking exactly one operand; def stands in for a missing
// one when set.
func parseOne(fs *flag.FlagSet, argv []string, what, def string, aliases ...string) (string, error) {
	got, err := parseOperands(fs, argv, aliases...)
	if err != nil {
		return "", err
	}
	if len(got) == 0 && def != "" {
		return def, nil
	}
	if len(got) != 1 {
		return "", misuse(fs, "want one %s, got %d", what, len(got))
	}
	return got[0], nil
}

// parseExit is the exit code for a parse error: 0 for an explicit -h or --help, else 2.
func parseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// printLines prints a JSON response compactly: an array one element per line, anything else on
// one line, each through compactJSON. An empty array prints none to stderr, so that no output is
// never mistaken for a command that did nothing.
func printLines(stdout, stderr io.Writer, none string, raw json.RawMessage, drop ...string) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return fmt.Errorf("response is not JSON: %w", err)
		}
		if len(items) == 0 {
			fmt.Fprintf(stderr, "hl: %s\n", none)
		}
		for _, item := range items {
			if err := printLine(stdout, item, drop...); err != nil {
				return err
			}
		}
		return nil
	}
	return printLine(stdout, raw, drop...)
}

func printLine(stdout io.Writer, raw json.RawMessage, drop ...string) error {
	line, err := compactJSON(raw, drop...)
	if err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "%s\n", line)
	return err
}

// compactJSON re-encodes raw on one line in its own key order, leaving out $schema, every null,
// "" and {} value, and the named keys of the top-level object.
func compactJSON(raw []byte, drop ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b bytes.Buffer
	if err := writeValue(dec, &b, drop); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeValue(dec *json.Decoder, b *bytes.Buffer, drop []string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			b.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := writeValue(dec, b, nil); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		} else {
			b.WriteByte('{')
			n := 0
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				var v bytes.Buffer
				if err := writeValue(dec, &v, nil); err != nil {
					return err
				}
				if s := v.String(); key == "$schema" || slices.Contains(drop, key) || s == "null" || s == `""` || s == "{}" {
					continue
				}
				if n > 0 {
					b.WriteByte(',')
				}
				writeString(b, key)
				b.WriteByte(':')
				b.Write(v.Bytes())
				n++
			}
			b.WriteByte('}')
		}
		_, err := dec.Token()
		return err
	case string:
		writeString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	}
	return nil
}

func writeString(b *bytes.Buffer, s string) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Truncate(b.Len() - 1)
}

// --- register ----------------------------------------------------------------------------------

func cmdRegister(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("register", "usage: hl register --id ID [--name NAME] [--kind agent|human]", stderr)
	id := fs.String("id", "", "stable unique agent id (required)")
	name := fs.String("name", "", "human-readable display name; defaults to id")
	kind := fs.String("kind", "human", "agent or human")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *id == "" {
		return parseExit(misuse(fs, "--id is required"))
	}
	body := map[string]string{"id": *id, "name": *name, "kind": *kind}
	if body["name"] == "" {
		body["name"] = *id
	}
	var out json.RawMessage
	if err := c.Do("POST", "/agents", body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// --- signup --------------------------------------------------------------------------------------

func cmdSignup(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("signup", "usage: hl signup [--platform-experiment PE] [--agent A] [--quota-tier guaranteed|burst_only]\n\nSigning up again once signed up succeeds.", stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default $PLATFORM_EXPERIMENT_ID)")
	agent := fs.String("agent", "", "agent id (default $AGENT_ID)")
	quotaTier := fs.String("quota-tier", "", "override this signup's guaranteed-vs-burst-only split "+
		"(\"guaranteed\" or \"burst_only\"); omit to use the platform experiment's default_quota_tier")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	*pe = orEnv(*pe, platformExperimentEnv)
	*agent = orEnv(*agent, agentEnv)
	if *pe == "" || *agent == "" {
		return parseExit(misuse(fs, "--platform-experiment and --agent are required"))
	}
	if *quotaTier != "" && *quotaTier != "guaranteed" && *quotaTier != "burst_only" {
		fmt.Fprintln(stderr, "hl: signup: --quota-tier must be \"guaranteed\" or \"burst_only\", got "+*quotaTier)
		return 2
	}
	body := map[string]string{"agent_id": *agent}
	if *quotaTier != "" {
		body["quota_tier"] = *quotaTier
	}
	var out json.RawMessage
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(*pe)+"/signup", body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
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
		case "results":
			return cmdResults(c, args[1:], stdout, stderr)
		}
	}
	fs := newFlags("platform-experiments", "usage: hl platform-experiments [list] [--limit N] | get [PE] | create [FILE] | start PE | update PE [FILE]", stderr)
	limit := fs.Int("limit", 20, "max results")
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unknown subcommand %q", fs.Arg(0)))
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
	fs := newFlags("platform-experiments create", `usage: hl platform-experiments create [FILE|-]

FILE is a YAML file shaped exactly like the POST /platform-experiments body. Minimal shape:

  name: my-platform-experiment
  description: what this research program is testing
  budget_accelerator_hours: 10
  max_agents: 5
  report_interval_seconds: 10
  metrics:
    - key: val_accuracy
      direction: maximize

To create several platform experiments in one call, use an experiments: list. Fields set
alongside the list (e.g. a shared budget_accelerator_hours) default onto every item that
doesn't set its own:

  budget_accelerator_hours: 10
  experiments:
    - name: my-first-platform-experiment
      description: ...
    - name: my-second-platform-experiment
      description: ...

A bare top-level YAML list of mappings works the same way, without shared defaults.

See controlplane/settings/examples/experiment.yaml for the full DSL reference.`, stderr)
	path, err := parseOne(fs, args, "FILE", "-")
	if err != nil {
		return parseExit(err)
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

// cmdPlatformExperimentsUpdate handles `hl platform-experiments update PE [FILE|-]`. FILE is a
// YAML file shaped exactly like the create body (see controlplane/settings/examples/
// experiment.yaml) — while the platform experiment is still open, every field including metrics
// is editable; once running, only name and description are.
func cmdPlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments update", `usage: hl platform-experiments update PE [FILE|-]

FILE is shaped exactly like 'hl platform-experiments create's body (see
controlplane/settings/examples/experiment.yaml). While the platform experiment is still open,
every field including metrics is editable; once running, only name and description may change
and any other field that differs from the live value is rejected, so update from the file the
experiment was created with (or set the named fields to the live values).`, stderr)
	id := fs.String("id", "", "")
	operands, err := parseArgs(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if *id == "" && len(operands) > 0 {
		*id, operands = operands[0], operands[1:]
	}
	if *id == "" || len(operands) > 1 {
		return parseExit(misuse(fs, "want PE and at most one FILE"))
	}
	path := "-"
	if len(operands) == 1 {
		path = operands[0]
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
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(*id), body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments get", `usage: hl platform-experiments get [PE]

The platform experiment (default $PLATFORM_EXPERIMENT_ID): its status, description, metrics,
stages and current_stage.`, stderr)
	id, err := parseOne(fs, args, "PE", os.Getenv(platformExperimentEnv), "id")
	if err != nil {
		return parseExit(err)
	}
	var out map[string]any
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &out); err != nil {
		return fail(stderr, err)
	}
	delete(out, "$schema")
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments start", "usage: hl platform-experiments start PE", stderr)
	id, err := parseOne(fs, args, "PE", "", "id")
	if err != nil {
		return parseExit(err)
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/start", map[string]any{}, &out); err != nil {
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
	fs := newFlags("watch", `usage: hl watch [--agent A | --all] [--platform-experiment PE] [--kinds K,...] [--follow]
   or: hl watch --experiment JOB [--until 'status in COMPLETED,FAILED,EVICTED']

Prints events, one JSON object per line. With $AGENT_ID set and no --experiment or --all, the
agent's own resources, resuming from where its last watch stopped.`, stderr)
	experiment := fs.String("experiment", os.Getenv("HYPOTHESISLOOP_EXPERIMENT_ID"), "watch every event of one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "platform experiment to watch (default $PLATFORM_EXPERIMENT_ID unless --experiment is set)")
	agent := fs.String("agent", "", "watch this agent's own resources: its jobs' outcomes and queue reasons, its quota and cut, stage advances, the run's status and brief, and others' comments, findings and links on its hypotheses (default $AGENT_ID unless --experiment or --all is set)")
	all := fs.Bool("all", false, "watch the whole platform experiment instead of one agent's resources")
	kinds := fs.String("kinds", "", "comma-separated event kinds instead of the default set (everything but metric.point); see GET /watch/kinds")
	showSubscription := fs.Bool("show-subscription", false, "ask the server what this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'")
	follow := fs.Bool("follow", false, "without --until, stream until the timeout instead of returning with the first batch of events")
	timeout := fs.Float64("timeout", 540.0, "give up after this many seconds")
	since := fs.Int64("since", 0, "resume from this cursor instead of the saved one: replays from it, the events at it included, before going live")
	urlFlag := fs.String("url", "", "platform API base URL (defaults to $HYPOTHESISLOOP_API_URL, then $API_URL)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	if *experiment == "" {
		*platformExperiment = orEnv(*platformExperiment, platformExperimentEnv)
		if !*all {
			*agent = orEnv(*agent, agentEnv)
		}
	}

	// An agent watching its own resources with the default kinds resumes where its last watch
	// stopped. Any other subscription would skip events of kinds it did not carry.
	var cursorFile string
	if *agent != "" && *platformExperiment != "" && *experiment == "" && *kinds == "" {
		dir, err := stateDir()
		if err != nil {
			fmt.Fprintf(stderr, "hl watch: %s\n", err)
			return 2
		}
		cursorFile = filepath.Join(dir, "watch-"+url.PathEscape(*platformExperiment)+"-"+url.PathEscape(*agent)+".json")
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
		Follow:             *follow,
		Timeout:            time.Duration(*timeout * float64(time.Second)),
		Since:              *since,
		CursorFile:         cursorFile,
		Stdout:             stdout,
		Stderr:             stderr,
	})
}

// stateDir is where hl keeps state between runs: $HL_STATE_DIR, else $XDG_STATE_HOME/hl, else
// ~/.local/state/hl.
func stateDir() (string, error) {
	if dir := os.Getenv("HL_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "hl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no state directory: set HL_STATE_DIR (%w)", err)
	}
	return filepath.Join(home, ".local", "state", "hl"), nil
}
