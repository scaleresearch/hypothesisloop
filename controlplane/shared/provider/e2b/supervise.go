package e2b

import (
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider/supervise"
)

// The supervisor's files inside a sandbox. /var/tmp is on the sandbox's disk and any user may
// write to it; /tmp is held in memory. Create makes the directory for its user alone.
const (
	superviseDir = "/var/tmp/.hl-supervisor"
	launchPath   = superviseDir + "/launch"
	outputPath   = superviseDir + "/out"
	statePath    = superviseDir + "/state"

	stateRunning = "running"
	stateExited  = "exited"
	stateExpired = "expired"

	// supervisorTag is the name envd knows the supervisor's process by.
	supervisorTag = "hl-supervisor"

	// imageEnvPath is where a template build leaves the image's own environment, which a template
	// does not carry into its sandboxes.
	imageEnvPath = "/etc/hl-image.env"
)

// launchFile is the shell file envd runs: the shared supervisor core with this vendor's ends. A
// sandbox runs nothing by itself and shows nothing of what runs in it, so the supervisor keeps
// what a caller asks about in files: the command's stdout and stderr, and a state file that gets
// one line before the command starts and a second after it has ended. Lines are only ever
// appended to the state file, so a reader never finds it half written.
//
// A lifetime above zero is the supervisor's to count: at its end it kills the command and the
// second line says expired, where it says exited after any other ending.
//
// The file holds the command's environment and argv, the image's environment first and the
// spec's over it, every value in single quotes, inside which a shell reads each byte as itself.
// It removes itself before anything else: the values are in the supervisor's memory from then on
// and in no file. In a template without the image's environment file the command is not run.
func launchFile(command []string, env map[string]string, lifetime time.Duration) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	var before strings.Builder
	before.WriteString(". " + imageEnvPath + " || exit 126\n")
	for _, name := range names {
		before.WriteString("export " + name + "=" + quoted(env[name]) + "\n")
	}
	before.WriteString("set --")
	for _, arg := range command {
		before.WriteString(" " + quoted(arg))
	}
	before.WriteString("\necho " + stateRunning + " >> " + statePath + " || exit 126")
	after := "echo " + stateExited + " >> " + statePath
	if lifetime > 0 {
		after = `if [ -n "$expired" ]; then echo ` + stateExpired + "; else echo " + stateExited + "; fi >> " + statePath
	}
	return "rm -f " + launchPath + "\n" + supervise.Script(supervise.Parts{
		Before: before.String(),
		Output: "> " + outputPath + " 2>&1",
		After:  after,
	}, lifetime)
}

func quoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// tailScript prints the last lines of the output file, and nothing when the command never
// started and so never made one.
const tailScript = `[ -e "$1" ] || exit 0; exec tail -n "$2" "$1"`
