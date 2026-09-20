package e2b

import (
	"sort"
	"strings"
)

// The supervisor's files inside a sandbox. /var/tmp is on the sandbox's disk and any user may
// write to it; /tmp is held in memory.
const (
	superviseDir = "/var/tmp/.hl-supervisor"
	launchPath   = superviseDir + "/launch"
	outputPath   = superviseDir + "/out"
	statePath    = superviseDir + "/state"

	stateRunning = "running"
	stateExited  = "exited"

	// supervisorTag is the name envd knows the supervisor's process by.
	supervisorTag = "hl-supervisor"

	// imageEnvPath is where templateScript leaves the image's own environment, which a template
	// does not carry into its sandboxes.
	imageEnvPath = "/etc/hl-image.env"
)

// supervisor is what envd runs, after the lines of the launch file that set the command's
// environment and argv. A sandbox runs nothing by itself and shows nothing of what runs in it, so
// the supervisor keeps what a caller asks about in files: the command's stdout and stderr, and a
// state file that says running before the command starts and exited after it has ended. Lines
// are only ever appended to the state file, so a reader never finds it half written.
//
// The first SIGTERM goes on to the command; later ones are ignored, because Stop is repeated
// while the command saves and a second signal would start its handler again.
const supervisor = `echo ` + stateRunning + ` >> ` + statePath + ` || exit 126
"$@" > ` + outputPath + ` 2>&1 &
child=$!
trap 'trap "" TERM; kill -TERM "$child" 2>/dev/null' TERM
while kill -0 "$child" 2>/dev/null; do wait "$child"; code=$?; done
echo "` + stateExited + ` $code" >> ` + statePath + `
exit "$code"
`

// launchFile is the shell file that runs command with env under the supervisor: the image's
// environment first, then the spec's over it. Every value is written in single quotes, inside
// which a shell reads each byte as itself, so the command sees its environment and its arguments
// unchanged. In a template without the image's environment file the shell stops at the first
// line and the command is not run.
func launchFile(command []string, env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	var file strings.Builder
	file.WriteString(". " + imageEnvPath + "\n")
	for _, name := range names {
		file.WriteString("export " + name + "=" + quoted(env[name]) + "\n")
	}
	file.WriteString("set --")
	for _, arg := range command {
		file.WriteString(" " + quoted(arg))
	}
	file.WriteString("\n" + supervisor)
	return file.String()
}

func quoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// tailScript prints the last lines of the output file, and nothing when the command never
// started and so never made one.
const tailScript = `[ -e "$1" ] || exit 0; exec tail -n "$2" "$1"`
