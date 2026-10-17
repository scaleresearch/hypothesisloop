package fly

import (
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/supervise"
)

// outputPrefix starts every log line that is the command's, startMark is the line the supervisor
// writes before the first of them and endMark the one it writes after the last. The vendor's log
// holds the lines of its own init, ssh server and kernel under the same source as the command's,
// in between them, and nothing in an entry tells them apart.
//
// A Machine that is started a second time (by the vendor, by hand, or by a Resume of one whose
// snapshot the vendor dropped) boots the image's filesystem again, so nothing inside it can tell
// the supervisor that it ran before, and the command runs again into the same log. startMark is
// what keeps the two runs apart: a reader takes what follows the newest one (lastRun).
const (
	outputPrefix = "hl-out|"
	startMark    = "hl-started"
	endMark      = "hl-ended"

	// drainSeconds is how long after the command ended the supervisor still passes on output
	// from whatever the command left running with its output open. Without the bound a daemon
	// the command started would keep a Machine whose command is gone running, and billed.
	drainSeconds = "5"
)

// supervisor is what a Machine runs in place of the bare command: the shared core with this
// vendor's ends. It writes startMark first. Before the command starts it opens a pipe and a reader that passes every line
// the command writes to stdout or stderr on under outputPrefix, a last line with no newline
// included. After the command has ended it waits for the reader, which ends when everything
// holding the pipe has, for drainSeconds at most, and then writes endMark and exits, which stops
// the Machine. The reader is waited for in a loop and killed before the mark is written, so
// nothing can follow the mark, whatever signal arrives meanwhile.
//
// It needs sh, mkfifo and sleep in the image and a temporary directory it can write to. Without
// them it says so under outputPrefix and does not run the command.
func supervisor() string {
	return supervise.Script(parts, 0)
}

var parts = supervise.Parts{
	Before: `echo "` + startMark + `"
refuse() {
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

// supervised is the argv that runs command under the supervisor.
func supervised(command []string) []string {
	return append([]string{"/bin/sh", "-c", supervisor(), "hl-supervisor"}, command...)
}
