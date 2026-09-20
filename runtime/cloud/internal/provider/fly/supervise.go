package fly

// outputPrefix starts every log line that is the command's, and endMark is the line the
// supervisor writes after the last of them. The vendor's log holds the lines of its own init, ssh
// server and kernel under the same source as the command's, in between them, and nothing in an
// entry tells them apart.
const (
	outputPrefix = "hl-out|"
	endMark      = "hl-ended|"
)

// supervisor is what a Machine runs in place of the bare command. It runs the command once, with
// its argv as positional parameters that no shell parses, and passes every line the command
// writes to stdout or stderr on under outputPrefix, a last line with no newline included. The
// first SIGTERM goes on to the command; later ones are ignored, because Stop is repeated while
// the command saves and a second signal would start its handler again. When the command and
// everything holding its output have ended, the supervisor writes endMark with the exit code and
// exits with that code, which stops the Machine.
//
// It needs sh and mkfifo in the image and a temporary directory it can write to. Without them it
// says so under outputPrefix and does not run the command.
const supervisor = `refuse() {
  echo "` + outputPrefix + `hl-supervisor: $1, the command was not run"
  echo "` + endMark + `$2"
  exit "$2"
}
command -v mkfifo >/dev/null || refuse 'the image has no mkfifo' 127
pipe="${TMPDIR:-/tmp}/.hl-supervisor-$$"
mkfifo "$pipe" || refuse "no pipe could be made at $pipe" 126
while IFS= read -r line || [ -n "$line" ]; do printf '` + outputPrefix + `%s\n' "$line"; done < "$pipe" &
reader=$!
"$@" > "$pipe" 2>&1 &
child=$!
trap 'trap "" TERM; kill -TERM "$child" 2>/dev/null' TERM
while kill -0 "$child" 2>/dev/null; do wait "$child"; code=$?; done
wait "$reader"
echo "` + endMark + `$code"
exit "$code"`

// supervised is the argv that runs command under the supervisor.
func supervised(command []string) []string {
	return append([]string{"/bin/sh", "-c", supervisor, "hl-supervisor"}, command...)
}
