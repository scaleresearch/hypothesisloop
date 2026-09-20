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

// fakeCurl stands in for the vendor. A write records its arguments, one per line, so a test reads
// what the supervisor asked for. A read records its own and answers with the instance file, or
// fails while there is none, as an unreachable API does.
const fakeCurl = `#!/bin/sh
case " $* " in
*" PUT "*) printf '%s\n' "$@" >> "$CURL_CALLS" ;;
*) printf '%s\n' "$@" >> "$CURL_READS"; cat "$CURL_INSTANCE" 2>/dev/null || exit 22 ;;
esac
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
		"CURL_READS=" + filepath.Join(dir, "curl-reads"),
		"CURL_INSTANCE=" + filepath.Join(dir, "instance.json"),
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
	t.Parallel()
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
	t.Parallel()
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

const savingCommand = `trap 'echo term >> "$OUT.terms"; sleep 1; echo saved > "$OUT"; exit 0' TERM; echo ready > "$OUT.ready"; while :; do sleep 0.05 & wait $!; done`

func (r *supervisedRun) showInstance(t *testing.T, label string) {
	t.Helper()
	body := `{"instances": {"actual_status": "running", "extra_env": [["NOTE", "\"label\": \"x.stop\""]], "label": "` + label + `", "id": 9001}}`
	if err := os.WriteFile(filepath.Join(r.dir, "instance.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const wantReadCall = `-s
-f
-m
20
-H
Authorization: Bearer instance-key
https://console.vast.ai/api/v0/instances/9001/?owner=me
`

func TestSupervisorSendsSIGTERMOnceWhenItsLabelIsMarkedAndLetsTheCommandFinish(t *testing.T) {
	t.Parallel()
	run := startSupervised(t, t.TempDir(), "/bin/sh", "-c", savingCommand)
	run.showInstance(t, "hl-exp1-a2")
	run.awaitFile(t, "out.ready")
	if reads := run.awaitFile(t, "curl-reads"); !strings.HasPrefix(reads, wantReadCall) {
		t.Errorf("label read\n got %q\nwant %q", reads, wantReadCall)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(run.dir, "out.terms")); err == nil {
		t.Fatal("the command was signalled while its label carried no mark")
	}

	run.showInstance(t, "hl-exp1-a2.stop")
	run.awaitFile(t, "curl-calls")
	if saved, err := os.ReadFile(filepath.Join(run.dir, "out")); err != nil || string(saved) != "saved\n" {
		t.Errorf("the instance was stopped before the command finished its save: %q, %v", saved, err)
	}
	time.Sleep(4 * time.Second)
	if terms := run.awaitFile(t, "out.terms"); terms != "term\n" {
		t.Errorf("the command got SIGTERM %d times, want once", strings.Count(terms, "term"))
	}
}

func TestSupervisorKeepsTheCommandRunningWhileItsLabelCannotBeRead(t *testing.T) {
	t.Parallel()
	run := startSupervised(t, t.TempDir(), "/bin/sh", "-c", savingCommand)
	run.awaitFile(t, "out.ready")
	run.awaitFile(t, "curl-reads")
	time.Sleep(4 * time.Second)

	if _, err := os.Stat(filepath.Join(run.dir, "out.terms")); err == nil {
		t.Error("the command was signalled because a read failed")
	}
	if strings.Count(run.awaitFile(t, "curl-reads"), "owner=me") < 2 {
		t.Error("the supervisor gave up reading its label after a failed read")
	}
}

func TestSupervisorPassesOnASIGTERMOfItsOwn(t *testing.T) {
	t.Parallel()
	run := startSupervised(t, t.TempDir(), "/bin/sh", "-c", savingCommand)
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
