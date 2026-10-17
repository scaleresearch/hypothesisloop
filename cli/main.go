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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v4"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
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
		return command.run(client, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "hl: unknown command %q\n\n%s\n", args[0], usage(token))
	return 2
}

// subcommand is one command of hl or of a group. A group's listing takes each synopsis from the
// command's own --help, so the two cannot drift. Who may run a command is the platform's to say.
type subcommand struct {
	name, about string
	run         func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int
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
	{subcommand{"platform-experiments", "", cmdPlatformExperiments}, platformExperimentCommands},
	{subcommand{"quota", "your share of the budget and what a job submitted now may still hold of it; --all for every agent", cmdQuota}, nil},
	{subcommand{"results", "standings on the declared metrics", cmdResults}, nil},
	{subcommand{"capacity", "live accelerators per cluster, available and total", cmdCapacity}, nil},
	{subcommand{"situation", "the run now: time, objective, capacity, budget and each agent's work and allowances", cmdSituation}, nil},
	{subcommand{"data-key", `your S3 key to the experiment's data: eval "$(hl data-key)"`, cmdDataKey}, nil},
	{subcommand{"git-credential", "git's credential helper for your experiment's repository; git runs it", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return cmdGitCredential(c, os.Getenv("HL_GIT_URL"), args, os.Stdin, stdout, stderr)
	}}, nil},
	{subcommand{"hypothesis", "", cmdHypothesis}, hypothesisCommands},
	{subcommand{"job", "", group("job", jobCommands)}, jobCommands},
	{subcommand{"env", "", group("env", envCommands)}, envCommands},
	{subcommand{"repo", "the files every experiment's repository holds", group("repo", repoCommands)}, repoCommands},
	{subcommand{"org", "", group("org", orgCommands)}, orgCommands},
	{subcommand{"project", "", group("project", projectCommands)}, projectCommands},
	{subcommand{"api-key", "", group("api-key", apiKeyCommands)}, apiKeyCommands},
	{subcommand{"cluster", "", group("cluster", clusterCommands)}, clusterCommands},
	{subcommand{"watch", "your own updates, one JSON object per line", cmdWatch}, nil},
	{subcommand{"exec", "--stop-grace SECONDS -- COMMAND: a runtime's wrapper around a job's command", func(_ *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return cmdExec(args, stdout, stderr)
	}}, nil},
}

const usageRules = `
An ID is positional (--id ID works too). A command given a job's or a hypothesis's ID takes no
--platform-experiment; every other one in a platform experiment takes it as a flag, never an
operand. --agent and --platform-experiment default to what an agent's or a coordinator's token
names; a person or an API key writes both out. Where --agent filters a list it has no default.`

const usageOfPeople = ` --org defaults to $HL_ORG and
then to the one org the token reaches, --project to $HL_PROJECT. hl job --help, hl hypothesis
--help and the --help of org, project, api-key and cluster list their commands with every flag.`

// usage lists the commands of hl.
func usage(token string) string {
	var b strings.Builder
	b.WriteString("usage: hl <command> ...\n\n")
	for _, command := range commands {
		about := command.about
		if command.subs != nil {
			var names []string
			for _, sub := range command.subs {
				names = append(names, sub.name)
			}
			about = strings.TrimSuffix(strings.Join(names, " | ")+": "+about, ": ")
		}
		fmt.Fprintf(&b, "  %-22s%s\n", command.name, about)
	}
	b.WriteString(usageRules)
	if kindOf(token) == callerOther {
		b.WriteString(usageOfPeople)
	}
	return b.String()
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
	var body platformapi.ErrorModel
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
	if errors.As(err, &refused) && refused.Status == http.StatusForbidden {
		fmt.Fprintf(stderr, "hl: the platform refuses this token %s %s: %s\n", refused.Method, strings.SplitN(refused.Path, "?", 2)[0], refusalMessage([]byte(refused.Body)))
		return 1
	}
	if errors.As(err, &refused) {
		if invalid := invalidFields([]byte(refused.Body)); invalid != "" {
			refused.Body = invalid
		} else if problems := problemLines([]byte(refused.Body)); problems != "" {
			refused.Body = problems
		} else if body, jsonErr := compactJSON([]byte(refused.Body)); jsonErr == nil {
			refused.Body = string(body)
		}
	}
	fmt.Fprintf(stderr, "hl: %s\n", err)
	return 1
}

// refusalMessage is the platform's own words in a refusal's body, or the body as it is.
func refusalMessage(body []byte) string {
	var refusal platformapi.ErrorModel
	if json.Unmarshal(body, &refusal) == nil && refusal.Message != "" {
		return refusal.Message
	}
	return strings.TrimSpace(string(body))
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

// problemLines is a refusal that holds several problems, one line each, reason first, so all of
// them are fixed in one pass. Empty for an answer with fewer than two.
func problemLines(body []byte) string {
	var refusal struct {
		Problems []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"problems"`
	}
	if json.Unmarshal(body, &refusal) != nil || len(refusal.Problems) < 2 {
		return ""
	}
	lines := []string{fmt.Sprintf("%d problems, fix them all:", len(refusal.Problems))}
	for _, p := range refusal.Problems {
		lines = append(lines, "  "+p.Reason+": "+p.Message)
	}
	return strings.Join(lines, "\n")
}

// flagSet answers -h with the command's help and any misuse with one error line and the synopsis,
// the first paragraph of the help.
type flagSet struct {
	*flag.FlagSet
	help   string
	stderr io.Writer
	// stdinReader names the flag that read stdin, which only one can.
	stdinReader string
}

func newFlags(name, help string, stderr io.Writer) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, help: help, stderr: stderr}
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

// textFlag is free text given inline as --NAME or read by --NAME-file from a file, or stdin for -,
// where a shell neither expands nor splits it.
type textFlag struct {
	name   string
	inline *string
	file   *string
}

func newTextFlag(fs *flagSet, name, usage string) *textFlag {
	return &textFlag{
		name:   name,
		inline: fs.String(name, "", usage),
		file:   fs.String(name+"-file", "", "read --"+name+" from this `file`, - for stdin (a quoted heredoc: <<'EOF')"),
	}
}

// value is the text given by either flag, its trailing whitespace dropped, and empty when neither
// was. Giving both, or a file that holds no text, is a misuse.
func (t *textFlag) value(fs *flagSet) (string, error) {
	if *t.file == "" {
		return *t.inline, nil
	}
	if *t.inline != "" {
		return "", misuse(fs, "--%s and --%s-file are two ways to say one thing: give one", t.name, t.name)
	}
	var raw []byte
	var err error
	if *t.file == "-" {
		if err := fs.claimStdin("--" + t.name + "-file -"); err != nil {
			return "", err
		}
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*t.file)
	}
	if err != nil {
		return "", misuse(fs, "--%s-file: %s", t.name, err)
	}
	text := strings.TrimRightFunc(string(raw), unicode.IsSpace)
	if strings.TrimSpace(text) == "" {
		return "", misuse(fs, "--%s-file %s holds no text", t.name, *t.file)
	}
	return text, nil
}

// claimStdin records that what reads stdin, a misuse when another argument already does.
func (fs *flagSet) claimStdin(what string) error {
	if fs.stdinReader != "" {
		return misuse(fs, "%s and %s both read stdin: give one of them a file", fs.stdinReader, what)
	}
	fs.stdinReader = what
	return nil
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

// compactJSON re-encodes raw on one line in its own key order, leaving out every null,
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
				if s := v.String(); slices.Contains(drop, key) || s == "null" || s == `""` || s == "{}" {
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
where your jobs write. It opens nothing of another run: what a job of one of its prior_runs
(hl platform-experiments get) wrote, hl job data JOB lists with a url for each object. The
same key every time; it does not expire. Any S3 client works:
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
	{"list", "the platform experiments this token reaches, one line each", cmdPlatformExperimentsList},
	{"get", "the platform experiment: its status, what it waits for, its brief and its stages", cmdPlatformExperimentsGet},
	{"health", "where each agent and the coordinator last stood: first job, jobs in flight, restarts, messages", cmdPlatformExperimentsHealth},
	{"start", "push a directory to a repository and create the platform experiment its spec states", cmdPlatformExperimentsStart},
	{"update", "change the brief or other fields, or declare or raise the baseline", cmdPlatformExperimentsUpdate},
	{"propose", "a draft's planner: propose the experiment the draft becomes", cmdPlatformExperimentsPropose},
	{"decline", "a draft's planner: say why no experiment is proposed", cmdPlatformExperimentsDecline},
	{"close", "end the platform experiment for good", cmdPlatformExperimentsClose},
	{"reopen", "set a closed platform experiment running again until TIME, as it was", cmdPlatformExperimentsReopen},
	{"summary", "write the summary of the platform experiment", cmdPlatformExperimentsSummary},
	{"agents", "the agents of a platform experiment", group("platform-experiments agents", agentsCommands)},
}

// cmdPlatformExperiments is the group, with list as what it does when no command is named.
func cmdPlatformExperiments(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help") {
		return cmdPlatformExperimentsList(c, args, stdout, stderr)
	}
	return runGroup("platform-experiments", platformExperimentCommands, c, args, stdout, stderr)
}

func cmdPlatformExperimentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments list", `usage: hl platform-experiments list [--limit N] [--offset N] [--project PROJECT] [--shared[=false]]

One platform experiment per line as compact JSON, without description and summary
(hl platform-experiments get prints both). A closed one carries close_reason.
hl platform-experiments with no command is this list.`, stderr)
	limit := fs.Int("limit", 20, "page size (max 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	project := fs.String("project", "", "only this project's platform experiments (default $HL_PROJECT)")
	shared := fs.Bool("shared", false, "only the platform experiments the platform shows every account, or with --shared=false none of them. Without it both are listed, the shared ones marked \"shared\": true. Read those like your own, write nothing to them")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *offset != 0 {
		q.Set("offset", strconv.Itoa(*offset))
	}
	if p := orEnv(*project, projectEnv); p != "" {
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
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "platform-experiments list: none", out, "description", "summary"); err != nil {
		return fail(stderr, err)
	}
	var shown []json.RawMessage
	_ = json.Unmarshal(out, &shown)
	pageNotice(stderr, "platform-experiments list", header, *offset, len(shown))
	return 0
}

// cmdPlatformExperimentsUpdate handles `hl platform-experiments update [FIELDS.yaml]`. FIELDS.yaml
// holds keys of the experiment spec: while the platform experiment is still open, every field
// including metrics is editable; once running, only name, description, duration_hours and the
// baseline are.
func cmdPlatformExperimentsUpdate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments update", `usage: hl platform-experiments update [--platform-experiment PE] [--description TEXT | --description-file FILE|-] [--baseline-job JOB [--reason TEXT | --reason-file FILE|-]] [--name NAME] [FIELDS.yaml]

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
	pe := experimentFlag(fs)
	name := fs.String("name", "", "the new name")
	descriptionFlag := newTextFlag(fs, "description", "the new description")
	baselineJob := fs.String("baseline-job", "", "the completed `job` that measured the baseline")
	reasonFlag := newTextFlag(fs, "reason", "with --baseline-job on a running experiment: why the baseline is raised to it")
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
	description, err := descriptionFlag.value(fs)
	if err != nil {
		return parseExit(err)
	}
	reason, err := reasonFlag.value(fs)
	if err != nil {
		return parseExit(err)
	}
	body := map[string]any{}
	if len(operands) == 1 {
		if operands[0] == "-" {
			if err := fs.claimStdin("FIELDS.yaml -"); err != nil {
				return parseExit(err)
			}
		}
		if body, err = readYAMLBody(operands[0]); err != nil {
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
		return parseExit(misuse(fs, "nothing to change: name a field"))
	}
	var out map[string]any
	if err := c.Do("PATCH", "/platform-experiments/"+url.PathEscape(id), body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
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

func cmdPlatformExperimentsPropose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments propose", `usage: hl platform-experiments propose [--platform-experiment PE] [--check] [FILE|-]

For the planner of a draft. A proposal defines the experiment and runs nothing: no job, no
code. --check runs the platform's whole check of FILE and stores nothing: it prints every
problem at once, or that the proposal passes. Try variants with --check; propose only the one
you mean, since a propose that passes is stored at once.
FILE is YAML of this shape, with every field shown required unless it says optional.
Every free-text value is a | block, a list item too (- |): unquoted text breaks on a colon or
a leading quote.

  goal_version: 1                      # the draft's goal_version you planned from: a proposal for
                                       # an older goal is refused with goal_changed
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
  image: ...                           # the job image the brief names, from the same line of 'hl capacity':
                                       # one of its job_images, or its only_job_image where it shows one
  cpu: 8                               # optional, like memory and storage: what one node of a job
  memory: 32Gi                         # asks for, as job.cpu, job.memory and job.storage state it.
  storage: 100Gi                       # hl repo init writes them into job.yaml, so no brief repeats them
  budget_accelerator_hours: 6          # proposed, positive; the person states the real ones at accept
  duration_hours: 4                    # the run, from its start: a time the goal states is this
  baseline_minutes: 60                 # what measuring the baseline needs from the accept, from
                                       # planning.min_baseline_minutes (45) to
                                       # planning.baseline_limit_minutes
  agents:                              # one entry per researcher
    - agent_type: data-search          # optional: generalist (the default), architecture-search,
      hyperparameters:                 #   hyperparameter-search or data-search. The direction you
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
  questions:                           # each decision the goal left open, with the answer you
    - question: |                      # planned on. A proposal with questions waits for the
        how big a win counts           # person to confirm or change each answer, which then goes
      answer: |                        # into the brief; one without is accepted as proposed
        1.5x the control's throughput, since the goal states no margin
  repo_map: |                          # optional, at most 4000 bytes: what you read of the repository,
    a line per file or directory that matters, its entry points, how it builds and runs, and
    the toolchain and dependency pins. The baseline agent is given it when it starts.

A refusal names every problem at once and changes nothing: for a file that is no YAML the line
and what is wrong there, accelerator_unreachable with the types that are listed, for the image
what a job submit would refuse (unknown_job_image, no_cluster_for_image), and invalid_resources
for a cpu, memory or storage that is no quantity. Without --check, one that passes is stored at
once, replaces the one before and is what the person is asked to accept, so propose only the
finished proposal. Your machine is kept some minutes after it, and a change of the goal the
person makes in that time arrives as your next update.`, stderr)
	pe := experimentFlag(fs)
	check := fs.Bool("check", false, "check the proposal and store nothing")
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
	textsAsLists(body)
	path = "/platform-experiments/" + url.PathEscape(id) + "/proposal"
	if *check {
		path += "?check=true"
	}
	var out any
	if err := c.Do("PUT", path, body, &out); err != nil {
		return fail(stderr, err)
	}
	if *check {
		fmt.Fprintln(stdout, "the proposal passes every check; nothing is stored. Propose it without --check when it is the one you mean.")
		return 0
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

// textsAsLists sends a proposal's out_of_scope or risks written as one | block as the one-item
// list the API reads. A planner was refused 422 "expected array" for questions: |.
func textsAsLists(body map[string]any) {
	for _, key := range []string{"out_of_scope", "risks"} {
		if text, ok := body[key].(string); ok {
			body[key] = []any{text}
		}
	}
}

func cmdPlatformExperimentsDecline(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments decline", `usage: hl platform-experiments decline [--platform-experiment PE] --goal-version N --reason TEXT | --reason-file FILE|-

For the planner of a draft, when the goal cannot be made into an experiment: nothing measurable,
or nothing this platform can run. The reason is shown to the person who wrote the goal in place
of a proposal, so say in plain words why, and what would make it plannable. A proposal you made
before no longer stands. --goal-version is the draft's goal_version you answer: a decline of an
older goal is refused with goal_changed. The person may change the goal, which starts you again.`, stderr)
	pe := experimentFlag(fs)
	goalVersion := fs.Int("goal-version", 0, "the draft's goal_version the decline answers")
	reasonFlag := newTextFlag(fs, "reason", "why no experiment is proposed")
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	if *goalVersion < 1 {
		return parseExit(misuse(fs, "want --goal-version, the draft's goal_version"))
	}
	reason, err := reasonFlag.value(fs)
	if err != nil {
		return parseExit(err)
	}
	if strings.TrimSpace(reason) == "" {
		return parseExit(misuse(fs, "want --reason"))
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/decline", map[string]any{"reason": reason, "goal_version": *goalVersion}, &out); err != nil {
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

func cmdPlatformExperimentsHealth(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments health", `usage: hl platform-experiments health [--platform-experiment PE]

Where each agent of the platform experiment, and its hosted coordinator, last stood, one line
each as compact JSON, as the metrics store recorded it while the run was open or running:
first_job_seconds and first_completed_job_seconds from when it could first submit (absent
before there is one), jobs_in_flight, restarts of its machine over the whole run,
coordinator_messages sent to it, and for a hosted agent rate_limit_utilization, the share of
its model account's rate limit used, and while the model provider refuses its calls
rate_limited_until, when the limit resets: it waits until then and resumes. The coordinator's
line has role coordinator and no agent_id.`, stderr)
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	var out struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(id)+"/health", nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "platform-experiments health: nothing recorded yet", out.Agents); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdPlatformExperimentsClose(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments close", `usage: hl platform-experiments close [--reason TEXT | --reason-file FILE|-] [--platform-experiment PE]

Ends the platform experiment for good: every job still running
is evicted, nothing more is accepted, and the standings are taken from the metrics reported. Its
summary can still be written afterwards. --reason says why: it is kept as close_message, written
in the platform's record of the close, and told to the coordinator and the agents with the close.`, stderr)
	reasonFlag := newTextFlag(fs, "reason", "why it is closed, in a sentence or two")
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	reason, err := reasonFlag.value(fs)
	if err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	body := map[string]any{}
	if reason != "" {
		body["reason"] = reason
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/close", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsReopen(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments reopen", `usage: hl platform-experiments reopen [--platform-experiment PE] --ends-at TIME [--coordinator-model M]

Staff only. Sets a closed platform experiment, whose run started, running again until TIME
(RFC 3339) as the same run: its hypotheses, jobs, branches, brief, baseline and agents stay. Every
agent whose run ended only with the close is at work again, and so is a hosted coordinator, on
--coordinator-model when it is given. An agent that finished or was cut before the close stays as
it ended.`, stderr)
	endsAt := fs.String("ends-at", "", "when the run closes again, RFC 3339")
	model := fs.String("coordinator-model", "", "the model the hosted coordinator runs on from now (default the one it has)")
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	if *endsAt == "" {
		return parseExit(misuse(fs, "want --ends-at"))
	}
	end, err := time.Parse(time.RFC3339, *endsAt)
	if err != nil {
		return parseExit(misuse(fs, "--ends-at %q is no RFC 3339 time, such as 2026-10-12T18:00:00Z", *endsAt))
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	body := map[string]any{"ends_at": end.Format(time.RFC3339)}
	if *model != "" {
		body["coordinator_model"] = *model
	}
	var out any
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(id)+"/reopen", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
}

func cmdPlatformExperimentsSummary(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments summary", `usage: hl platform-experiments summary [--platform-experiment PE] --text T | --text-file FILE|-

Writes the summary of the platform experiment: what the run
found and what the next one should do differently. Each write replaces the one before, so write
the whole text; hl platform-experiments get shows it as summary. It can be written while the
experiment runs and after it closed.`, stderr)
	summary := newTextFlag(fs, "text", "the summary")
	fs.StringVar(summary.file, "file", "", "")
	pe := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := namedExperiment(fs, c, *pe)
	if err != nil {
		return parseExit(err)
	}
	text, err := summary.value(fs)
	if err != nil {
		return parseExit(err)
	}
	if strings.TrimSpace(text) == "" {
		return parseExit(misuse(fs, "want --text or --text-file"))
	}
	var out any
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": text}, &out); err != nil {
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

Prints changes, one JSON object per line, each with its id. With an agent's token or --agent and
no --experiment or --all, the agent's own resources, reading on from where its last watch stopped.
--all reads on the same way. occurred_at is when the change was made, in UTC. Changes are held
for 7 days: a watch whose saved place is older prints one line of kind watch.reset, reads on
from the newest change, and returns; re-read state (hl situation) then.
A job that waits is told as experiment.blocked, and as job.held once, when it has waited longer
than its estimate. value is the code of what it waits for (not_admitted_reason), detail the sentence
(not_admitted_message).`, stderr)
	experiment := fs.String("experiment", "", "watch every event of one job (domain.Experiment id)")
	platformExperiment := fs.String("platform-experiment", "", "platform experiment to watch (default the token's unless --experiment is set)")
	agent := fs.String("agent", "", "watch this agent's own resources: its jobs' outcomes and the queue reasons waiting will not clear, its quota and cut, stage advances, the run's status and brief, others' comments, findings and links on its hypotheses, and others' comments on the objective (default the token's agent unless --experiment or --all is set)")
	all := fs.Bool("all", false, "watch the whole platform experiment instead of one agent's resources")
	decisions := fs.Bool("decisions", false, "with --all, leave out what needs no decision: a job being queued, admitted or started, and a queue reason that waiting clears")
	kinds := fs.String("kinds", "", "comma-separated event kinds instead of every kind; see GET /watch/kinds")
	showSubscription := fs.Bool("show-subscription", false, "print what the server says this connection is subscribed to")
	until := fs.String("until", "", "stop when a job or platform experiment status event has one of these values; one form only: 'status in COMPLETED,FAILED,EVICTED' or 'status in running'. A job ends COMPLETED, FAILED, EVICTED, REJECTED or CANCELLED")
	wake := fs.String("wake", "", "comma-separated event kinds, each KIND, KIND=VALUE for events of that value only, KIND!=VALUE for events of any other, and KIND@SUBJECT, alone or ahead of =VALUE or !=VALUE, for events of that subject only: return only once an event of one of them arrives, and print ahead of it the others that came before. Until then those are not printed and the next watch gets them again")
	noWait := fs.Bool("no-wait", false, "print the changes already waiting and return once caught up, without listening for more")
	follow := fs.Bool("follow", false, "without --until, stream until the timeout instead of returning with the first batch of events")
	timeout := fs.Float64("timeout", 540.0, "give up after this many seconds")
	after := fs.Int64("after", 0, "read on from the change after this id instead of the saved place")
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

	// An agent watching its own resources with every kind reads on where its last watch stopped,
	// and so does a watch of the whole platform experiment. Any other subscription would skip
	// changes of kinds it did not carry.
	var cursorFile string
	if (*agent != "" || *all) && *platformExperiment != "" && *experiment == "" && *kinds == "" {
		dir, err := stateDir()
		if err != nil {
			fmt.Fprintf(stderr, "hl watch: %s\n", err)
			return 2
		}
		// No agent id is empty, so a whole experiment's cursor is no agent's file.
		cursorFile = filepath.Join(dir, "changes-"+url.PathEscape(*platformExperiment)+"-"+url.PathEscape(*agent)+".json")
	}

	var waking func(kind, subject, value string) bool
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
			if kind := cutWake(entry).kind; !known[kind] {
				fmt.Fprintf(stderr, "hl watch: --wake: %q is no event kind; GET /watch/kinds lists them\n", kind)
				return 2
			}
		}
		waking = func(kind, subject, value string) bool {
			return slices.ContainsFunc(entries, func(entry string) bool {
				return cutWake(entry).wakes(kind, subject, value)
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
		After:              *after,
		CursorFile:         cursorFile,
		Stdout:             stdout,
		Stderr:             stderr,
	})
}

// wakeEntry is one entry of --wake: KIND[@SUBJECT][=VALUE|!=VALUE].
type wakeEntry struct {
	kind, subject, value string
	onSubject, onValue   bool
	// differs: the entry wakes for an event of any value but value.
	differs bool
}

// cutWake reads one entry of --wake: KIND wakes for every event of the kind, KIND@SUBJECT for
// those of that subject, and =VALUE or !=VALUE after either for those with that value and those
// with any other.
func cutWake(entry string) wakeEntry {
	var w wakeEntry
	rest := entry
	if before, value, ok := strings.Cut(rest, "!="); ok {
		rest, w.value, w.onValue, w.differs = before, value, true, true
	} else if before, value, ok := strings.Cut(rest, "="); ok {
		rest, w.value, w.onValue = before, value, true
	}
	w.kind = rest
	if kind, subject, ok := strings.Cut(rest, "@"); ok {
		w.kind, w.subject, w.onSubject = kind, subject, true
	}
	return w
}

func (w wakeEntry) wakes(kind, subject, value string) bool {
	if w.kind != kind || (w.onSubject && w.subject != subject) {
		return false
	}
	return !w.onValue || (value == w.value) != w.differs
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
