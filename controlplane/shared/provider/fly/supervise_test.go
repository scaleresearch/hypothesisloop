package fly

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider/supervise/supervisetest"
)

// startSupervised runs command the way a Machine does, the supervisor and then the argv, under
// the given shell.
func startSupervised(t *testing.T, shell []string, dir string, env []string, command ...string) *exec.Cmd {
	t.Helper()
	cmd := supervisetest.Command(shell, supervised(command)[1:]...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + dir, "DIR=" + dir}, env...)
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// finish returns what reached the Machine's output once the supervisor has ended.
func finish(t *testing.T, cmd *exec.Cmd) []string {
	t.Helper()
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("supervisor: %v", err)
	}
	return strings.Split(strings.TrimSuffix(cmd.Stdout.(*bytes.Buffer).String(), "\n"), "\n")
}

func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wantLines holds got against the start mark, which every start writes first, and then want.
func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	want = append([]string{"hl-started"}, want...)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("output\n got %q\nwant %q", got, want)
	}
}

func TestSupervisorRunsTheArgvUnchangedAndMarksEveryLineOfBothStreams(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		argv := []string{"/bin/sh", "-c", `printf 'arg=%s\n' "$@"; echo to-stderr >&2; printf '%s\n' "$VALUE"`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
		cmd := startSupervised(t, shell, t.TempDir(), []string{`VALUE=  lead \t tab-literal | pipe`}, argv...)

		wantLines(t, finish(t, cmd),
			"hl-out|arg=two  spaces",
			`hl-out|arg=quotes ' " $HOME \ kept`,
			"hl-out|arg=",
			"hl-out|arg=--flag=a b",
			"hl-out|to-stderr",
			`hl-out|  lead \t tab-literal | pipe`,
			"hl-ended")
	})
}

func TestSupervisorPassesOnALastLineWithoutANewline(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		cmd := startSupervised(t, shell, t.TempDir(), nil, "/bin/sh", "-c", `echo first; echo; printf final; exit 3`)

		wantLines(t, finish(t, cmd), "hl-out|first", "hl-out|", "hl-out|final", "hl-ended")
	})
}

func TestSupervisorMarksTheEndOfACommandThatCannotBeRun(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		lines := finish(t, startSupervised(t, shell, t.TempDir(), nil, "/no/such/program"))

		if len(lines) < 3 || lines[0] != "hl-started" || !strings.HasPrefix(lines[1], "hl-out|") || lines[len(lines)-1] != "hl-ended" {
			t.Errorf("output = %q, want the start mark, the shell's complaint as output and then the end mark", lines)
		}
	})
}

const savingCommand = `trap 'echo term >> "$DIR/terms"; sleep 1; echo saved; exit 0' TERM; : > "$DIR/ready"; while :; do sleep 0.05 & wait $!; done`

func TestSupervisorPassesTheFirstSIGTERMOnAndLetsTheCommandFinish(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		cmd := startSupervised(t, shell, dir, nil, "/bin/sh", "-c", savingCommand)
		awaitFile(t, filepath.Join(dir, "ready"))

		for range 3 {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		wantLines(t, finish(t, cmd), "hl-out|saved", "hl-ended")
		terms, err := os.ReadFile(filepath.Join(dir, "terms"))
		if err != nil || string(terms) != "term\n" {
			t.Errorf("the command got SIGTERM %d times (%v), want once: Stop is repeated while it saves", strings.Count(string(terms), "term"), err)
		}
	})
}

// The supervisor is the Machine's main process, so a signal that killed it would stop the
// Machine with no end mark. mkfifo runs before the command exists; this one signals the
// supervisor from there.
func TestSupervisorOutlivesASIGTERMThatComesBeforeTheCommandAndStartsNothing(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir, tools := t.TempDir(), t.TempDir()
		real, err := exec.LookPath("mkfifo")
		if err != nil {
			t.Fatal(err)
		}
		signalling := "#!/bin/sh\nkill -TERM $PPID\nexec " + real + ` "$@"` + "\n"
		if err := os.WriteFile(filepath.Join(tools, "mkfifo"), []byte(signalling), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := startSupervised(t, shell, dir, []string{"PATH=" + tools + ":" + os.Getenv("PATH")}, "/bin/sh", "-c", `: > "$DIR/ran"; echo ran`)

		wantLines(t, finish(t, cmd), "hl-ended")
		if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
			t.Error("the command was started after the stop")
		}
	})
}

func TestSupervisorWaitsForOutputWrittenByWhatTheCommandLeftBehind(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		cmd := startSupervised(t, shell, t.TempDir(), nil, "/bin/sh", "-c", `(sleep 0.5; echo late) & echo early`)

		wantLines(t, finish(t, cmd), "hl-out|early", "hl-out|late", "hl-ended")
	})
}

// A daemon the command started holds the command's output open for as long as it lives. The
// Machine must still end: its command is gone, and it is billed while it runs.
func TestSupervisorEndsAFewSecondsAfterTheCommandWhateverStillHoldsItsOutput(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		began := time.Now()
		cmd := startSupervised(t, shell, t.TempDir(), nil, "/bin/sh", "-c", `(sleep 1; echo late; sleep 60; echo never) & echo main-done; exit 3`)

		lines := finish(t, cmd)
		wantLines(t, lines, "hl-out|main-done", "hl-out|late", "hl-ended")
		if took := time.Since(began); took < 4*time.Second || took > 9*time.Second {
			t.Errorf("the supervisor ended %s after the command, want the %s seconds it waits for late output", took.Round(100*time.Millisecond), drainSeconds)
		}
	})
}

// Stop is repeated until the instance shows as exited, so a SIGTERM can arrive after the command
// ended, while its last output is still being passed on. The mark tells a reader that the output
// is complete, so nothing may follow it.
func TestSupervisorWritesTheEndMarkLastWhenSignalledWhileOutputDrains(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		cmd := startSupervised(t, shell, dir, nil, "/bin/sh", "-c", `(sleep 1; echo late) & echo main-done; : > "$DIR/ended"; exit 3`)
		awaitFile(t, filepath.Join(dir, "ended"))

		for range 3 {
			time.Sleep(100 * time.Millisecond)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		wantLines(t, finish(t, cmd), "hl-out|main-done", "hl-out|late", "hl-ended")
	})
}

// Without mkfifo there is no way to tell the command's lines from the vendor's, and without
// sleep no way to bound the wait for late output.
func TestSupervisorRefusesToRunTheCommandInAnImageWithoutItsTools(t *testing.T) {
	for _, missing := range []string{"mkfifo", "sleep"} {
		t.Run(missing, func(t *testing.T) {
			dir, tools := t.TempDir(), t.TempDir()
			for _, present := range []string{"mkfifo", "sleep"} {
				if present != missing {
					if err := os.WriteFile(filepath.Join(tools, present), []byte("#!/bin/sh\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			cmd := exec.Command("/bin/sh", supervised([]string{"/bin/sh", "-c", `: > "$DIR/ran"`})[1:]...)
			cmd.Env = []string{"PATH=" + tools, "TMPDIR=" + dir, "DIR=" + dir}

			output, err := cmd.Output()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 127 {
				t.Fatalf("supervisor ended with %v, want exit code 127", err)
			}
			if string(output) != "hl-started\nhl-out|hl-supervisor: the image has no "+missing+", the command was not run\nhl-ended\n" {
				t.Errorf("output = %q, want the start mark, the reason as a line of output and the end mark", output)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
				t.Error("the command ran with no way to read its output")
			}
		})
	}
}

func TestSupervisorRefusesToRunTheCommandWithoutATemporaryDirectory(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", supervised([]string{"/bin/sh", "-c", `: > "$DIR/ran"`})[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + filepath.Join(dir, "missing"), "DIR=" + dir}

	output, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("supervisor ended with %v, want exit code 127", err)
	}
	if !strings.HasPrefix(string(output), "hl-started\n") || !strings.Contains(string(output), "hl-out|hl-supervisor: no pipe could be made at ") || !strings.HasSuffix(string(output), "hl-ended\n") {
		t.Errorf("output = %q, want the reason as a line of output and the end mark", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the command ran with no way to read its output")
	}
}
