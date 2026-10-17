package command

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// Func runs one command of hl on the arguments after its name.
type Func func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int

// Command is one command of hl or of a group. One with Subs is a group, run as Group unless it
// has a Run of its own. A group's listing takes each synopsis from the command's own --help, so
// the two cannot drift. Who may run a command is the platform's to say.
type Command struct {
	Name, About string
	Run         Func
	Subs        []Command
}

// Runner is what runs the command at path, the words that name it after hl.
func (cmd Command) Runner(path string) Func {
	if cmd.Run != nil {
		return cmd.Run
	}
	return func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return Group(path, cmd.Subs, c, args, stdout, stderr)
	}
}

// GroupUsage lists each subcommand as the first usage form of its --help and what it is for.
func GroupUsage(group string, subs []Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: hl %s <command> ...\n", group)
	for _, sub := range subs {
		var help bytes.Buffer
		// A subcommand may read its defaults from the client's token before it parses its flags.
		sub.Runner(group+" "+sub.Name)(&apiclient.Client{}, []string{"-h"}, &help, &help)
		usage, _, _ := strings.Cut(help.String(), "\n\n")
		usage, _, _ = strings.Cut(usage, "\n   or:")
		synopsis := strings.Join(strings.Fields(strings.TrimPrefix(usage, "usage: hl "+group+" ")), " ")
		fmt.Fprintf(&b, "\n  %s\n    %s", synopsis, sub.About)
	}
	return b.String()
}

// Group runs the subcommand of group that args name.
func Group(group string, subs []Command, c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, GroupUsage(group, subs))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, GroupUsage(group, subs))
		return 0
	}
	for _, sub := range subs {
		if sub.Name == args[0] {
			return sub.Runner(group+" "+sub.Name)(c, args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "hl: %s: unknown subcommand %q\n\n%s\n", group, args[0], GroupUsage(group, subs))
	return 2
}
