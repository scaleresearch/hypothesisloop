// Package supervise is the shell core every adapter's supervisor is built around. An instance
// runs the supervisor in place of the bare command, and the adapter adds its vendor's own ends:
// how the command's output is kept, how its end is marked, what happens after it. The core knows
// no vendor.
package supervise

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
func Script(parts Parts) string {
	return rule + parts.Before + "\n" +
		`if [ -z "$terminated" ]; then` + "\n" +
		`"$@" ` + parts.Output + " &\n" +
		"child=$!\n" +
		`[ -z "$terminated" ] || forward` + "\n" +
		parts.Started + "\n" +
		`while kill -0 "$child" 2>/dev/null; do wait "$child"; done` + "\n" +
		"fi\n" +
		parts.After + "\n"
}

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
