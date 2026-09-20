// Package exitline is the one line a job's wrapper writes when the job's command ends, and how
// the runtime reads the exit code back out of the instance's output. A vendor's API does not
// have to report exit status for this to work, so every vendor is read the same way.
package exitline

import (
	"strconv"
	"strings"
)

const prefix = "hl-exec: exit-code="

// Format is the line for a command that ended with code.
func Format(code int) string {
	return prefix + strconv.Itoa(code)
}

// Find returns the exit code in the last exit line among lines. The wrapper writes its line, on a
// line of its own, after the command has ended, so a line the command itself printed in the same
// form is never the last.
func Find(lines []string) (code int, found bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		raw, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), prefix)
		if !ok {
			continue
		}
		code, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		return code, true
	}
	return 0, false
}
