package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

// termOnReady is the command's output, and sends SIGTERM twice once the command has printed
// "ready", which it does after setting its trap.
type termOnReady struct {
	out     bytes.Buffer
	signals chan os.Signal
	sent    bool
}

func (w *termOnReady) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if !w.sent && strings.Contains(w.out.String(), "ready\n") {
		w.sent = true
		w.signals <- syscall.SIGTERM
		w.signals <- syscall.SIGTERM
	}
	return n, err
}

func execResult(t *testing.T, terminate bool, args ...string) (code int, lines []string) {
	t.Helper()
	out := &termOnReady{signals: make(chan os.Signal, 2), sent: !terminate}
	code = runExec(args, out, out, out.signals)
	return code, strings.Split(strings.TrimRight(out.out.String(), "\n"), "\n")
}

func TestExecReportsTheCommandsExitCodeAsItsLastLine(t *testing.T) {
	for _, want := range []int{0, 3} {
		code, lines := execResult(t, false, "--stop-grace", "1", "--", "sh", "-c", fmt.Sprintf("echo working; exit %d", want))
		reported, found := exitline.Find(lines)
		if code != want || !found || reported != want || lines[0] != "working" || lines[len(lines)-1] != exitline.Format(want) {
			t.Errorf("exit %d: code %d, output %q", want, code, lines)
		}
	}
}

func TestExecPassesSIGTERMOnAndLetsTheCommandSave(t *testing.T) {
	code, lines := execResult(t, true, "--stop-grace", "30", "--",
		"sh", "-c", `trap 'echo saved; sleep 0.3; echo saved twice; exit 0' TERM; echo ready; while :; do sleep 0.05; done`)
	if code != 0 || !contains(lines, "saved") || !contains(lines, "saved twice") {
		t.Fatalf("code %d, output %q, want the trap to run and exit 0", code, lines)
	}
}

func TestExecKillsACommandThatOutlivesItsStopGrace(t *testing.T) {
	started := time.Now()
	code, lines := execResult(t, true, "--stop-grace", "1", "--", "sh", "-c", `trap '' TERM; echo ready; exec sleep 30`)
	if code != 128+int(syscall.SIGKILL) || lines[len(lines)-1] != exitline.Format(code) {
		t.Fatalf("code %d, output %q, want the command killed", code, lines)
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("killed after %v, want the 1 s stop grace", elapsed)
	}
}

func TestExecReportsACommandThatCannotStart(t *testing.T) {
	code, lines := execResult(t, false, "--stop-grace", "1", "--", "/nonexistent/command")
	if reported, found := exitline.Find(lines); code != 127 || !found || reported != 127 {
		t.Fatalf("code %d, output %q, want 127 reported", code, lines)
	}
}

func TestExecRefusesToRunWithoutAStopGraceOrACommand(t *testing.T) {
	for _, args := range [][]string{{"--", "true"}, {"--stop-grace", "5"}} {
		code, lines := execResult(t, false, args...)
		if _, found := exitline.Find(lines); code != 2 || found {
			t.Errorf("hl exec %v: code %d, output %q, want a usage error", args, code, lines)
		}
	}
}

func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
