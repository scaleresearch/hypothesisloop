package fly

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/supervise"
)

// outputPrefix starts every log line that is the command's, and endMark is the line the
// supervisor writes after the last of them. The vendor's log holds the lines of its own init, ssh
// server and kernel under the same source as the command's, in between them, and nothing in an
// entry tells them apart.
const (
	outputPrefix = "hl-out|"
	endMark      = "hl-ended"

	// drainSeconds is how long after the command ended the supervisor still passes on output
	// from whatever the command left running with its output open. Without the bound a daemon
	// the command started would keep a Machine whose command is gone running, and billed.
	drainSeconds = "5"
)

// supervisor is what a Machine runs in place of the bare command: the shared core with this
// vendor's ends. Before the command starts it opens a pipe and a reader that passes every line
// the command writes to stdout or stderr on under outputPrefix, a last line with no newline
// included. After the command has ended it waits for the reader, which ends when everything
// holding the pipe has, for drainSeconds at most, and then writes endMark and exits, which stops
// the Machine. The reader is waited for in a loop and killed before the mark is written, so
// nothing can follow the mark, whatever signal arrives meanwhile.
//
// It needs sh, mkfifo and sleep in the image and a temporary directory it can write to. Without
// them it says so under outputPrefix and does not run the command.
//
// Fly has no lifetime of its own for a Machine, so the supervisor counts the spec's: when it
// ends the command is killed, the supervisor exits as after any ending, and the Machine stops.
func supervisor(lifetime time.Duration) string {
	return supervise.Script(parts, lifetime)
}

var parts = supervise.Parts{
	Before: `refuse() {
  echo "` + outputPrefix + `hl-supervisor: $1, the command was not run"
  echo "` + endMark + `"
  exit 127
}
for tool in mkfifo sleep; do
  command -v "$tool" >/dev/null || refuse "the image has no $tool"
done
pipe="${TMPDIR:-/tmp}/.hl-supervisor-$$"
mkfifo "$pipe" || refuse "no pipe could be made at $pipe"
while IFS= read -r line || [ -n "$line" ]; do printf '` + outputPrefix + `%s\n' "$line"; done < "$pipe" &
reader=$!
exec 3> "$pipe"`,
	Output: `> "$pipe" 2>&1 3>&-`,
	After: `exec 3>&-
(sleep ` + drainSeconds + `; kill -KILL "$reader" 2>/dev/null) >/dev/null 2>&1 &
timer=$!
while kill -0 "$reader" 2>/dev/null; do wait "$reader"; done
kill -KILL "$timer" 2>/dev/null
echo "` + endMark + `"`,
}

// supervised is the argv that runs command under the supervisor for lifetime at most.
func supervised(lifetime time.Duration, command []string) []string {
	return append([]string{"/bin/sh", "-c", supervisor(lifetime), "hl-supervisor"}, command...)
}
