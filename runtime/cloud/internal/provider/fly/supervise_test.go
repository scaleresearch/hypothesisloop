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
)

// runSupervised runs command the way a Machine does, sh with the supervisor and then the argv,
// and returns what reached the Machine's output and the exit code.
func runSupervised(t *testing.T, env []string, command ...string) (lines []string, code int) {
	t.Helper()
	cmd := startSupervised(t, t.TempDir(), env, command...)
	return finish(t, cmd)
}

func startSupervised(t *testing.T, dir string, env []string, command ...string) *exec.Cmd {
	t.Helper()
	argv := supervised(command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + dir, "DIR=" + dir}, env...)
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func finish(t *testing.T, cmd *exec.Cmd) (lines []string, code int) {
	t.Helper()
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("supervisor: %v", err)
	}
	output := strings.TrimSuffix(cmd.Stdout.(*bytes.Buffer).String(), "\n")
	return strings.Split(output, "\n"), cmd.ProcessState.ExitCode()
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

func TestSupervisorRunsTheArgvUnchangedAndMarksEveryLineOfBothStreams(t *testing.T) {
	t.Parallel()
	argv := []string{"/bin/sh", "-c", `printf 'arg=%s\n' "$@"; echo to-stderr >&2; printf '%s\n' "$VALUE"`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
	lines, code := runSupervised(t, []string{`VALUE=  lead \t tab-literal | pipe`}, argv...)

	want := []string{
		"hl-out|arg=two  spaces",
		`hl-out|arg=quotes ' " $HOME \ kept`,
		"hl-out|arg=",
		"hl-out|arg=--flag=a b",
		"hl-out|to-stderr",
		`hl-out|  lead \t tab-literal | pipe`,
		"hl-ended|0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") || code != 0 {
		t.Errorf("output\n got %q\nwant %q\ncode %d", lines, want, code)
	}
}

func TestSupervisorPassesOnALastLineWithoutANewlineAndTheExitCode(t *testing.T) {
	t.Parallel()
	lines, code := runSupervised(t, nil, "/bin/sh", "-c", `echo first; echo; printf final; exit 3`)

	want := []string{"hl-out|first", "hl-out|", "hl-out|final", "hl-ended|3"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("output = %q, want %q", lines, want)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want the command's 3", code)
	}
}

func TestSupervisorMarksTheEndOfACommandThatCannotBeRun(t *testing.T) {
	t.Parallel()
	lines, code := runSupervised(t, nil, "/no/such/program")

	if len(lines) < 2 || !strings.HasPrefix(lines[0], "hl-out|") || lines[len(lines)-1] != "hl-ended|127" || code != 127 {
		t.Errorf("output = %q with code %d, want the shell's complaint as output and the end mark with 127", lines, code)
	}
}

const savingCommand = `trap 'echo term >> "$DIR/terms"; sleep 1; echo saved; exit 0' TERM; : > "$DIR/ready"; while :; do sleep 0.05 & wait $!; done`

func TestSupervisorPassesTheFirstSIGTERMOnAndLetsTheCommandFinish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := startSupervised(t, dir, nil, "/bin/sh", "-c", savingCommand)
	awaitFile(t, filepath.Join(dir, "ready"))

	for range 3 {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	lines, code := finish(t, cmd)
	if strings.Join(lines, "\n") != "hl-out|saved\nhl-ended|0" || code != 0 {
		t.Errorf("output = %q with code %d, want the save finished and then the end mark", lines, code)
	}
	terms, err := os.ReadFile(filepath.Join(dir, "terms"))
	if err != nil || string(terms) != "term\n" {
		t.Errorf("the command got SIGTERM %d times (%v), want once: Stop is repeated while it saves", strings.Count(string(terms), "term"), err)
	}
}

func TestSupervisorWaitsForOutputWrittenByWhatTheCommandLeftBehind(t *testing.T) {
	t.Parallel()
	lines, _ := runSupervised(t, nil, "/bin/sh", "-c", `(sleep 0.5; echo late) & echo early`)

	if strings.Join(lines, "\n") != "hl-out|early\nhl-out|late\nhl-ended|0" {
		t.Errorf("output = %q, want the end mark after the last line anything wrote", lines)
	}
}

// Without mkfifo there is no way to tell the command's lines from the vendor's, so nothing the
// command wrote could be read back.
func TestSupervisorRefusesToRunTheCommandInAnImageWithoutMkfifo(t *testing.T) {
	dir := t.TempDir()
	argv := supervised([]string{"/bin/sh", "-c", `: > "$DIR/ran"`})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + dir, "TMPDIR=" + dir, "DIR=" + dir}

	output, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("supervisor ended with %v, want exit code 127", err)
	}
	if string(output) != "hl-out|hl-supervisor: the image has no mkfifo, the command was not run\nhl-ended|127\n" {
		t.Errorf("output = %q, want the reason as a line of output and the end mark", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the command ran with no way to read its output")
	}
}

func TestSupervisorRefusesToRunTheCommandWithoutATemporaryDirectory(t *testing.T) {
	dir := t.TempDir()
	argv := supervised([]string{"/bin/sh", "-c", `: > "$DIR/ran"`})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + filepath.Join(dir, "missing"), "DIR=" + dir}

	output, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 126 {
		t.Fatalf("supervisor ended with %v, want exit code 126", err)
	}
	if !strings.Contains(string(output), "hl-out|hl-supervisor: no pipe could be made at ") || !strings.HasSuffix(string(output), "hl-ended|126\n") {
		t.Errorf("output = %q, want the reason as a line of output and the end mark", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the command ran with no way to read its output")
	}
}
