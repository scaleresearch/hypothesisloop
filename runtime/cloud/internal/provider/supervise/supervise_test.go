package supervise_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/supervise"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/supervise/supervisetest"
)

type supervised struct{ cmd *exec.Cmd }

// start runs a supervisor made of parts over command, with $DIR as a place the parts and the
// command write to.
func start(t *testing.T, shell []string, dir string, parts supervise.Parts, command ...string) supervised {
	t.Helper()
	cmd := supervisetest.Command(shell, append([]string{"-c", supervise.Script(parts), "hl-supervisor"}, command...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DIR=" + dir}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return supervised{cmd}
}

func (s supervised) terminate(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

func (s supervised) wait(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end")
	}
}

func await(t *testing.T, path string) {
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

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestScriptRunsTheArgvUnchangedBetweenTheAdaptersParts(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		parts := supervise.Parts{
			Before:  `echo before >> "$DIR/order"`,
			Output:  `> "$DIR/out" 2>&1`,
			Started: `echo "started $child" >> "$DIR/order"`,
			After:   `echo after >> "$DIR/order"`,
		}
		argv := []string{"/bin/sh", "-c", `printf '%s|' "$@"; echo $$ > "$DIR/pid"; echo to-stderr >&2`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
		run := start(t, shell, dir, parts, argv...)
		run.wait(t)

		if got, want := read(t, filepath.Join(dir, "out")), strings.Join(argv[4:], "|")+"|to-stderr\n"; got != want {
			t.Errorf("the command wrote %q, want %q", got, want)
		}
		pid := strings.TrimSpace(read(t, filepath.Join(dir, "pid")))
		if got, want := read(t, filepath.Join(dir, "order")), "before\nstarted "+pid+"\nafter\n"; got != want {
			t.Errorf("parts ran as %q, want %q", got, want)
		}
	})
}

const savingCommand = `trap 'echo term >> "$DIR/terms"; sleep 1; echo saved > "$DIR/saved"; exit 0' TERM; : > "$DIR/ready"; while :; do sleep 0.05 & wait $!; done`

func TestScriptPassesTheFirstSIGTERMOnAndWaitsForTheCommand(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		run := start(t, shell, dir, supervise.Parts{After: `cat "$DIR/saved" > "$DIR/after"`}, "/bin/sh", "-c", savingCommand)
		await(t, filepath.Join(dir, "ready"))

		for range 3 {
			run.terminate(t)
			time.Sleep(200 * time.Millisecond)
		}
		run.wait(t)
		if got := read(t, filepath.Join(dir, "terms")); got != "term\n" {
			t.Errorf("the command got SIGTERM %d times, want once: a stop is repeated while it saves", strings.Count(got, "term"))
		}
		if got := read(t, filepath.Join(dir, "after")); got != "saved\n" {
			t.Errorf("the part after the command ran before the command finished its save: %q", got)
		}
	})
}

// Before the command exists there is nothing to pass a signal on to. A supervisor that set its
// trap only after the start would be killed here, and on a vendor that signals the supervisor
// the instance would end without its own parts running.
func TestScriptDoesNotStartTheCommandAfterASIGTERMThatCameBeforeIt(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		parts := supervise.Parts{Before: `kill -TERM $$`, Started: `: > "$DIR/started"`, After: `: > "$DIR/after"`}
		run := start(t, shell, dir, parts, "/bin/sh", "-c", `: > "$DIR/ran"; sleep 30`)
		run.wait(t)

		if _, err := os.Stat(filepath.Join(dir, "after")); err != nil {
			t.Errorf("the supervisor died of a signal that came before its command: %v", err)
		}
		for _, name := range []string{"ran", "started"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				t.Errorf("%s was written: the command was started after the stop", name)
			}
		}
	})
}
