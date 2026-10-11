package vast

import (
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/supervise"
)

// supervisor is what an instance runs in place of the bare command, because the host starts a
// container again about half a minute after its command ends, whatever the exit code, for as long
// as the instance is meant to run. Left alone a finished job would run again, never show as
// exited, and bill until destroyed.
//
// The supervisor is the shared core with this vendor's ends. Beside the command it reads its own
// instance's label every few seconds, with the instance's own key the vendor puts in every
// container, and signals itself when the label carries the stop mark, which the core passes on
// to the command as it does any first SIGTERM. When the command has ended, whenever it chooses to, the supervisor asks the vendor
// to stop the instance and waits to be stopped: a stopped instance stays exited with its output
// readable and is billed for storage only. A start that finds the marker of an earlier one (the
// host rebooted, the container was restarted) stops the instance without running the command a
// second time.
//
// It needs sh, sleep and curl in the image, and a temporary directory that outlives a restart of
// the container. Without curl or sleep it could neither see a stop nor stop the instance, so it
// refuses to run the command.
//
// The command is started without the instance's key in its environment. That keeps the key out
// of what a job prints or passes on by accident and nothing more: the supervisor is process 1 of
// the same container under the same user (root, on every instance looked at), and its own
// environment, key included, stays readable at /proc/1/environ.
func supervisor() string {
	return supervise.Script(parts, 0)
}

var parts = supervise.Parts{
	Before: `instance="` + defaultBaseURL + `/api/v0/instances/$CONTAINER_ID/"
key=$CONTAINER_API_KEY
unset CONTAINER_API_KEY
stop() {
  until curl -sS -f -o /dev/null -X PUT -H "Authorization: Bearer $key" -H 'Content-Type: application/json' -d '{"state":"stopped"}' "$instance"; do sleep 5; done
  while :; do sleep 3600 & wait $!; done
}
marked() {
  shown=$(curl -s -f -m 20 -H "Authorization: Bearer $key" "$instance?owner=me") || return 1
  label=${shown#*\"label\":}
  label=${label# }
  case $label in \"*) ;; *) return 1 ;; esac
  label=${label#\"}
  case ${label%%\"*} in *` + stopMark + `) return 0 ;; esac
  return 1
}
for tool in curl sleep; do
  command -v "$tool" >/dev/null || { echo "hl-supervisor: the image has no $tool, the command was not run" >&2; exit 127; }
done
started="${TMPDIR:-/tmp}/.hl-supervisor-started"
[ -e "$started" ] && stop
: > "$started" || stop`,
	Started: `(
  trap - TERM
  until marked; do sleep ` + stopPollSeconds + `; done
  kill -TERM $$
) &
watcher=$!`,
	After: `kill -KILL "$watcher" 2>/dev/null
stop`,
}

// stopPollSeconds is how often an instance reads its own label, and so the most a Stop waits to
// reach the command beyond the request itself.
const stopPollSeconds = "3"

// supervised is the argv that runs command under the supervisor.
func supervised(command []string) (entrypoint string, args []string) {
	return "/bin/sh", append([]string{"-c", supervisor(), "hl-supervisor"}, command...)
}
