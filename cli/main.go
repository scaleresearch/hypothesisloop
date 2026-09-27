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
// exactly like the POST /experiments body, because hand-encoding that shape as a second set
// of flags is the kind of silently-drifting copy of a contract this codebase explicitly avoids
// elsewhere. Its --hypothesis and --env flags override, for one submission, the fields that change
// per job. YAML rather than JSON because a human is meant to write and edit this file by hand.
//
// --agent and --platform-experiment default to $AGENT_ID and $PLATFORM_EXPERIMENT_ID, which the
// experimentator harness sets in an agent's container.
//
//	hl platform-experiments agents add --platform-experiment pe-123 --hosted --flavor generalist
//	hl platform-experiments agents add --platform-experiment pe-123 --agent jane --quota-tier guaranteed
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
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

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

	if args[0] == "exec" {
		return cmdExec(args[1:], stdout, stderr)
	}

	apiURL := os.Getenv("API_URL")
	token := os.Getenv("HL_TOKEN")
	client := apiclient.New(apiURL, token)

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "platform-experiments":
		return cmdPlatformExperiments(client, rest, stdout, stderr)
	case "quota":
		return cmdQuota(client, rest, stdout, stderr)
	case "results":
		return cmdResults(client, rest, stdout, stderr)
	case "capacity":
		return cmdCapacity(client, rest, stdout, stderr)
	case "data-key":
		return cmdDataKey(client, rest, stdout, stderr)
	case "git-credential":
		return cmdGitCredential(client, rest, os.Stdin, stdout, stderr)
	case "hypothesis":
		return cmdHypothesis(client, rest, stdout, stderr)
	case "job":
		return runGroup("job", jobCommands, client, rest, stdout, stderr)
	case "repo":
		return runGroup("repo", repoCommands, client, rest, stdout, stderr)
	case "org":
		return runGroup("org", orgCommands, client, rest, stdout, stderr)
	case "project":
		return runGroup("project", projectCommands, client, rest, stdout, stderr)
	case "api-key":
		return runGroup("api-key", apiKeyCommands, client, rest, stdout, stderr)
	case "cluster":
		return runGroup("cluster", clusterCommands, client, rest, stdout, stderr)
	case "watch":
		return cmdWatch(apiURL, token, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hl: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
}

const usage = `usage: hl <command> ...

  platform-experiments  [list] | get | create [FILE] | start | update --description-file F | propose [FILE] | decline --reason TEXT | close | summary | agents add|remove|list|finish|session
  quota                 your share of the budget and what a job submitted now may still hold of it; --all for every agent
  results               standings on the declared metrics
  capacity              live accelerators per cluster, available and total
  data-key              your S3 key to the experiment's data: eval "$(hl data-key)"
  git-credential        git's credential helper for your experiment's repository; git runs it
  hypothesis            submit | link | comment | status | list | get | query
  job                   submit | list | status | metrics | summary | logs | data | cost | cancel | stats
  repo                  init | check: the files every experiment's repository holds
  org                   list | create NAME | rename NAME | members list|set-role|remove | invites create|list|revoke|accept
  project               list | create NAME | rename NAME
  api-key               create NAME | list | revoke ID
  cluster               add NAME | list | token | order | settings | disable | enable | forget-identity | remove
  watch                 [--agent A | --all] [--platform-experiment PE] | --experiment JOB [--until ...] [--follow]
  exec                  --stop-grace SECONDS -- COMMAND: a runtime's wrapper around a job's command

An ID is positional; --id ID works too. The platform experiment is never an operand. A command
given the ID of a job or a hypothesis takes no --platform-experiment, since an ID is unique on
the platform; every other command that works in a platform experiment takes it. --agent and
--platform-experiment default to what the token names, an agent's its agent and platform
experiment and a coordinator's its platform experiment, so neither writes them, and for
another token to $AGENT_ID and $PLATFORM_EXPERIMENT_ID. Where --agent filters a list it
has no default: write the id out. --org defaults to $HL_ORG and
then to the one org the token reaches, --project to $HL_PROJECT. hl job --help, hl hypothesis
--help and the --help of org, project, api-key and cluster list their commands with every flag.`

// notStarted is what an agent can do about an open platform experiment: nothing but wait. It
// says how long when startsAt is the experiment's start time.
func notStarted(startsAt string) string {
	in := ""
	if at, err := time.Parse(time.RFC3339, startsAt); err == nil {
		in = fmt.Sprintf(" starts in %d s;", max(0, int(time.Until(at).Seconds())))
	}
	return "not started:" + in + " every job submit is refused until it runs; prepare, then end your turn and the start arrives as an update"
}

// subcommand is one command of a group. The group's listing takes its synopsis from the command's
// own --help, so the two cannot drift.
type subcommand struct {
	name, about string
	run         func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int
}

// groupUsage lists each subcommand as the first usage form of its --help and what it is for.
func groupUsage(group string, subs []subcommand) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: hl %s <command> ...\n", group)
	for _, sub := range subs {
		var help bytes.Buffer
		// A subcommand may read its defaults from the client's token before it parses its flags.
		sub.run(&apiclient.Client{}, []string{"-h"}, &help, &help)
		usage, _, _ := strings.Cut(help.String(), "\n\n")
		usage, _, _ = strings.Cut(usage, "\n   or:")
		synopsis := strings.Join(strings.Fields(strings.TrimPrefix(usage, "usage: hl "+group+" ")), " ")
		fmt.Fprintf(&b, "\n  %s\n    %s", synopsis, sub.about)
	}
	return b.String()
}

func runGroup(group string, subs []subcommand, c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, groupUsage(group, subs))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, groupUsage(group, subs))
		return 0
	}
	for _, sub := range subs {
		if sub.name == args[0] {
			return sub.run(c, args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "hl: %s: unknown subcommand %q\n\n%s\n", group, args[0], groupUsage(group, subs))
	return 2
}

const (
	agentEnv              = "AGENT_ID"
	platformExperimentEnv = "PLATFORM_EXPERIMENT_ID"
	orgEnv                = "HL_ORG"
	projectEnv            = "HL_PROJECT"
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

// fail prints err on one line, a JSON reply of the platform compacted like any other output.
func fail(stderr io.Writer, err error) int {
	var refused *apiclient.HTTPError
	if errors.As(err, &refused) {
		if invalid := invalidFields([]byte(refused.Body)); invalid != "" {
			refused.Body = invalid
		} else if body, jsonErr := compactJSON([]byte(refused.Body)); jsonErr == nil {
			refused.Body = string(body)
		}
	}
	fmt.Fprintf(stderr, "hl: %s\n", err)
	return 1
}

// invalidFields is what a request refused for its shape got wrong, one clause per field: where
// and what. The API's answer also carries the whole value it read at each place, which for a
// missing field is the request itself and buries the one line that matters. Empty for an answer
// that lists no fields.
func invalidFields(body []byte) string {
	var problem struct {
		Errors []struct {
			Message  string `json:"message"`
			Location string `json:"location"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &problem) != nil || len(problem.Errors) == 0 {
		return ""
	}
	clauses := make([]string, 0, len(problem.Errors))
	for _, e := range problem.Errors {
		clause := e.Message
		if e.Location != "" {
			clause = strings.TrimPrefix(e.Location, "body.") + ": " + e.Message
		}
		clauses = append(clauses, clause)
	}
	return strings.Join(clauses, "; ")
}

// flagSet answers -h with the command's help and any misuse with one error line and the synopsis,
// the first paragraph of the help.
type flagSet struct {
	*flag.FlagSet
	help   string
	stderr io.Writer
}

func newFlags(name, help string, stderr io.Writer) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{fs, help, stderr}
}

func (fs *flagSet) Parse(args []string) error {
	err := fs.FlagSet.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(fs.stderr, fs.help)
		fs.printFlags()
		return err
	}
	if err != nil && strings.HasSuffix(err.Error(), "not defined: -platform-experiment") {
		fmt.Fprintf(fs.stderr, "hl: %s: takes no --platform-experiment, leave it out: an ID is found on the whole platform, and a command given none is not about one platform experiment\n", fs.Name())
		return errUsage
	}
	if err != nil {
		return misuse(fs, "%s", err)
	}
	return nil
}

// printFlags lists the flags except the aliases, which are registered with an empty usage.
func (fs *flagSet) printFlags() {
	header := false
	fs.VisitAll(func(f *flag.Flag) {
		if f.Usage == "" {
			return
		}
		if !header {
			fmt.Fprintln(fs.stderr, "\nflags:")
			header = true
		}
		name, help := flag.UnquoteUsage(f)
		if name != "" {
			name = " " + name
		}
		fmt.Fprintf(fs.stderr, "  --%s%s\n      %s\n", f.Name, name, help)
	})
}

var errUsage = errors.New("usage")

// misuse reports a wrong invocation followed by the command's synopsis.
func misuse(fs *flagSet, format string, a ...any) error {
	synopsis, _, _ := strings.Cut(fs.help, "\n\n")
	fmt.Fprintf(fs.stderr, "hl: %s: %s\n%s\n", fs.Name(), fmt.Sprintf(format, a...), synopsis)
	return errUsage
}

// parseArgs parses flags placed before, between or after the positional arguments.
func parseArgs(fs *flagSet, args []string) ([]string, error) {
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
func parseOperands(fs *flagSet, argv []string, aliases ...string) ([]string, error) {
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
func parseOne(fs *flagSet, argv []string, what, def string, aliases ...string) (string, error) {
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

// experimentFlag registers --platform-experiment, the one way a command names its platform
// experiment. It is never an operand and has no shorter spelling.
func experimentFlag(fs *flagSet) *string {
	return fs.String("platform-experiment", "", "the platform experiment (default the token's, else $"+platformExperimentEnv+")")
}

// namedExperiment is the platform experiment a command works in (see experimentOf), or a misuse
// when nothing names one.
func namedExperiment(fs *flagSet, c *apiclient.Client, flagValue string) (string, error) {
	if id := experimentOf(c, flagValue); id != "" {
		return id, nil
	}
	return "", misuse(fs, "want --platform-experiment: HL_TOKEN names none and $"+platformExperimentEnv+" is not set")
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

func cmdDataKey(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("data-key", `usage: hl data-key [--platform-experiment PE] [--agent A]

Your own S3 key to the platform experiment's data, as export lines: eval "$(hl data-key)".
It reads everything under $HYPOTHESISLOOP_DATA_SHARED and writes under $HYPOTHESISLOOP_DATA_URI,
where your jobs write. The same key every time; it does not expire. Any S3 client works:
  uvx --from awscli aws s3 ls "$HYPOTHESISLOOP_DATA_SHARED"`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default $PLATFORM_EXPERIMENT_ID)")
	agent := fs.String("agent", "", "agent id (default $AGENT_ID)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	*pe = experimentOf(c, *pe)
	var err error
	if *agent, err = writingAgent(c, *agent); err != nil {
		return fail(stderr, err)
	}
	if *pe == "" || *agent == "" || fs.NArg() > 0 {
		return parseExit(misuse(fs, "--platform-experiment and --agent are required, and nothing else"))
	}
	var out struct {
		URI             string `json:"uri"`
		Shared          string `json:"shared"`
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(*pe)+"/data-key", map[string]string{"agent_id": *agent}, &out); err != nil {
		return fail(stderr, err)
	}
	for _, v := range [][2]string{
		{"AWS_ACCESS_KEY_ID", out.AccessKeyID},
		{"AWS_SECRET_ACCESS_KEY", out.SecretAccessKey},
		{"AWS_ENDPOINT_URL", out.Endpoint},
		{"AWS_ENDPOINT_URL_S3", out.Endpoint},
		{"AWS_REGION", out.Region},
		{"HYPOTHESISLOOP_DATA_URI", out.URI},
		{"HYPOTHESISLOOP_DATA_SHARED", out.Shared},
	} {
		fmt.Fprintf(stdout, "export %s='%s'\n", v[0], strings.ReplaceAll(v[1], "'", `'\''`))
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
		case "close":
			return cmdPlatformExperimentsClose(c, args[1:], stdout, stderr)
		case "summary":
			return cmdPlatformExperimentsSummary(c, args[1:], stdout, stderr)
		case "update":
			return cmdPlatformExperimentsUpdate(c, args[1:], stdout, stderr)
		case "propose":
			return cmdPlatformExperimentsPropose(c, args[1:], stdout, stderr)
		case "decline":
			return cmdPlatformExperimentsDecline(c, args[1:], stdout, stderr)
		case "get":
			return cmdPlatformExperimentsGet(c, args[1:], stdout, stderr)
		case "agents":
			return runGroup("platform-experiments agents", agentsCommands, c, args[1:], stdout, stderr)
		}
	}
	fs := newFlags("platform-experiments", "usage: hl platform-experiments [list] [--limit N] [--project PROJECT] [--shared[=false]] | get | create [FILE] | start | update --description-file F | propose [FILE] | decline --reason TEXT | close | summary | agents add|remove|list|finish|session", stderr)
	limit := fs.Int("limit", 20, "max results")
	project := fs.String("project", "", "only this project's platform experiments (default $HL_PROJECT)")
	shared := fs.Bool("shared", false, "only the platform experiments the platform shows every account, or with --shared=false none of them. Without it both are listed, the shared ones marked \"shared\": true. Read those like your own, write nothing to them")
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unknown subcommand %q", fs.Arg(0)))
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if p := orEnv(*project, projectEnv); p != "" {
		q.Set("project_id", p)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "shared" {
			q.Set("shared", strconv.FormatBool(*shared))
		}
	})
	var out any
	if err := c.Do("GET", "/platform-experiments?"+q.Encode(), nil, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// cmdPlatformExperimentsCreate handles `hl platform-experiments create [FILE|-]`. FILE is a YAML
// file shaped like the POST /platform-experiments body — see
// controlplane/settings/examples/experiment.yaml for the full DSL reference.
func cmdPlatformExperimentsCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments create", `usage: hl platform-experiments create [--project PROJECT] [FILE|-]

FILE is a YAML file shaped exactly like the POST /platform-experiments body. Every platform
experiment belongs to a project: project_id in the file, else --project, else $HL_PROJECT.
Minimal shape:

  name: my-platform-experiment
  description: what this research program is testing
  budget_accelerator_hours: 10
  max_agents: 4
  duration_hours: 24
  metrics:
    - key: val_accuracy
      direction: maximize

Every field shown is required. How long it lasts is duration_hours or ends_at, one of the two.
report_interval_seconds and starts_at may be left out: the platform's default applies, and
without starts_at it starts when you start it.

The experiment keeps its code in a repository on the deployment's git host. On GitHub,
repository: owner/name names one a GitHub account connected to the org covers. Left out, the
platform creates a private one there and the answer names it; repository_account says on
which account when the org connected several. With no account connected the answer is 422
github_not_connected: an owner connects GitHub in Settings first. On any other git host,
repository names one created there beforehand, and leaving it out is 422 repository_required.

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
	project := fs.String("project", "", "project the platform experiments belong to (default $HL_PROJECT)")
	path, err := parseOne(fs, args, "FILE", "-")
	if err != nil {
		return parseExit(err)
	}
	*project = orEnv(*project, projectEnv)
	items, err := readYAMLItems(path, "experiments")
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	results := make([]any, 0, len(items))
	for _, body := range items {
		if _, ok := body["project_id"]; !ok {
			if *project == "" {
				return parseExit(misuse(fs, "a project is required: project_id in the file, --project, or $HL_PROJECT"))
			}
			body["project_id"] = *project
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

// cmdPlatformExperimentsUpdate handles `hl platform-experiments update [FIELDS.yaml]`. FILE is a
// YAML file shaped exactly like the create body (see controlplane/settings/examples/
// experiment.yaml) — while the platform experiment is still open, every field including metrics
// is editable; once running, only name and description are.
func cmdPlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments update", `usage: hl platform-experiments update [--platform-experiment PE] [--description-file FILE] [--description TEXT] [--name NAME] [FIELDS.yaml]

Changes the fields you name and leaves every other as it is. The brief is one command:

  hl platform-experiments update --description-file brief.md

FIELDS.yaml, for the rest, is a YAML mapping of the fields that change, such as
budget_accelerator_hours, max_agents, ends_at, metrics, report_interval_seconds. While the
platform experiment is open any of them can change; once it runs only name and description do.
The experiment as changed is checked like a create, so neither name nor description is emptied.
A coordinator's token changes the description only.`, stderr)
	pe := experimentFlag(fs)
	name := fs.String("name", "", "the new name")
	description := fs.String("description", "", "the new description")
	descriptionFile := fs.String("description-file", "", "read the new description from this file, - for stdin")
	operands, err := parseArgs(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one FIELDS.yaml, got %d operands: the platform experiment is --platform-experiment", len(operands)))
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	if *description != "" && *descriptionFile != "" {
		return parseExit(misuse(fs, "--description and --description-file are two ways to say one thing"))
	}
	body := map[string]any{}
	if len(operands) == 1 {
		if body, err = readYAMLBody(operands[0]); err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
	}
	if *name != "" {
		body["name"] = *name
	}
	if *description != "" {
		body["description"] = *description
	}
	if *descriptionFile != "" {
		var raw []byte
		if *descriptionFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(*descriptionFile)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		body["description"] = string(raw)
	}
	if len(body) == 0 {
		return parseExit(misuse(fs, "nothing to change: name a field"))
	}
	var out any
	if err := c.Do("PATCH", "/platform-experiments/"+url.PathEscape(id), body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsPropose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments propose", `usage: hl platform-experiments propose [--platform-experiment PE] [FILE|-]

For the planner of a draft. FILE is YAML of this shape, with every field shown required unless
it says optional:

  experiment:
    name: short title
    description: |
      the brief, the only thing researchers read. It holds a BASELINE block that names
      baseline_job_id and that job's code_ref, or the proposal is refused.
    report_interval_seconds: 30        # how often the harness really reports
    metrics:
      - key: val_accuracy              # one metric ranks: role ranking is the default
        direction: maximize            # or minimize
      - key: exact_match               # optional gate
        direction: maximize
        role: constraint
        bound: 1                       # a value the baseline holds
  budget_accelerator_hours: 6          # proposed; the person states the real ones at accept
  duration_hours: 4
  agents:                              # one entry per researcher
    - flavor: hyperparameter-search    # optional: generalist (the default), architecture-search,
      hyperparameters:                 #   data-search or hyperparameter-search. The direction you
        direction: what this one tries #   give a researcher goes in hyperparameters, any keys.
    - {}
  baseline_job_id: job-...             # your COMPLETED job with its summary filed
  metric_reason: why this metric is the goal, in plain words
  measurement: how it is measured, in plain words
  sizing_reason: why this budget, duration and number of researchers are the least that reach
    the goal, with the arithmetic from the baseline's cost and time
  out_of_scope: [optional sentences]
  risks: [optional sentences]
  questions: [optional sentences]

The proposal is checked before it is stored, and a refusal names what to fix. One that passes is
stored at once, replaces the one before, releases your machine and is what the person is asked
to accept: send only the finished proposal, never one written to try the checks.`, stderr)
	pe := experimentFlag(fs)
	operands, err := parseArgs(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one FILE, got %d operands", len(operands)))
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
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
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/proposal", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsDecline(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments decline", `usage: hl platform-experiments decline [--platform-experiment PE] --reason TEXT

For the planner of a draft, when the goal cannot be made into an experiment: nothing measurable,
or nothing this platform can run. The reason is shown to the person who wrote the goal in place
of a proposal, so say in plain words why, and what would make it plannable. A proposal you made
before no longer stands. The person may answer with a revision, which starts you again.`, stderr)
	pe := experimentFlag(fs)
	reason := fs.String("reason", "", "why no experiment is proposed")
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	if strings.TrimSpace(*reason) == "" {
		return parseExit(misuse(fs, "want --reason"))
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/decline", map[string]string{"reason": *reason}, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments get", `usage: hl platform-experiments get [--platform-experiment PE]

The platform experiment: its status, description, metrics, stages and current_stage. A draft
has no ends_at: it carries planning_ends_at while it is planned and proposal_expires_at once
its planner answered, when the platform closes it if nothing happened by then.`, stderr)
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var out map[string]any
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id), nil, &out); err != nil {
		return fail(stderr, err)
	}
	delete(out, "$schema")
	// Ahead of the record, whose description is long: a reader of its first lines must not miss it.
	if out["status"] == "open" {
		startsAt, _ := out["starts_at"].(string)
		fmt.Fprintln(stderr, "hl: platform-experiments get: "+notStarted(startsAt))
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsStart(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments start", "usage: hl platform-experiments start [--platform-experiment PE]", stderr)
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
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

func cmdPlatformExperimentsClose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments close", `usage: hl platform-experiments close [--platform-experiment PE]

Ends the platform experiment for good: every job still running
is evicted, nothing more is accepted, and the standings are taken from the metrics reported. Its
summary can still be written afterwards.`, stderr)
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/close", map[string]any{}, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsSummary(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments summary", `usage: hl platform-experiments summary [--platform-experiment PE] --text T | --file FILE|-

Writes the summary of the platform experiment: what the run
found and what the next one should do differently. Each write replaces the one before, so write
the whole text; hl platform-experiments get shows it as summary. It can be written while the
experiment runs and after it closed.`, stderr)
	text := fs.String("text", "", "the summary")
	file := fs.String("file", "", "read the summary from this file, - for stdin")
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	if (*text == "") == (*file == "") {
		return parseExit(misuse(fs, "one of --text and --file is required"))
	}
	if *file != "" {
		var raw []byte
		if *file == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(*file)
		}
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		*text = string(raw)
	}
	if strings.TrimSpace(*text) == "" {
		return parseExit(misuse(fs, "the summary is empty"))
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": *text}, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

// yamlProblem is why raw is not the YAML a command reads, with where the parser stopped when
// it says: the line, the column and what that line reads.
func yamlProblem(raw []byte, err error) string {
	var problem *yaml.LoadError
	if !errors.As(err, &problem) || problem.Mark.Line == 0 {
		return err.Error()
	}
	said := problem.Mark.String() + ": " + problem.Message
	lines := strings.Split(string(raw), "\n")
	if problem.Mark.Line > len(lines) || strings.TrimSpace(lines[problem.Mark.Line-1]) == "" {
		return said
	}
	return said + ", where the line reads: " + strings.TrimSpace(lines[problem.Mark.Line-1])
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
		return nil, fmt.Errorf("%s is not valid YAML: %s", path, yamlProblem(raw, err))
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
		return nil, fmt.Errorf("%s is not valid YAML: %s", path, yamlProblem(raw, err))
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

func cmdWatch(apiURL, token string, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("watch", `usage: hl watch [--agent A | --all [--decisions]] [--platform-experiment PE] [--kinds K,...] [--follow | --no-wait]
   or: hl watch --experiment JOB [--until 'status in COMPLETED,FAILED,EVICTED']

Prints events, one JSON object per line. With an agent's token or $AGENT_ID and no --experiment
or --all, the agent's own resources, resuming from where its last watch stopped. --all resumes the same way.`, stderr)
	experiment := fs.String("experiment", os.Getenv("HYPOTHESISLOOP_EXPERIMENT_ID"), "watch every event of one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "platform experiment to watch (default $PLATFORM_EXPERIMENT_ID unless --experiment is set)")
	agent := fs.String("agent", "", "watch this agent's own resources: its jobs' outcomes and the queue reasons waiting will not clear, its quota and cut, stage advances, the run's status and brief, and others' comments, findings and links on its hypotheses (default $AGENT_ID unless --experiment or --all is set)")
	all := fs.Bool("all", false, "watch the whole platform experiment instead of one agent's resources")
	decisions := fs.Bool("decisions", false, "with --all, leave out what needs no decision: a job being queued, admitted or started, and a queue reason that waiting clears")
	kinds := fs.String("kinds", "", "comma-separated event kinds instead of the default set (everything but metric.point); see GET /watch/kinds")
	showSubscription := fs.Bool("show-subscription", false, "ask the server what this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'. A job ends COMPLETED, FAILED, EVICTED, REJECTED or CANCELLED")
	noWait := fs.Bool("no-wait", false, "print the events already waiting and return at once, without listening for more")
	follow := fs.Bool("follow", false, "without --until, stream until the timeout instead of returning with the first batch of events")
	timeout := fs.Float64("timeout", 540.0, "give up after this many seconds")
	since := fs.Int64("since", 0, "resume from this cursor instead of the saved one: replays from it, the events at it included, before going live")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	if *experiment == "" {
		c := apiclient.New(apiURL, token)
		*platformExperiment = experimentOf(c, *platformExperiment)
		if readsEveryAgent(token, *agent) {
			*all = true
		}
		if !*all {
			*agent = agentOf(c, *agent)
		}
	}

	// An agent watching its own resources with the default kinds resumes where its last watch
	// stopped, and so does a watch of the whole platform experiment. Any other subscription
	// would skip events of kinds it did not carry.
	var cursorFile string
	if (*agent != "" || *all) && *platformExperiment != "" && *experiment == "" && *kinds == "" {
		dir, err := stateDir()
		if err != nil {
			fmt.Fprintf(stderr, "hl watch: %s\n", err)
			return 2
		}
		// No agent id is empty, so a whole experiment's cursor is no agent's file.
		cursorFile = filepath.Join(dir, "watch-"+url.PathEscape(*platformExperiment)+"-"+url.PathEscape(*agent)+".json")
	}

	return watchclient.Run(watchclient.Options{
		URL:                apiURL,
		Token:              token,
		Experiment:         *experiment,
		PlatformExperiment: *platformExperiment,
		Agent:              *agent,
		Kinds:              *kinds,
		Decisions:          *decisions && *all,
		ShowSubscription:   *showSubscription,
		Until:              *until,
		Follow:             *follow,
		NoWait:             *noWait,
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
