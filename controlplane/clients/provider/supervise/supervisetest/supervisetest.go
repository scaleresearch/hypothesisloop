// Package supervisetest runs a supervisor's tests under the shells images carry. A supervisor is
// shell text, and /bin/sh is bash on one host, dash in a Debian image and busybox ash in an
// Alpine one.
package supervisetest

import (
	"os/exec"
	"syscall"
	"testing"
)

// shells are tried by name: the host's own sh, then the two the images carry.
var shells = map[string][]string{
	"sh":          {"/bin/sh"},
	"dash":        {"dash"},
	"busybox ash": {"busybox", "ash"},
}

// EachShell runs test once under every shell the host has, as a subtest named after the shell,
// handing it the argv prefix that starts that shell. A shell the host lacks is skipped by name,
// so a run shows which ones a result covers.
func EachShell(t *testing.T, test func(t *testing.T, shell []string)) {
	t.Helper()
	for name, argv := range shells {
		t.Run(name, func(t *testing.T) {
			if _, err := exec.LookPath(argv[0]); err != nil {
				t.Skipf("this host has no %s", argv[0])
			}
			test(t, argv)
		})
	}
}

// Command is the command that runs args under shell.
func Command(shell []string, args ...string) *exec.Cmd {
	argv := append(append([]string{}, shell...), args...)
	return exec.Command(argv[0], argv[1:]...)
}

// Start starts cmd as the leader of its own process group, and at the end of the test kills the
// whole group and reaps cmd, so nothing the script left behind outlives the test, whether it
// passed, failed or timed out. One SIGKILL to the group ends every member at once. Nothing waits
// for the group to be gone: a killed member stays a zombie, and so signallable, until whatever
// adopted it reaps it, which a host's subreaper may never do.
func Start(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
}
