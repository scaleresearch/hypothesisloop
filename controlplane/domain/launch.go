package domain

import (
	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
)

// JobCodeDir is where a job's commit is checked out and its command runs.
const JobCodeDir = "/workspace/code"

// codeUnreachable is clusterapi.ExitCodeCodeUnreachable as the script writes it.
const codeUnreachable = "75"

// LaunchScript is what every node of every job runs, on every runtime, with the node's command
// and arguments as its own (clusterapi.LaunchCommand). It needs sh and git in the image. It fetches
// the job's commit through the platform's git proxy with the job's token, sent as a header and
// never in a URL, into JobCodeDir, and runs the command there. An image whose user cannot create
// JobCodeDir, as one that runs as a user other than root, gets the code in workspace/code under
// its home, or under /tmp. Fetching is tried for about three
// minutes; a repository that never answers ends it with clusterapi.ExitCodeCodeUnreachable,
// since the command never ran, and one that answers without the commit with 1. The command's own
// exit with that code ends the script with 1, so only the script reports its code unreachable.
//
// SIGTERM is passed to every process of the script's group, which is everything the command
// started however many shells stand between, and the command keeps jobenv.StopGrace seconds to
// end. Past them the script prints the exit line with 137 and kills the whole group, itself
// included: a shell without a terminal cannot give the command a group of its own. A command that
// ends in time takes the stop grace's timer with it. The last line,
// once the command has ended, is the exit line (clusterapi.FindExitLine).
const LaunchScript = `set -u
started=$(date +%s)
line() { printf '\n` + clusterapi.ExitLinePrefix + ` code=%d started=%d ended=%d\n' "$1" "$started" "$(date +%s)"; }
timer=
finish() {
	[ -z "$timer" ] || kill -s KILL "$timer" 2>/dev/null
	line "$1"
	exit "$1"
}
say() { echo "launch: $*" >&2; }
pid=
interrupted=
stop() {
	trap '' TERM
	[ -n "$pid" ] || finish 143
	interrupted=1
	kill -s TERM 0
	deadline=$(($(date +%s) + $` + jobenv.StopGrace + `))
	(
		while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
		kill -s ALRM $$
	) &
	timer=$!
}
overdue() {
	trap '' ALRM
	say "the command did not end within ${` + jobenv.StopGrace + `}s of SIGTERM, so every process of the job is killed"
	line 137
	kill -s KILL 0
}
trap stop TERM
trap overdue ALRM
fetch() { git -c "http.extraHeader=Authorization: Bearer $` + jobenv.Token + `" "$@"; }
commit=${` + jobenv.CodeRef + `##*@}
for workdir in ` + JobCodeDir + ` "${HOME:-/tmp}/workspace/code" /tmp/workspace/code; do
	mkdir -p "$workdir" 2>/dev/null && git -C "$workdir" init -q 2>/dev/null && break
	workdir=
done
[ -n "$workdir" ] || { say "could not make a git repository at ` + JobCodeDir + `, under the home directory or under /tmp"; finish 1; }
[ "$workdir" = ` + JobCodeDir + ` ] || say "the image's user cannot create ` + JobCodeDir + `, so the code is in $workdir"
try=1
pause=5
while ! said=$(fetch -C "$workdir" fetch -q --depth 1 "$` + jobenv.GitURL + `" "$commit" 2>&1); do
	said=$(printf '%s' "$said" | tr '\n' ' ')
	if [ "$try" -ge 7 ]; then
		if fetch ls-remote -q "$` + jobenv.GitURL + `" HEAD >/dev/null 2>&1; then
			say "commit $commit is not in the repository, which answers: $said"
			finish 1
		fi
		say "the repository did not answer in $try tries, so the command never ran and the platform queues the job again: $said"
		finish ` + codeUnreachable + `
	fi
	say "try $try of 7 to fetch commit $commit failed, the next in ${pause}s: $said"
	try=$((try + 1))
	sleep "$pause"
	pause=$((pause * 2 > 60 ? 60 : pause * 2))
done
git -C "$workdir" checkout -q FETCH_HEAD && cd "$workdir" || finish 1
"$@" &
pid=$!
while :; do
	interrupted=
	wait "$pid"
	code=$?
	[ -n "$interrupted" ] || break
done
if [ "$code" -eq ` + codeUnreachable + ` ]; then
	say "the command exited ` + codeUnreachable + `, the code of a repository that did not answer, so the launch ends with 1"
	code=1
fi
finish "$code"
`
