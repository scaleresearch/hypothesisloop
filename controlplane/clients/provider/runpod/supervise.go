package runpod

import (
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/supervise"
)

// supervisor is what a pod runs in place of the bare command, because the vendor starts the
// command again about two seconds after it ends, whatever the exit code, for as long as the pod
// runs, and its stop removes the container and the output with it.
//
// The supervisor is the shared core with this vendor's ends, which reach the API with the key the
// vendor puts in every container, good for this pod alone. It marks the pod's name with upMark
// once the command has started, and beside the command reads the name every few seconds and
// signals itself when it carries stopMark, which the core passes on to the command as it does any
// first SIGTERM. The command's stdout and stderr reach the container's output through a pipe and
// a reader that passes every line on, a last line with no newline included, which the vendor
// would otherwise hold back for as long as the container runs. When the command has ended,
// whenever it chooses to, and the reader with it, the supervisor marks the name with exitMark and
// keeps the container, and so its output, until the pod is destroyed; it marks
// the name again every little while, as a Stop that read the name before can write it without
// the mark. A start that finds the marker of an earlier one (the container was started again)
// marks the pod exited without running the command a second time.
//
// On the global network it waits for the network's interface before it starts the command, and
// marks the pod exited without running the command when the interface does not come up.
//
// It reads the pod's name as the value of a "name" key that starts with the instance name, which
// no other key of the pod has, and holds no quote or backslash.
//
// It needs sh, curl, mkfifo and sleep in the image, and a temporary directory that outlives a
// restart of the container. The command is started without the pod's key in its environment.
func supervisor(name string, networked bool) string {
	parts := supervised
	parts.Before = "self=" + name + "\n" + parts.Before
	if networked {
		parts.Before += "\n" + awaitInterface
	}
	return supervise.Script(parts, 0)
}

var supervised = supervise.Parts{
	Before: `api="` + defaultBaseURL + `/v2/pods/$RUNPOD_POD_ID"
key=$RUNPOD_API_KEY
unset RUNPOD_API_KEY
named() {
  shown=$(curl -s -f -m 20 -H "Authorization: Bearer $key" "$api") || return 1
  for sep in '"name":"' '"name": "'; do
    marks=${shown#*$sep$self}
    [ "$marks" != "$shown" ] || continue
    marks=${marks%%\"*}
    case $marks in *[!a-z.]*) return 1 ;; esac
    name=$self$marks
    return 0
  done
  return 1
}
mark() {
  named || return 1
  base=${name%` + upMark + `}
  base=${base%` + exitMark + `}
  [ "$base.$1" != "$name" ] || return 0
  curl -s -f -m 20 -o /dev/null -X PATCH -H "Authorization: Bearer $key" -H 'Content-Type: application/json' -d "{\"name\":\"$base.$1\"}" "$api"
}
stopped() {
  named || return 1
  case $name in *` + stopMark + `|*` + stopMark + upMark + `|*` + stopMark + exitMark + `) return 0 ;; esac
  return 1
}
finish() {
  sleep ` + deliverySeconds + `
  while :; do
    mark exited
    sleep ` + remarkSeconds + ` & wait $!
  done
}
for tool in curl mkfifo sleep; do
  command -v "$tool" >/dev/null || { echo "hl-supervisor: the image has no $tool, the command was not run" >&2; exit 127; }
done
started="${TMPDIR:-/tmp}/.hl-supervisor-started"
[ -e "$started" ] && finish
: > "$started" || finish
pipe="${TMPDIR:-/tmp}/.hl-supervisor-$$"
mkfifo "$pipe" || { echo "hl-supervisor: no pipe could be made at $pipe, the command was not run" >&2; exit 127; }
while IFS= read -r line || [ -n "$line" ]; do printf '%s\n' "$line"; done < "$pipe" &
reader=$!
exec 3> "$pipe"`,
	Output: `> "$pipe" 2>&1 3>&-`,
	Started: `(
  trap - TERM
  until mark up; do sleep 5; done
  until stopped; do sleep ` + stopPollSeconds + `; done
  kill -TERM $$
) >/dev/null 2>&1 &
watcher=$!`,
	After: `kill -KILL "$watcher" 2>/dev/null
exec 3>&-
(sleep ` + drainSeconds + `; kill -KILL "$reader" 2>/dev/null) >/dev/null 2>&1 &
timer=$!
while kill -0 "$reader" 2>/dev/null; do wait "$reader"; done
kill -KILL "$timer" 2>/dev/null
finish`,
}

const awaitInterface = `waited=0
until [ -d /sys/class/net/` + interfaceName + ` ]; do
  waited=$((waited+1))
  if [ $waited -gt ` + interfaceWaitSeconds + ` ]; then
    echo "hl-supervisor: the private network interface ` + interfaceName + ` did not come up, the command was not run" >&2
    finish
  fi
  sleep 1
done`

const (
	// stopPollSeconds is how often a pod reads its own name, and so the most a Stop waits to
	// reach the command beyond the request itself.
	stopPollSeconds = "5"
	// deliverySeconds is how long the supervisor waits after the last line before it marks the
	// pod exited, as the vendor's log stream serves a line a moment after it is written.
	deliverySeconds = "3"
	// drainSeconds is how long after the command ended the supervisor still passes on output
	// from whatever the command left running with its output open.
	drainSeconds = "5"
	// remarkSeconds is how often a pod whose command ended marks its name again.
	remarkSeconds = "15"
	// interfaceWaitSeconds bounds the wait for the global network's interface, which came up
	// within ten seconds of the container's start.
	interfaceWaitSeconds = "120"
)

// supervisedCommand is the argv that runs command under the supervisor in the instance called
// name, which meets provider.ValidateName and so needs no quoting.
func supervisedCommand(name string, command []string, networked bool) []string {
	return append([]string{"/bin/sh", "-c", supervisor(name, networked), "hl-supervisor"}, command...)
}

var _ provider.ImageNeeder = (*Provider)(nil)

// ImageNeeds are what supervisor runs beside sh, which every image has.
func (p *Provider) ImageNeeds() []string { return []string{"curl", "mkfifo", "sleep"} }
