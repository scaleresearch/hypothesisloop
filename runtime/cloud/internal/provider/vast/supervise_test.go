package vast

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCurl records each call's arguments, one per line, so a test reads what the supervisor asked
// of the vendor.
const fakeCurl = `#!/bin/sh
printf '%s\n' "$@" >> "$CURL_CALLS"
`

type supervisedRun struct {
	dir string
	cmd *exec.Cmd
}

// startSupervised runs command the way an instance does: sh, the supervisor, then the argv.
func startSupervised(t *testing.T, dir string, command ...string) *supervisedRun {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint, args := supervised(command)
	cmd := exec.Command(entrypoint, args...)
	cmd.Env = []string{
		"PATH=" + dir + ":" + os.Getenv("PATH"),
		"TMPDIR=" + dir,
		"OUT=" + filepath.Join(dir, "out"),
		"CURL_CALLS=" + filepath.Join(dir, "curl-calls"),
		"CONTAINER_API_KEY=instance-key",
		"CONTAINER_ID=9001",
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &supervisedRun{dir: dir, cmd: cmd}
}

func (r *supervisedRun) awaitFile(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(r.dir, name))
		if err == nil && len(raw) > 0 {
			return string(raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not written", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const wantStopCall = `-sS
-f
-o
/dev/null
-X
PUT
-H
Authorization: Bearer instance-key
-H
Content-Type: application/json
-d
{"state":"stopped"}
https://console.vast.ai/api/v0/instances/9001/
`

func TestSupervisorRunsTheArgvUnchangedThenStopsTheInstanceAndWaits(t *testing.T) {
	argv := []string{"/bin/sh", "-c", `printf '%s\n' "$@" > "$OUT"`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
	run := startSupervised(t, t.TempDir(), argv...)

	if calls := run.awaitFile(t, "curl-calls"); calls != wantStopCall {
		t.Errorf("stop request\n got %q\nwant %q", calls, wantStopCall)
	}
	if got, want := run.awaitFile(t, "out"), strings.Join(argv[4:], "\n")+"\n"; got != want {
		t.Errorf("the command saw arguments %q, want %q", got, want)
	}
	time.Sleep(200 * time.Millisecond)
	if err := run.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the supervisor exited after asking for the stop, which lets the host start the container again: %v", err)
	}
}

func TestSupervisorDoesNotRunTheCommandOnASecondStart(t *testing.T) {
	dir := t.TempDir()
	first := startSupervised(t, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
	first.awaitFile(t, "curl-calls")
	_ = first.cmd.Process.Kill()
	_ = first.cmd.Wait()
	if err := os.Remove(filepath.Join(dir, "curl-calls")); err != nil {
		t.Fatal(err)
	}

	second := startSupervised(t, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
	if calls := second.awaitFile(t, "curl-calls"); calls != wantStopCall {
		t.Errorf("the restarted supervisor did not ask for the stop: %q", calls)
	}
	if got := second.awaitFile(t, "out"); got != "run\n" {
		t.Errorf("the command ran %d times, want once", strings.Count(got, "run"))
	}
}

func TestSupervisorPassesSIGTERMToTheCommandAndStopsOnlyAfterItEnded(t *testing.T) {
	run := startSupervised(t, t.TempDir(), "/bin/sh", "-c",
		`trap 'sleep 1; echo saved > "$OUT"; exit 0' TERM; echo ready > "$OUT.ready"; while :; do sleep 0.05 & wait $!; done`)
	run.awaitFile(t, "out.ready")

	if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	run.awaitFile(t, "curl-calls")
	if saved, err := os.ReadFile(filepath.Join(run.dir, "out")); err != nil || string(saved) != "saved\n" {
		t.Errorf("the stop was asked for before the command finished its save: %q, %v", saved, err)
	}
}

func TestSupervisorRefusesToRunTheCommandInAnImageWithoutCurl(t *testing.T) {
	dir := t.TempDir()
	entrypoint, args := supervised([]string{"/bin/sh", "-c", `echo run > "$OUT"`})
	cmd := exec.Command(entrypoint, args...)
	cmd.Env = []string{"PATH=" + dir, "TMPDIR=" + dir, "OUT=" + filepath.Join(dir, "out")}

	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("supervisor ended with %v, want exit code 127", err)
	}
	if !strings.Contains(string(output), "the image has no curl") {
		t.Errorf("output = %q, want the reason", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Error("the command ran in an image the supervisor cannot stop")
	}
}
