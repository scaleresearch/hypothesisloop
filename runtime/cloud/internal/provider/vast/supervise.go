package vast

// supervisor is what an instance runs in place of the bare command, because the host starts a
// container again about half a minute after its command ends, whatever the exit code, for as long
// as the instance is meant to run. Left alone a finished job would run again, never show as
// exited, and bill until destroyed.
//
// The supervisor runs the command once, with its argv as positional parameters that no shell
// parses. Every few seconds it reads its own instance's label, with the instance's own key the
// vendor puts in every container, and sends the command SIGTERM once when the label carries the
// stop mark. When the command has ended, whenever it chooses to, the supervisor asks the vendor
// to stop the instance and waits to be stopped: a stopped instance stays exited with its output
// readable and is billed for storage only. A start that finds the marker of an earlier one (the
// host rebooted, the container was restarted) stops the instance without running the command a
// second time.
//
// It needs sh, sleep, grep and curl in the image, and a temporary directory that outlives a
// restart of the container. Without curl it could neither see a stop nor stop the instance, so it
// refuses to run the command.
const supervisor = `instance="` + defaultBaseURL + `/api/v0/instances/$CONTAINER_ID/"
stop() {
  until curl -sS -f -o /dev/null -X PUT -H "Authorization: Bearer $CONTAINER_API_KEY" -H 'Content-Type: application/json' -d '{"state":"stopped"}' "$instance"; do sleep 5; done
  while :; do sleep 3600 & wait $!; done
}
command -v curl >/dev/null || { echo "hl-supervisor: the image has no curl, the command was not run" >&2; exit 127; }
started="${TMPDIR:-/tmp}/.hl-supervisor-started"
[ -e "$started" ] && stop
: > "$started" || stop
"$@" &
child=$!
trap 'kill -TERM "$child" 2>/dev/null' TERM
(
  trap - TERM
  until curl -s -f -m 20 -H "Authorization: Bearer $CONTAINER_API_KEY" "$instance?owner=me" | grep -q '"label": *"[^"]*\` + stopMark + `"'; do sleep ` + stopPollSeconds + `; done
  kill -TERM "$child" 2>/dev/null
) &
watcher=$!
while kill -0 "$child" 2>/dev/null; do wait "$child"; done
kill "$watcher" 2>/dev/null
stop`

// stopPollSeconds is how often an instance reads its own label, and so the most a Stop waits to
// reach the command beyond the request itself.
const stopPollSeconds = "3"

// supervised is the argv that runs command under the supervisor.
func supervised(command []string) (entrypoint string, args []string) {
	return "/bin/sh", append([]string{"-c", supervisor, "hl-supervisor"}, command...)
}
