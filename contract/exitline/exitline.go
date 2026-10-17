// Package exitline is the two lines a job's wrapper writes when the job's command ends, and how
// the runtime reads the exit code and when the command ran back out of the instance's output. A
// vendor's API does not have to report exit status or the command's times for this to work, so
// every vendor is read the same way.
package exitline

import (
	"strconv"
	"strings"
	"time"
)

// The wrapper is hl exec, which a job image carries: the command name, and the flag that sets how
// many seconds the job's command runs on after SIGTERM.
const (
	WrapperCommand = "exec"
	StopGraceFlag  = "stop-grace"
)

// Wrap is the command line that runs command under the wrapper, which lets it run stopGrace
// seconds after SIGTERM.
func Wrap(stopGrace int64, command []string) []string {
	return append([]string{"hl", WrapperCommand, "--" + StopGraceFlag, strconv.FormatInt(stopGrace, 10), "--"}, command...)
}

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
