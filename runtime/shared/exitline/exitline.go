// Package exitline is the one line a job's wrapper writes when the job's command ends, and how
// the runtime reads the exit code back out of the instance's output. A vendor's API does not
// have to report exit status for this to work, so every vendor is read the same way.
package exitline

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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

// lastOutputBytes bounds what LastOutput returns: it travels in a job's failure reason, behind
// the runtime's own words and inside domain's failureCauseDetailRunes.
const lastOutputBytes = 300

// lastOutputLines is how many lines LastOutput carries: an error is often followed by a few lines
// of advice, and the last line alone is then the advice.
const lastOutputLines = 5

// lastOutputSeparator stands between the lines of LastOutput, which is itself one line.
const lastOutputSeparator = " | "

// LastOutput is the last things a job said among lines, as one printable line of at most
// lastOutputBytes bytes: its last lastOutputLines lines that are neither blank nor the wrapper's
// exit line, the most recent last, as many of them as fit whole. The last line alone is cut where
// it does not fit. Empty when the job said nothing.
func LastOutput(lines []string) string {
	var said []string
	size := 0
	for i := len(lines) - 1; i >= 0 && len(said) < lastOutputLines; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), prefix) {
			continue
		}
		printable := strings.Map(func(r rune) rune {
			if r == utf8.RuneError || !unicode.IsPrint(r) {
				return ' '
			}
			return r
		}, lines[i])
		line := strings.Join(strings.Fields(printable), " ")
		if line == "" {
			continue
		}
		if len(said) == 0 {
			for len(line) > lastOutputBytes {
				_, last := utf8.DecodeLastRuneInString(line)
				line = line[:len(line)-last]
			}
		} else if size+len(lastOutputSeparator)+len(line) > lastOutputBytes {
			break
		} else {
			size += len(lastOutputSeparator)
		}
		size += len(line)
		said = append(said, line)
	}
	slices.Reverse(said)
	return strings.Join(said, lastOutputSeparator)
}
