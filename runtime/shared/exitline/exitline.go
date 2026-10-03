// Package exitline is the two lines a job's wrapper writes when the job's command ends, and how
// the runtime reads the exit code and when the command ran back out of the instance's output. A
// vendor's API does not have to report exit status or the command's times for this to work, so
// every vendor is read the same way.
package exitline

import (
	"slices"
	"strconv"
	"strings"
	"time"
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

const runPrefix = "hl-exec: ran "

const runTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatRun is the line for a command that ran from startedAt to endedAt, by the clock of the
// machine it ran on. The wrapper writes it just before the exit line.
func FormatRun(startedAt, endedAt time.Time) string {
	return runPrefix + startedAt.UTC().Format(runTimeLayout) + " " + endedAt.UTC().Format(runTimeLayout)
}

// FindRun returns the times in the last run line among lines, the wrapper's own for the same
// reason as in Find.
func FindRun(lines []string) (startedAt, endedAt time.Time, found bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		raw, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), runPrefix)
		if !ok {
			continue
		}
		from, to, ok := strings.Cut(raw, " ")
		if !ok {
			continue
		}
		startedAt, startErr := time.Parse(runTimeLayout, from)
		endedAt, endErr := time.Parse(runTimeLayout, to)
		if startErr != nil || endErr != nil {
			continue
		}
		return startedAt, endedAt, true
	}
	return time.Time{}, time.Time{}, false
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
		if own := strings.TrimSpace(lines[i]); strings.HasPrefix(own, prefix) || strings.HasPrefix(own, runPrefix) {
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
