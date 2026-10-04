// A job of hl is a domain.Experiment: the API, the event kinds and experiment_id say experiment
// where hl's commands and flags say job, for the same row.
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
	token := os.Getenv("HL_TOKEN")
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage(token))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, usage(token))
		return 0
	}

	client := apiclient.New(os.Getenv("API_URL"), token)
	for _, command := range commands {
		if command.name != args[0] {
			continue
		}
		if kind := kindOf(token); !command.who.admits(kind) {
			return fail(stderr, fmt.Errorf("%s: %w", command.name, command.who.refusal(kind)))
		}
		return command.run(client, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "hl: unknown command %q\n\n%s\n", args[0], usage(token))
	return 2
}

// subcommand is one command of hl or of a group. who says whose token runs it: the listing a
// caller reads holds the commands that admit it, and runGroup refuses the others, so the help
// cannot name a command its reader is refused. A group's listing takes each synopsis from the
// command's own --help, so those two cannot drift either.
type subcommand struct {
	name, about string
	run         func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int
	who         callers
}

func group(name string, subs []subcommand) func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return runGroup(name, subs, c, args, stdout, stderr)
	}
}

// The commands of hl. One with subs is a group, listed as the commands of it its reader runs,
// then its about.
var commands = []struct {
	subcommand
	subs []subcommand
}{
	{subcommand{"platform-experiments", "", cmdPlatformExperiments, everyone}, platformExperimentCommands},
	{subcommand{"quota", "your share of the budget and what a job submitted now may still hold of it; --all for every agent", cmdQuota, everyone}, nil},
	{subcommand{"results", "standings on the declared metrics", cmdResults, everyone}, nil},
	{subcommand{"capacity", "live accelerators per cluster, available and total", cmdCapacity, everyone}, nil},
	{subcommand{"data-key", `your S3 key to the experiment's data: eval "$(hl data-key)"`, cmdDataKey, asAgent}, nil},
	{subcommand{"git-credential", "git's credential helper for your experiment's repository; git runs it", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return cmdGitCredential(c, args, os.Stdin, stdout, stderr)
	}, everyone}, nil},
	{subcommand{"hypothesis", "", cmdHypothesis, everyone}, hypothesisCommands},
	{subcommand{"job", "", group("job", jobCommands), everyone}, jobCommands},
	{subcommand{"env", "", group("env", envCommands), everyone}, envCommands},
	{subcommand{"repo", "the files every experiment's repository holds", group("repo", repoCommands), everyone}, repoCommands},
	{subcommand{"org", "", group("org", orgCommands), ofPeople}, orgCommands},
	{subcommand{"project", "", group("project", projectCommands), ofPeople}, projectCommands},
	{subcommand{"api-key", "", group("api-key", apiKeyCommands), ofPeople}, apiKeyCommands},
	{subcommand{"cluster", "", group("cluster", clusterCommands), ofPeople}, clusterCommands},
	{subcommand{"watch", "your own updates, one JSON object per line", cmdWatch, everyone}, nil},
	// A job's token is none of an agent's or a coordinator's, so a runtime's is admitted.
	{subcommand{"exec", "--stop-grace SECONDS -- COMMAND: a runtime's wrapper around a job's command", func(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return cmdExec(args, stdout, stderr)
	}, ofPeople}, nil},
}

const usageRules = `
An ID is positional (--id ID works too). A command given a job's or a hypothesis's ID takes no
--platform-experiment; every other one in a platform experiment takes it as a flag, never an
operand. --agent and --platform-experiment default to what an agent's or a coordinator's token
names; a person or an API key writes both out. Where --agent filters a list it has no default.`

const usageOfPeople = ` --org defaults to $HL_ORG and
then to the one org the token reaches, --project to $HL_PROJECT. hl job --help, hl hypothesis
--help and the --help of org, project, api-key and cluster list their commands with every flag.`

// usage lists the commands the caller of token can run.
func usage(token string) string {
	kind := kindOf(token)
	var b strings.Builder
	b.WriteString("usage: hl <command> ...\n\n")
	for _, command := range commands {
		if !command.who.admits(kind) {
			continue
		}
		about := command.about
		if command.subs != nil {
			var names []string
			for _, sub := range command.subs {
				if sub.who.admits(kind) {
					names = append(names, sub.name)
				}
			}
			about = strings.TrimSuffix(strings.Join(names, " | ")+": "+about, ": ")
		}
		fmt.Fprintf(&b, "  %-22s%s\n", command.name, about)
	}
	b.WriteString(usageRules)
	if kind == callerOther {
		b.WriteString(usageOfPeople)
	}
	return b.String()
}

// groupUsage lists each subcommand a caller of kind runs as the first usage form of its --help
// and what it is for.
func groupUsage(group string, subs []subcommand, kind callerKind) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: hl %s <command> ...\n", group)
	for _, sub := range subs {
		if !sub.who.admits(kind) {
			continue
		}
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
	kind := kindOf(c.Token)
	if len(args) == 0 {
		fmt.Fprintln(stderr, groupUsage(group, subs, kind))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, groupUsage(group, subs, kind))
		return 0
	}
	for _, sub := range subs {
		if sub.name != args[0] {
			continue
		}
		if !sub.who.admits(kind) {
			return fail(stderr, fmt.Errorf("%s %s: %w", group, sub.name, sub.who.refusal(kind)))
		}
		return sub.run(c, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "hl: %s: unknown subcommand %q\n\n%s\n", group, args[0], groupUsage(group, subs, kind))
	return 2
}

const (
	orgEnv     = "HL_ORG"
	projectEnv = "HL_PROJECT"
)

// orEnv is a flag's value, or the named environment variable when the flag was omitted.
func orEnv(flagValue, name string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(name)
}

// printLeading prints record as printJSON does with the keys lead ahead of the others: its
// description is long, and a reader of the first lines must not miss what it waits for.
func printLeading(stdout io.Writer, record map[string]any, lead ...string) {
	keys := make([]string, 0, len(record))
	for _, key := range lead {
		if _, has := record[key]; has {
			keys = append(keys, key)
		}
	}
	rest := make([]string, 0, len(record))
	for key := range record {
		if !slices.Contains(lead, key) {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	var b bytes.Buffer
	b.WriteString("{")
	for i, key := range append(keys, rest...) {
		name, _ := json.Marshal(key)
		value, err := json.MarshalIndent(record[key], "  ", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hl: encoding response: %s\n", err)
			return
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %s: %s", name, value)
	}
	b.WriteString("\n}")
	fmt.Fprintln(stdout, b.String())
}

// refusalOf is the reason and the message err was refused with by the platform, empty for any
// other error.
func refusalOf(err error) (reason, message string) {
	var refused *apiclient.HTTPError
	var body struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if !errors.As(err, &refused) || json.Unmarshal([]byte(refused.Body), &body) != nil {
		return "", ""
	}
	return body.Reason, body.Message
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
		return misuse(fs, "%s", asTyped(err.Error(), args))
	}
	return nil
}

// asTyped is a flag error of the standard library, which names every flag with one dash, naming
// the flag the way args spelled it.
func asTyped(msg string, args []string) string {
	for _, arg := range args {
		spelled, _, _ := strings.Cut(arg, "=")
		if !strings.HasPrefix(spelled, "--") {
			continue
		}
		if one := spelled[1:]; strings.HasSuffix(msg, ": "+one) {
			return strings.TrimSuffix(msg, one) + spelled
		} else if strings.Contains(msg, " "+one+": ") {
			return strings.Replace(msg, " "+one+": ", " "+spelled+": ", 1)
		}
	}
	return msg
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
	return fs.String("platform-experiment", "", "the platform experiment (default the token's)")
}

// namedExperiment is the platform experiment a command works in (see experimentOf), or a misuse
// when nothing names one.
func namedExperiment(fs *flagSet, c *apiclient.Client, flagValue string) (string, error) {
	if id := experimentOf(c, flagValue); id != "" {
		return id, nil
	}
	return "", misuse(fs, "want --platform-experiment: HL_TOKEN names none")
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
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	agent := fs.String("agent", "", "whose key it is, for a person or an API key; an agent's token says it")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	*pe = experimentOf(c, *pe)
	*agent = agentOf(c, *agent)
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

var platformExperimentCommands = []subcommand{
	{"list", "the platform experiments this token reaches, one line each", cmdPlatformExperimentsList, everyone},
	{"get", "the platform experiment: its status, what it waits for, its brief and its stages", cmdPlatformExperimentsGet, everyone},
	{"create", "create a platform experiment from a YAML file", cmdPlatformExperimentsCreate, ofPeople},
	{"update", "change the brief or other fields, or declare the baseline", cmdPlatformExperimentsUpdate, ofTheRun},
	{"propose", "a draft's planner: propose the experiment the draft becomes", cmdPlatformExperimentsPropose, asAgent},
	{"decline", "a draft's planner: say why no experiment is proposed", cmdPlatformExperimentsDecline, asAgent},
	{"close", "end the platform experiment for good", cmdPlatformExperimentsClose, ofTheRun},
	{"summary", "write the summary of the platform experiment", cmdPlatformExperimentsSummary, ofTheRun},
	{"agents", "the agents of a platform experiment", group("platform-experiments agents", agentsCommands), everyone},
}

// cmdPlatformExperiments is the group, with list as what it does when no command is named.
func cmdPlatformExperiments(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help") {
		return cmdPlatformExperimentsList(c, args, stdout, stderr)
	}
	return runGroup("platform-experiments", platformExperimentCommands, c, args, stdout, stderr)
}

func cmdPlatformExperimentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments list", `usage: hl platform-experiments list [--limit N] [--project PROJECT] [--shared[=false]]

One platform experiment per line as compact JSON, without description and summary
(hl platform-experiments get prints both). A closed one carries close_reason.
hl platform-experiments with no command is this list.`, stderr)
	limit := fs.Int("limit", 20, "page size (max 200)")
	project := fs.String("project", "", "only this project's platform experiments (default $HL_PROJECT)")
	shared := fs.Bool("shared", false, "only the platform experiments the platform shows every account, or with --shared=false none of them. Without it both are listed, the shared ones marked \"shared\": true. Read those like your own, write nothing to them")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
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
	var out json.RawMessage
	if err := c.Do("GET", "/platform-experiments?"+q.Encode(), nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "platform-experiments list: none", out, "description", "summary"); err != nil {
		return fail(stderr, err)
	}
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
  max_agents: 2               # researchers at work at once: baseline agents and the
                              # coordinator never count
  duration_hours: 24          # the run, from its start once the baseline is declared
  coordinator_hosted: true    # optional: the platform runs the coordinator
  metrics:
    - key: val_accuracy
      direction: maximize

Every field shown is required but coordinator_hosted. report_interval_seconds may be left out:
the platform's default applies.

It is created open and no run has started. Add the researchers while it is open
(hl platform-experiments agents add). It starts on a declared baseline: with coordinator_hosted
the platform creates a hosted baseline agent with the experiment, as accepting a draft does, and
declares the job it leaves when it finishes. Without it you do both: add an
agent with --role baseline, and once its job is COMPLETED with a summary, run
hl platform-experiments update --baseline-job JOB. That request starts the run when a
researcher has joined and starts_at, when the file states one, has passed; otherwise the
request that adds the researcher starts it, or starts_at does. While it is open
hl platform-experiments get answers in status_reason what the start waits for and your next step, and in
baseline_due_at when the platform closes it, with close_reason baseline_limit_reached, if it is
not running by then.

The experiment keeps its code in a repository on the deployment's git host. On GitHub,
repository: owner/name names one a GitHub account connected to the org covers. Left out, the
platform creates a private one there and the answer names it; repository_account says on
which account when the org connected several. With no account connected the answer is 422
github_not_connected: an owner connects GitHub in Settings first. On any other git host,
repository names one created there beforehand, and leaving it out is 422 repository_required.

One file is one platform experiment: for several, run the command once for each.

See controlplane/settings/examples/experiment.yaml for the full DSL reference.`, stderr)
	project := fs.String("project", "", "project the platform experiments belong to (default $HL_PROJECT)")
	path, err := parseOne(fs, args, "FILE", "-")
	if err != nil {
		return parseExit(err)
	}
	*project = orEnv(*project, projectEnv)
	body, err := readYAMLBody(path)
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	if _, several := body["experiments"]; several {
		return parseExit(misuse(fs, "%s holds an experiments: list, and one file is one platform experiment: write each as a file of its own and create it with a command of its own", path))
	}
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
	printJSON(stdout, out)
	return 0
}

// cmdPlatformExperimentsUpdate handles `hl platform-experiments update [FIELDS.yaml]`. FILE is a
// YAML file shaped exactly like the create body (see controlplane/settings/examples/
// experiment.yaml) — while the platform experiment is still open, every field including metrics
// is editable; once running, only name, description and duration_hours are.
func cmdPlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments update", `usage: hl platform-experiments update [--platform-experiment PE] [--description-file FILE] [--description TEXT] [--baseline-job JOB] [--name NAME] [FIELDS.yaml]

Changes the fields you name and no other. A coordinator's token changes the description and
declares the baseline, nothing else.

--baseline-job JOB declares the baseline while the platform experiment is open. The platform
writes JOB, its code_ref and what it measured into a block of the description it keeps through
later changes, and starts the run in that request when a researcher has joined and no starts_at
is ahead. The line printed after says whether it started, with status_reason. Refusals:
no_baseline (JOB is no baseline agent's job here), baseline_not_completed,
baseline_without_finding (no summary filed), baseline_missing_metric, baseline_breaks_constraint
and budget_below_baseline (the budget does not hold one trial per researcher at JOB's
estimate); the last two are the owner's to change. baseline_environment_differs and
baseline_not_in_environment: JOB did not run in the environment the head of the baseline branch
declares in .hypothesisloop/Dockerfile.

FIELDS.yaml maps the other fields that change (budget_accelerator_hours, max_agents,
duration_hours, metrics, report_interval_seconds): any while it is open, only name,
description and duration_hours once it runs. The result is checked like a create, so name and description stay
non-empty; max_agents below the researchers that joined is 409 max_agents_below_researchers.`, stderr)
	pe := experimentFlag(fs)
	name := fs.String("name", "", "the new name")
	description := fs.String("description", "", "the new description")
	descriptionFile := fs.String("description-file", "", "read the new description from this file, - for stdin")
	baselineJob := fs.String("baseline-job", "", "the completed `job` that measured the baseline")
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
	if *baselineJob != "" {
		body["baseline_job_id"] = *baselineJob
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
	var out map[string]any
	if err := c.Do("PATCH", "/platform-experiments/"+url.PathEscape(id), body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	if *baselineJob != "" {
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

func cmdPlatformExperimentsPropose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments propose", `usage: hl platform-experiments propose [--platform-experiment PE] [FILE|-]

For the planner of a draft. A proposal defines the experiment and runs nothing: no job, no
code. FILE is YAML of this shape, with every field shown required unless it says optional.
Every free-text value is a | block, a list item too (- |): unquoted text breaks on a colon or
a leading quote.

  experiment:
    name: |
      short title
    description: |
      the brief, the only thing the baseline and researchers read: the objective first, then
      the metric, gate, control, references and levers. How they are measured is the baseline's
      to choose. The platform adds the baseline job, its code_ref and values once declared.
    report_interval_seconds: 30        # how often a job is to report
    metrics:
      - key: val_accuracy              # one metric ranks: role ranking is the default. A job ranks
        direction: maximize            # by the last value it reports on it (or minimize), so a
                                       # harness's final report there is its result
      - key: exact_match               # optional gate
        direction: maximize
        role: constraint
        bound: 1                       # a value the unchanged control holds by construction
      - key: other_val_accuracy        # optional: one per outside system the goal names, its own
        direction: maximize            # number apart from the control. Shown beside the standings,
        role: reference                # never ranked or gating
  accelerator_type: ...                # copied verbatim from 'hl capacity', the CPU type for work on cores
  image: ...                           # the job image the brief names, from the same line of 'hl capacity':
                                       # one of its job_images, or its only_job_image where it shows one
  cpu: 8                               # optional, like memory and storage: what one node of a job
  memory: 32Gi                         # asks for, as job.cpu, job.memory and job.storage state it.
  storage: 100Gi                       # hl repo init writes them into job.yaml, so no brief repeats them
  budget_accelerator_hours: 6          # proposed; the person states the real ones at accept
  duration_hours: 4                    # the run, from its start: a time the goal states is this
  baseline_minutes: 30                 # what measuring the baseline needs from the accept, at
                                       # most planning.baseline_limit_minutes
  agents:                              # one entry per researcher
    - flavor: hyperparameter-search    # optional: generalist (the default), architecture-search,
      hyperparameters:                 #   data-search or hyperparameter-search. The direction you
        direction: |                   #   give a researcher goes in hyperparameters, any keys.
          the approach this one explores: a mechanism, never a list of values to try, since
          its researcher makes each claim under it a hypothesis of its own
    - {}
  metric_reason: |
    why this metric is the goal, in plain words
  measurement: |
    how it is measured, in plain words
  sizing_reason: |
    why this budget, duration and number of researchers are the least that reach the goal,
    worded as the estimate it is
  out_of_scope:                        # optional, and so are risks and questions
    - |
      what you left out
  risks:
    - |
      what could make the result mislead
  questions:
    - |
      what the goal left open and you decided
  repo_map: |                          # optional, at most 4000 bytes: what you read of the repository,
    a line per file or directory that matters, its entry points, how it builds and runs, and
    the toolchain and dependency pins. The baseline agent is given it when it starts.

A refusal names what to fix and changes nothing: for a file that is no YAML the line and what
is wrong there, for the image what a job submit would refuse (unknown_job_image,
no_cluster_for_image), and invalid_resources for a cpu, memory or storage that is no quantity.
One that passes is stored at once, replaces the one before and is what the person is asked to
accept, so send only the finished proposal. Your machine is kept some minutes after it, and a
revision the person asks for in that time arrives as your next update.`, stderr)
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
	quantitiesAsStrings(body)
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/proposal", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
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
    baseline_without_finding, baseline_missing_metric, baseline_breaks_constraint,
    budget_below_baseline
  open, its baseline declared: starts_at_ahead, no_researchers, budget_spent, starting
  running: agents_starting while no researcher's machine is up yet, then running
  running, every researcher finished, cut or stopped for good: researchers_done
  any, while a machine it wants is on a launcher that stopped asking: launcher_unreachable
An agent of the roster adds instance_starting, or launcher_unreachable while its launcher is
silent (hl platform-experiments agents list).
Once the baseline is declared, baseline_branch and baseline_code_ref name the branch and commit
of its job, where its harness is: the default branch may hold none of it.
On an open one the message begins with your own next step, worded for your token: the command
a coordinator or a person runs, what a baseline agent measures, or that a researcher's jobs
are refused until the start. status_reason is printed ahead of the record with id and status,
and nothing is written beside the JSON, so the output parses whole.`, stderr)
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
	if job, _ := out["baseline_job_id"].(string); job != "" {
		var baseline struct {
			Branch  string `json:"branch"`
			CodeRef string `json:"code_ref"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(job), nil, &baseline); err != nil {
			return fail(stderr, fmt.Errorf("reading the baseline job %s: %w", job, err))
		}
		out["baseline_branch"], out["baseline_code_ref"] = baseline.Branch, baseline.CodeRef
	}
	printLeading(stdout, out, "id", "status", "status_reason")
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
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": *text}, &out); err != nil {
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

// readYAMLItems reads the hypotheses of FILE (or stdin for "-"), one map each: a bare mapping is
// one, a bare list of mappings one per element, and a mapping holding a listKey list one per
// element of it, with every other top-level key the default of an item that does not set it.
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

func cmdWatch(client *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	apiURL, token := client.BaseURL, client.Token
	fs := newFlags("watch", `usage: hl watch [--agent A | --all [--decisions]] [--platform-experiment PE] [--kinds K,...] [--follow | --no-wait | --wake K,...]
   or: hl watch --experiment JOB [--until 'status in COMPLETED,FAILED,EVICTED']

Prints events, one JSON object per line. With an agent's token or --agent and no --experiment
or --all, the agent's own resources, resuming from where its last watch stopped. --all resumes the same way.
occurred_at is when the change was made, in UTC, the same on an event that happens while you
watch and on one replayed from before the watch. A change from before the platform kept the
time is replayed without it.
A job that waits is told as experiment.blocked and as job.held: at once when it was submitted
after a COMPLETED job of yours ended that has no summary (summary_gate), and for any other reason once, when it has waited longer than its
estimate. value is the code of what it waits for (not_admitted_reason), detail the sentence
(not_admitted_message).`, stderr)
	experiment := fs.String("experiment", "", "watch every event of one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "platform experiment to watch (default the token's unless --experiment is set)")
	agent := fs.String("agent", "", "watch this agent's own resources: its jobs' outcomes and the queue reasons waiting will not clear, its quota and cut, stage advances, the run's status and brief, and others' comments, findings and links on its hypotheses (default the token's agent unless --experiment or --all is set)")
	all := fs.Bool("all", false, "watch the whole platform experiment instead of one agent's resources")
	decisions := fs.Bool("decisions", false, "with --all, leave out what needs no decision: a job being queued, admitted or started, and a queue reason that waiting clears")
	kinds := fs.String("kinds", "", "comma-separated event kinds instead of the default set (everything but metric.point); see GET /watch/kinds")
	showSubscription := fs.Bool("show-subscription", false, "ask the server what this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'. A job ends COMPLETED, FAILED, EVICTED, REJECTED or CANCELLED")
	wake := fs.String("wake", "", "comma-separated event kinds, each KIND, KIND=VALUE for events of that value only or KIND!=VALUE for events of any other: return only once an event of one of them arrives, and print ahead of it the others that came before. Until then those are not printed and the next watch gets them again")
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

	var waking func(kind, value string) bool
	if *wake != "" {
		var kinds []struct {
			Kind string `json:"kind"`
		}
		if err := apiclient.New(apiURL, token).Do("GET", "/watch/kinds", nil, &kinds); err != nil {
			return fail(stderr, err)
		}
		known := map[string]bool{}
		for _, k := range kinds {
			known[k.Kind] = true
		}
		entries := strings.Split(*wake, ",")
		for _, entry := range entries {
			if kind, _, _ := cutWake(entry); !known[kind] {
				fmt.Fprintf(stderr, "hl watch: --wake: %q is no event kind; GET /watch/kinds lists them\n", kind)
				return 2
			}
		}
		waking = func(kind, value string) bool {
			return slices.ContainsFunc(entries, func(entry string) bool {
				wanted, of, wakes := cutWake(entry)
				return wanted == kind && wakes(of == value)
			})
		}
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
		Wake:               waking,
		Timeout:            time.Duration(*timeout * float64(time.Second)),
		Since:              *since,
		CursorFile:         cursorFile,
		Stdout:             stdout,
		Stderr:             stderr,
	})
}

// cutWake reads one entry of --wake: KIND wakes for every event of the kind, KIND=VALUE for
// those with that value and KIND!=VALUE for those with any other. wakes says, for whether an
// event's value is the entry's, whether the event wakes.
func cutWake(entry string) (kind, value string, wakes func(same bool) bool) {
	if kind, value, ok := strings.Cut(entry, "!="); ok {
		return kind, value, func(same bool) bool { return !same }
	}
	if kind, value, ok := strings.Cut(entry, "="); ok {
		return kind, value, func(same bool) bool { return same }
	}
	return entry, "", func(bool) bool { return true }
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
