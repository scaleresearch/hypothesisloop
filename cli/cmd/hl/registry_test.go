package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

// Every command of the table answers -h with its own usage, so a row that names the wrong
// command.Func fails here.
func TestEveryCommandAnswersHelpWithTheUsageOfItsPath(t *testing.T) {
	t.Setenv("HL_TOKEN", "")
	t.Setenv("API_URL", "")
	var walk func(path []string, cmds []command.Command)
	walk = func(path []string, cmds []command.Command) {
		for _, cmd := range cmds {
			at := append(append([]string{}, path...), cmd.Name)
			if cmd.Subs != nil {
				walk(at, cmd.Subs)
				continue
			}
			var out bytes.Buffer
			code := run(append(at, "-h"), &out, &out)
			want := "usage: hl " + strings.Join(at, " ")
			if code != 0 || !(strings.HasPrefix(out.String(), want+" ") || strings.HasPrefix(out.String(), want+"\n")) {
				first, _, _ := strings.Cut(out.String(), "\n")
				t.Errorf("hl %s -h: exit code %d, first line %q, want 0 and %q", strings.Join(at, " "), code, first, want)
			}
		}
	}
	walk(nil, commands)
}
