package clusterapi

import (
	"strconv"
	"strings"
	"time"
)

// LaunchCommand is the process one node of a job runs: launch, the job's launch script, with the
// node's command and arguments as its own.
func LaunchCommand(launch string, command, args []string) []string {
	return append(append([]string{"sh", "-c", launch, "hl-launch"}, command...), args...)
}

// ExitLinePrefix starts the line the launch script writes last, once the job's command has ended:
// ExitLinePrefix code=<exit code> started=<unix seconds> ended=<unix seconds>, the times by the
// clock of the machine it ran on. A runtime that sees a node only through its output reads how
// and when the command ended from it.
const ExitLinePrefix = "hl-launch:"

// ExitLine is what the last exit line among lines says. The script writes its line after the
// command has ended, so a line the command itself printed in the same form is never the last.
type ExitLine struct {
	Code               int
	StartedAt, EndedAt time.Time
}

// FindExitLine returns the last exit line among lines.
func FindExitLine(lines []string) (ExitLine, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		raw, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), ExitLinePrefix)
		if !ok {
			continue
		}
		if line, ok := parseExitLine(raw); ok {
			return line, true
		}
	}
	return ExitLine{}, false
}

func parseExitLine(raw string) (ExitLine, bool) {
	fields := map[string]int64{}
	for _, field := range strings.Fields(raw) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return ExitLine{}, false
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return ExitLine{}, false
		}
		fields[key] = n
	}
	code, hasCode := fields["code"]
	started, hasStarted := fields["started"]
	ended, hasEnded := fields["ended"]
	if len(fields) != 3 || !hasCode || !hasStarted || !hasEnded {
		return ExitLine{}, false
	}
	return ExitLine{Code: int(code), StartedAt: time.Unix(started, 0).UTC(), EndedAt: time.Unix(ended, 0).UTC()}, true
}
