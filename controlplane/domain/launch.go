package domain

import (
	"strconv"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
)

// JobCodeDir is where a job's commit is checked out and its command runs.
const JobCodeDir = "/workspace/code"

// LaunchScript is what every node of every job runs, on every runtime, with the node's command
// and arguments as its own (clusterapi.LaunchCommand). It needs sh and git in the image. It fetches
// the job's commit through the platform's git proxy with the job's token, sent as a header and
// never in a URL, into JobCodeDir, and runs the command there. Fetching is tried for about three
// minutes; a repository that never answers ends it with clusterapi.ExitCodeCodeUnreachable,
// since the command never ran, and one that answers without the commit with 1.
//
// SIGTERM is passed to every process of the script's group, which is everything the command
// started however many shells stand between, and the command keeps jobenv.StopGrace seconds
// before it is killed. The last line, once the command has ended, is the exit line
// (clusterapi.FindExitLine).
var LaunchScript = `set -u
started=$(date +%s)
finish() {
	printf '\n` + clusterapi.ExitLinePrefix + ` code=%d started=%d ended=%d\n' "$1" "$started" "$(date +%s)"
	exit "$1"
}
say() { echo "launch: $*" >&2; }
pid=
stop() {
	trap '' TERM
	[ -n "$pid" ] || finish 143
	kill -s TERM 0
	(sleep "$` + jobenv.StopGrace + `"; kill -s KILL "$pid" 2>/dev/null) &
}
trap stop TERM
fetch() { git -c "http.extraHeader=Authorization: Bearer $` + jobenv.Token + `" "$@"; }
commit=${` + jobenv.CodeRef + `##*@}
mkdir -p ` + JobCodeDir + ` && git -C ` + JobCodeDir + ` init -q || { say "could not make a git repository at ` + JobCodeDir + `: the image's user must be able to create it"; finish 1; }
try=1
pause=5
while ! said=$(fetch -C ` + JobCodeDir + ` fetch -q --depth 1 "$` + jobenv.GitURL + `" "$commit" 2>&1); do
	said=$(printf '%s' "$said" | tr '\n' ' ')
	if [ "$try" -ge 7 ]; then
		if fetch ls-remote -q "$` + jobenv.GitURL + `" HEAD >/dev/null 2>&1; then
			say "commit $commit is not in the repository, which answers: $said"
			finish 1
		fi
		say "the repository did not answer in $try tries, so the command never ran and the platform queues the job again: $said"
		finish ` + strconv.Itoa(clusterapi.ExitCodeCodeUnreachable) + `
	fi
	say "try $try of 7 to fetch commit $commit failed, the next in ${pause}s: $said"
	try=$((try + 1))
	sleep "$pause"
	pause=$((pause * 2 > 60 ? 60 : pause * 2))
done
git -C ` + JobCodeDir + ` checkout -q FETCH_HEAD && cd ` + JobCodeDir + ` || finish 1
"$@" &
pid=$!
while :; do
	wait "$pid"
	code=$?
	kill -0 "$pid" 2>/dev/null || break
done
finish "$code"
`
