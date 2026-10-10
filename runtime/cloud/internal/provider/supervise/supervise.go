package supervise

import (
	"strconv"
	"strings"
	"time"
)

// Parts are an adapter's own ends of a supervisor, as shell text. Each may be empty.
type Parts struct {
	// Before runs once the signal rule is in place and before the command starts.
	Before string
	// Output is the redirection the command is started with, such as `> "$out" 2>&1`.
	Output string
	// Started runs right after the command has started, beside it.
	Started string
	// After runs once the command has ended.
	After string
	// EndsAtFile, when set, is the absolute path the end of a lifetime is kept at, in seconds
	// since the epoch, so that something outside the supervisor can move it (provider.Extender).
	EndsAtFile string
}

// Script is the supervisor made of the core and an adapter's parts. It runs its positional
// parameters as the command, once, so no shell parses the argv, and leaves the command's process
// id in $child for the parts.
//
// The signal rule is the same for every vendor: the first SIGTERM the supervisor gets goes on to
// the command, and later ones are ignored, because a stop is repeated while the command saves
// and a second signal would start the command's handler again. The command alone decides when
// it ends: nothing follows the signal.
//
// The trap is set before anything else. A SIGTERM that comes before the command was started
// means the command is not started at all, and After still runs: a shell that has just been
// forked to become the command swallows a signal (dash does, when its parent has a trap), so
// starting the command only to signal it at once would leave it running unsignalled. The same
// swallowing leaves one gap this text cannot close: a first SIGTERM in the instant between the
// fork and the command's exec is lost, and the command then ends only when it is destroyed.
//
// A lifetime above zero is counted inside the instance from the supervisor's start, so it runs
// out whether or not anything outside can still reach the instance. At its end the command is
// killed, or not started when it has not been yet, and After runs as it does after any ending.
// $expired is set from then on, which is how After tells this ending from the command's own.
// It needs sleep in the image. Zero is a supervisor with no end of its own.
//
// With parts.EndsAtFile the end is written there when the supervisor starts and read from it
// again at least every movedEndPoll, so writing another moment there moves it, later or
// earlier. A write that holds no number is ignored. It needs date in the image as well, and
// without it the lifetime is counted as it is with no file.
func Script(parts Parts, lifetime time.Duration) string {
	script := rule
	if lifetime > 0 {
		seconds := strconv.FormatInt(int64(lifetime/time.Second), 10)
		script += expiry
		if parts.EndsAtFile == "" {
			script += "(sleep " + seconds + " && kill -USR1 $$) >/dev/null 2>&1 &\n"
		} else {
			script += strings.NewReplacer("{seconds}", seconds, "{file}", quoted(parts.EndsAtFile), "{poll}", strconv.Itoa(int(movedEndPoll/time.Second))).Replace(movableEnd)
		}
	}
	return script + parts.Before + "\n" +
		`if [ -z "$terminated" ]; then` + "\n" +
		`"$@" ` + parts.Output + " &\n" +
		"child=$!\n" +
		`[ -z "$terminated" ] || forward` + "\n" +
		parts.Started + "\n" +
		`while kill -0 "$child" 2>/dev/null; do wait "$child"; done` + "\n" +
		"child=\n" +
		"fi\n" +
		parts.After + "\n"
}

// movedEndPoll is the longest the supervisor goes without reading its end again.
const movedEndPoll = time.Minute

// movableEnd counts the lifetime to the end kept in a file. An end that is no number, and a
// clock that is none, are never taken as one: arithmetic on them would end the counting shell.
const movableEnd = `(ends_at=$(date +%s 2>/dev/null)
case "$ends_at" in ''|*[!0-9]*) sleep {seconds} && kill -USR1 $$; exit ;; esac
ends_at=$((ends_at + {seconds}))
echo "$ends_at" > {file}.new && mv -f {file}.new {file}
while :; do
  read -r moved < {file} && case "$moved" in ''|*[!0-9]*) ;; *) ends_at=$moved ;; esac
  now=$(date +%s 2>/dev/null)
  case "$now" in ''|*[!0-9]*) sleep {poll}; continue ;; esac
  [ "$now" -lt "$ends_at" ] || break
  left=$((ends_at - now))
  [ "$left" -le {poll} ] || left={poll}
  sleep "$left"
done
kill -USR1 $$) >/dev/null 2>&1 &
`

// quoted is path as one shell word.
func quoted(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// expiry is what the lifetime's end does. forwarded is set with it: nothing is left to pass a
// later SIGTERM on to.
const expiry = `expired=
expire() {
  expired=1
  terminated=1
  forwarded=1
  [ -z "$child" ] || kill -KILL "$child" 2>/dev/null
}
trap expire USR1
`

const rule = `child=
terminated=
forwarded=
forward() {
  terminated=1
  if [ -n "$child" ] && [ -z "$forwarded" ]; then
    forwarded=1
    kill -TERM "$child" 2>/dev/null
  fi
}
trap forward TERM
`
