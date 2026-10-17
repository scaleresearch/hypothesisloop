package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const (
	OrgEnv     = "HL_ORG"
	ProjectEnv = "HL_PROJECT"
)

// OrEnv is a flag's value, or the named environment variable when the flag was omitted.
func OrEnv(flagValue, name string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(name)
}

// Flags answers -h with the command's help and any misuse with one error line and the synopsis,
// the first paragraph of the help.
type Flags struct {
	*flag.FlagSet
	help   string
	stderr io.Writer
	// stdinReader names the flag that read stdin, which only one can.
	stdinReader string
}

func NewFlags(name, help string, stderr io.Writer) *Flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &Flags{FlagSet: fs, help: help, stderr: stderr}
}

func (fs *Flags) Parse(args []string) error {
	err := fs.FlagSet.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(fs.stderr, fs.help)
		fs.printFlags()
		return err
	}
	if err != nil && strings.HasSuffix(err.Error(), "not defined: -platform-experiment") {
		fmt.Fprintf(fs.stderr, "hl: %s: takes no --platform-experiment, leave it out: an ID is found on the whole platform, and a command given none is not about one platform experiment\n", fs.Name())
		return ErrUsage
	}
	if err != nil {
		return Misuse(fs, "%s", asTyped(err.Error(), args))
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
func (fs *Flags) printFlags() {
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

// TextFlag is free text given inline as --NAME or read by --NAME-file from a file, or stdin for -,
// where a shell neither expands nor splits it.
type TextFlag struct {
	name   string
	inline *string
	File   *string
}

func NewTextFlag(fs *Flags, name, usage string) *TextFlag {
	return &TextFlag{
		name:   name,
		inline: fs.String(name, "", usage),
		File:   fs.String(name+"-file", "", "read --"+name+" from this `file`, - for stdin (a quoted heredoc: <<'EOF')"),
	}
}

// Value is the text given by either flag, its trailing whitespace dropped, and empty when neither
// was. Giving both, or a file that holds no text, is a misuse.
func (t *TextFlag) Value(fs *Flags) (string, error) {
	if *t.File == "" {
		return *t.inline, nil
	}
	if *t.inline != "" {
		return "", Misuse(fs, "--%s and --%s-file are two ways to say one thing: give one", t.name, t.name)
	}
	var raw []byte
	var err error
	if *t.File == "-" {
		if err := fs.ClaimStdin("--" + t.name + "-file -"); err != nil {
			return "", err
		}
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*t.File)
	}
	if err != nil {
		return "", Misuse(fs, "--%s-file: %s", t.name, err)
	}
	text := strings.TrimRightFunc(string(raw), unicode.IsSpace)
	if strings.TrimSpace(text) == "" {
		return "", Misuse(fs, "--%s-file %s holds no text", t.name, *t.File)
	}
	return text, nil
}

// ClaimStdin records that what reads stdin, a misuse when another argument already does.
func (fs *Flags) ClaimStdin(what string) error {
	if fs.stdinReader != "" {
		return Misuse(fs, "%s and %s both read stdin: give one of them a file", fs.stdinReader, what)
	}
	fs.stdinReader = what
	return nil
}

var ErrUsage = errors.New("usage")

// Misuse reports a wrong invocation followed by the command's synopsis.
func Misuse(fs *Flags, format string, a ...any) error {
	synopsis, _, _ := strings.Cut(fs.help, "\n\n")
	fmt.Fprintf(fs.stderr, "hl: %s: %s\n%s\n", fs.Name(), fmt.Sprintf(format, a...), synopsis)
	return ErrUsage
}

// ParseArgs parses flags placed before, between or after the positional arguments.
func ParseArgs(fs *Flags, args []string) ([]string, error) {
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

// ParseOperands returns the positional arguments followed by the values of the alias flags, which
// take the same arguments so that a first guess at the form works: hl job status ID and --id ID.
func ParseOperands(fs *Flags, argv []string, aliases ...string) ([]string, error) {
	var flagged StringList
	for _, name := range aliases {
		fs.Var(&flagged, name, "")
	}
	positional, err := ParseArgs(fs, argv)
	if err != nil {
		return nil, err
	}
	return append(positional, flagged...), nil
}

// ParseOne is ParseOperands for a command taking exactly one operand; def stands in for a missing
// one when set.
func ParseOne(fs *Flags, argv []string, what, def string, aliases ...string) (string, error) {
	got, err := ParseOperands(fs, argv, aliases...)
	if err != nil {
		return "", err
	}
	if len(got) == 0 && def != "" {
		return def, nil
	}
	if len(got) != 1 {
		return "", Misuse(fs, "want one %s, got %d", what, len(got))
	}
	return got[0], nil
}

// ExperimentFlag registers --platform-experiment, the one way a command names its platform
// experiment. It is never an operand and has no shorter spelling.
func ExperimentFlag(fs *Flags) *string {
	return fs.String("platform-experiment", "", "the platform experiment (default the token's)")
}

// NamedExperiment is the platform experiment a command works in (see apiclient.Client.Experiment), or a misuse
// when nothing names one.
func NamedExperiment(fs *Flags, c *apiclient.Client, flagValue string) (string, error) {
	if id := c.Experiment(flagValue); id != "" {
		return id, nil
	}
	return "", Misuse(fs, "want --platform-experiment: HL_TOKEN names none")
}

// ParseExit is the exit code for a parse error: 0 for an explicit -h or --help, else 2.
func ParseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// ParseNone parses the flags of a command that takes no operand.
func ParseNone(fs *Flags, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return Misuse(fs, "unexpected argument %q", fs.Arg(0))
	}
	return nil
}

type StringList []string

func (s *StringList) String() string { return strings.Join(*s, ",") }

func (s *StringList) Set(v string) error { *s = append(*s, v); return nil }
