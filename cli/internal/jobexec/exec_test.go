package jobexec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/exitline"
)

// execResult runs hl exec with its output in a file, which hands the command the file itself as
// a container hands it the instance's log. With terminate, SIGTERM is sent twice once the command
// has printed "ready", which a test's script does after setting its trap.
func execResult(t *testing.T, terminate bool, args ...string) (code int, lines []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "output")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	read := func() string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		}
		return string(raw)
	}

	signals := make(chan os.Signal, 2)
	stop := make(chan struct{})
	defer close(stop)
	if terminate {
		go func() {
			for !strings.Contains(read(), "ready\n") {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
			signals <- syscall.SIGTERM
			signals <- syscall.SIGTERM
		}()
	}
	code = runExec(args, out, out, signals)
	return code, strings.Split(strings.TrimRight(read(), "\n"), "\n")
}

func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func last(lines []string) string { return lines[len(lines)-1] }

func TestExecReportsTheCommandsExitCodeAsItsLastLine(t *testing.T) {
	for _, want := range []int{0, 3} {
		code, lines := execResult(t, false, "--stop-grace", "1", "--", "sh", "-c", fmt.Sprintf("echo working; exit %d", want))
		reported, found := exitline.Find(lines)
		if code != want || !found || reported != want || lines[0] != "working" || last(lines) != exitline.Format(want) {
			t.Errorf("exit %d: code %d, output %q", want, code, lines)
		}
	}
}

func TestExecReportsWhenTheCommandRanJustBeforeItsExitLine(t *testing.T) {
	before := time.Now().Truncate(time.Millisecond)
	code, lines := execResult(t, false, "--stop-grace", "1", "--", "sh", "-c", "sleep 0.3; printf done")
	after := time.Now()
	startedAt, endedAt, found := exitline.FindRun(lines)
	if code != 0 || !found || len(lines) < 3 || lines[len(lines)-2] != exitline.FormatRun(startedAt, endedAt) {
		t.Fatalf("code %d, output %q, want the run line before the exit line", code, lines)
	}
	if startedAt.Before(before) || endedAt.After(after) || endedAt.Sub(startedAt) < 300*time.Millisecond {
		t.Fatalf("ran %v to %v, want the 0.3 s of the command between %v and %v", startedAt, endedAt, before, after)
	}
}

func TestExecReportsNoRunOfACommandThatNeverStarted(t *testing.T) {
	code, lines := execResult(t, false, "--stop-grace", "1", "--", "/no/such/command")
	if _, _, found := exitline.FindRun(lines); code != 127 || found {
		t.Fatalf("code %d, output %q, want exit 127 and no run line", code, lines)
	}
}

// A progress bar, or any last print without a newline, must not swallow the exit line.
func TestExecReportsAfterOutputThatDoesNotEndInANewline(t *testing.T) {
	code, lines := execResult(t, false, "--stop-grace", "1", "--", "printf", "done 100%%")
	reported, found := exitline.Find(lines)
	if code != 0 || !found || reported != 0 || lines[0] != "done 100%" || last(lines) != exitline.Format(0) {
		t.Fatalf("code %d, output %q", code, lines)
	}
}

// The command counts the SIGTERMs it gets. The runtime stops an instance on every pass, and a
// second signal reaching a job in the middle of its save is what this wrapper is there to absorb.
func TestExecPassesSIGTERMOnOnceAndLetsTheCommandSave(t *testing.T) {
	code, lines := execResult(t, true, "--stop-grace", "30", "--", "sh", "-c",
		`n=0; trap 'n=$((n+1))' TERM; echo ready; while [ $n -eq 0 ]; do sleep 0.05; done; sleep 0.5; echo "saved after $n SIGTERM"; exit 0`)
	if code != 0 || !contains(lines, "saved after 1 SIGTERM") || last(lines) != exitline.Format(0) {
		t.Fatalf("code %d, output %q, want one SIGTERM, the save, and exit 0", code, lines)
	}
}

// `sh -c 'a && b'` leaves a shell between the wrapper and the process that saves. The shell dies
// on SIGTERM; the process behind it must still get the signal and its time, and the exit line
// must come after everything it wrote.
func TestExecSignalsTheProcessBehindAShellAndWaitsForItsSave(t *testing.T) {
	inner := `trap 'sleep 0.3; echo inner saved; exit 0' TERM; echo ready; while :; do sleep 0.05; done`
	code, lines := execResult(t, true, "--stop-grace", "30", "--", "sh", "-c", `sh -c "$0" && echo unreachable`, inner)
	reported, found := exitline.Find(lines)
	if !found || reported != code || !contains(lines, "inner saved") || contains(lines, "unreachable") {
		t.Fatalf("code %d, output %q, want the inner process to save", code, lines)
	}
	if last(lines) != exitline.Format(code) {
		t.Fatalf("output %q, want the exit line after the save", lines)
	}
}

func TestExecKillsWhatOutlivesTheStopGrace(t *testing.T) {
	for name, script := range map[string]string{
		"the command itself":         `trap '' TERM; echo ready; exec sleep 30`,
		"a process behind the shell": `sh -c 'trap "" TERM; echo ready; exec sleep 30' && echo unreachable`,
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			code, lines := execResult(t, true, "--stop-grace", "1", "--", "sh", "-c", script)
			if reported, found := exitline.Find(lines); !found || reported != code || code == 0 || last(lines) != exitline.Format(code) {
				t.Fatalf("code %d, output %q, want a killed command reported", code, lines)
			}
			if elapsed := time.Since(started); elapsed < time.Second || elapsed > 10*time.Second {
				t.Fatalf("ended after %v, want the 1 s stop grace", elapsed)
			}
		})
	}
}

// What the command left in the background does not outlive it or write after the exit line.
func TestExecKillsWhatTheCommandLeftBehind(t *testing.T) {
	started := time.Now()
	code, lines := execResult(t, false, "--stop-grace", "30", "--", "sh", "-c", `(sleep 30; echo late) & echo done`)
	if code != 0 || last(lines) != exitline.Format(0) || contains(lines, "late") || time.Since(started) > 10*time.Second {
		t.Fatalf("code %d, output %q after %v", code, lines, time.Since(started))
	}
}

// A stop can arrive while the instance is still starting the command. It is delivered once the
// command exists instead of being lost.
func TestExecDeliversASIGTERMThatArrivedBeforeTheCommandStarted(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	out, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	started := time.Now()
	code := runExec([]string{"--stop-grace", "30", "--", "sleep", "30"}, out, out, signals)
	if code != 128+int(syscall.SIGTERM) || time.Since(started) > 10*time.Second {
		t.Fatalf("code %d after %v, want the command ended by the early SIGTERM", code, time.Since(started))
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

// bounded runs the wrapper and fails the test when it has not returned in time, which is how a
// wrapper that waits for ever shows.
func bounded(t *testing.T, limit time.Duration, args ...string) (code int, lines []string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, lines = execResult(t, false, args...)
	}()
	select {
	case <-done:
		return code, lines
	case <-time.After(limit):
		t.Fatalf("hl exec had not written its exit line after %v", limit)
		return 0, nil
	}
}

// The command's exit status is the job's result. A member of its group that never goes away,
// which is what a process looks like that has died and that nobody collects, cannot hold the
// exit line back: the group is killed, given leftoverWait, and the line is written.
func TestExecDoesNotWaitForeverForAGroupThatNeverEmpties(t *testing.T) {
	defer func(wait time.Duration, gone func(int) bool) { leftoverWait, groupGone = wait, gone }(leftoverWait, groupGone)
	leftoverWait = 300 * time.Millisecond
	groupGone = func(int) bool { return false }

	started := time.Now()
	code, lines := bounded(t, 10*time.Second, "--stop-grace", "30", "--", "sh", "-c", "echo working; exit 7")
	if reported, found := exitline.Find(lines); code != 7 || !found || reported != 7 || last(lines) != exitline.Format(7) {
		t.Fatalf("code %d, output %q, want the command's own exit code", code, lines)
	}
	if elapsed := time.Since(started); elapsed < leftoverWait {
		t.Fatalf("returned after %v, before the group had its %v", elapsed, leftoverWait)
	}
}

// The same when the group is killed at the end of a stop grace instead of at the command's exit.
func TestExecDoesNotWaitForeverAfterTheStopGraceEither(t *testing.T) {
	defer func(wait time.Duration, gone func(int) bool) { leftoverWait, groupGone = wait, gone }(leftoverWait, groupGone)
	leftoverWait = 300 * time.Millisecond
	groupGone = func(int) bool { return false }

	for name, script := range map[string]string{
		"the command outlives the grace":    `trap '' TERM; echo ready; exec sleep 30`,
		"the command ends inside the grace": `trap 'exit 0' TERM; echo ready; while :; do sleep 0.05; done`,
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan []string, 1)
			go func() {
				_, lines := execResult(t, true, "--stop-grace", "1", "--", "sh", "-c", script)
				done <- lines
			}()
			select {
			case lines := <-done:
				if _, found := exitline.Find(lines); !found {
					t.Fatalf("output %q", lines)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("hl exec had not written its exit line 14 s after a 1 s stop grace")
			}
		})
	}
}

// With real processes, and the wrapper not the first process of a container: the command leaves
// in its group a process that has exited and whose parent, alive in another group, never
// collects it. The wrapper cannot collect it either, since it is not its child and no orphan.
// Where such a process counts as a member of its group (Linux), the group never empties.
func TestExecReportsWhenAnUncollectedProcessHoldsTheGroup(t *testing.T) {
	defer func(wait time.Duration) { leftoverWait = wait }(leftoverWait)
	leftoverWait = 500 * time.Millisecond

	t.Setenv("HL_EXEC_HELPER", "leader")
	code, lines := bounded(t, 20*time.Second, "--stop-grace", "30", "--", os.Args[0], "-test.run=TestExecHelperProcess")
	for _, line := range lines {
		var holder int
		if _, err := fmt.Sscanf(line, "holder %d", &holder); err == nil {
			defer syscall.Kill(holder, syscall.SIGKILL) //nolint:errcheck
		}
	}
	if reported, found := exitline.Find(lines); code != 7 || !found || reported != 7 {
		t.Fatalf("code %d, output %q, want the leader's exit code 7", code, lines)
	}
}

// TestExecHelperProcess is the processes of the test above, and nothing when run as a test.
//
// The leader is the wrapper's command. It starts the holder in a group of its own, which the
// wrapper's kill of the leader's group does not reach, and exits 7 once the holder says it is
// ready. The holder starts a process into the leader's group that exits at once, and then sleeps
// without ever waiting for it.
func TestExecHelperProcess(t *testing.T) {
	switch os.Getenv("HL_EXEC_HELPER") {
	case "leader":
		holder := exec.Command(os.Args[0], "-test.run=TestExecHelperProcess")
		holder.Env = append(os.Environ(), "HL_EXEC_HELPER=holder", fmt.Sprintf("HL_EXEC_GROUP=%d", os.Getpid()))
		holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		ready, err := holder.StdoutPipe()
		if err != nil || holder.Start() != nil {
			os.Exit(90)
		}
		buf := make([]byte, 16)
		if n, _ := ready.Read(buf); !strings.HasPrefix(string(buf[:n]), "ready") {
			os.Exit(91)
		}
		fmt.Printf("holder %d\n", holder.Process.Pid)
		os.Exit(7)
	case "holder":
		var group int
		if _, err := fmt.Sscanf(os.Getenv("HL_EXEC_GROUP"), "%d", &group); err != nil {
			os.Exit(92)
		}
		uncollected := exec.Command("/bin/sh", "-c", "exit 0")
		uncollected.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
		if err := uncollected.Start(); err != nil {
			os.Exit(93)
		}
		time.Sleep(300 * time.Millisecond)
		fmt.Println("ready")
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
}
