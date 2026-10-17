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

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/supervise/supervisetest"
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

// startSupervised runs command the way an instance does, the supervisor and then the argv, under
// the given shell.
func startSupervised(t *testing.T, shell []string, dir string, command ...string) *supervisedRun {
	t.Helper()
	_, args := supervised(command)
	return startScript(t, shell, dir, args)
}

// startScript runs the supervisor's arguments, which a test may have changed.
func startScript(t *testing.T, shell []string, dir string, args []string) *supervisedRun {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := supervisetest.Command(shell, args...)
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
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		argv := []string{"/bin/sh", "-c", `printf '%s\n' "$@" > "$OUT"`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
		run := startSupervised(t, shell, t.TempDir(), argv...)

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
	})
}

func TestSupervisorDoesNotRunTheCommandOnASecondStart(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		dir := t.TempDir()
		first := startSupervised(t, shell, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
		first.awaitFile(t, "curl-calls")
		_ = first.cmd.Process.Kill()
		_ = first.cmd.Wait()
		if err := os.Remove(filepath.Join(dir, "curl-calls")); err != nil {
			t.Fatal(err)
		}

		second := startSupervised(t, shell, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
		if calls := second.awaitFile(t, "curl-calls"); calls != wantStopCall {
			t.Errorf("the restarted supervisor did not ask for the stop: %q", calls)
		}
		if got := second.awaitFile(t, "out"); got != "run\n" {
			t.Errorf("the command ran %d times, want once", strings.Count(got, "run"))
		}
	})
}

// The marker file is the last thing the supervisor does before it starts the command. A signal
// there finds no command to pass it to, and must neither kill the supervisor, which is what stops
// the instance, nor be followed by the command starting.
func TestSupervisorStopsTheInstanceAfterASIGTERMThatCameBeforeTheCommand(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		_, args := supervised([]string{"/bin/sh", "-c", `echo run >> "$OUT"`})
		const marker = `: > "$started" || stop`
		if !strings.Contains(args[1], marker) {
			t.Fatalf("the supervisor no longer holds %q", marker)
		}
		args[1] = strings.Replace(args[1], marker, marker+"\nkill -TERM $$", 1)
		run := startScript(t, shell, t.TempDir(), args)

		if calls := run.awaitFile(t, "curl-calls"); calls != wantStopCall {
			t.Errorf("stop request\n got %q\nwant %q", calls, wantStopCall)
		}
		if _, err := os.Stat(filepath.Join(run.dir, "out")); err == nil {
			t.Error("the command was started after the stop")
		}
	})
}

const savingCommand = `trap 'echo term >> "$OUT.terms"; sleep 1; echo saved > "$OUT"; exit 0' TERM; echo ready > "$OUT.ready"; while :; do sleep 0.05 & wait $!; done`

func (r *supervisedRun) showInstance(t *testing.T, label string) {
	t.Helper()
	body := `{"instances": {"actual_status": "running", "extra_env": [["NOTE", "\"label\": \"x.stop\""]], "label": "` + label + `", "status_msg": "pulled x.stop", "template_label": "t.stop"}}`
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
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", savingCommand)
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
	})
}

func TestSupervisorKeepsTheCommandRunningWhileItsLabelCannotBeRead(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", savingCommand)
		run.awaitFile(t, "out.ready")
		run.awaitFile(t, "curl-reads")
		time.Sleep(4 * time.Second)

		if _, err := os.Stat(filepath.Join(run.dir, "out.terms")); err == nil {
			t.Error("the command was signalled because a read failed")
		}
		if strings.Count(run.awaitFile(t, "curl-reads"), "owner=me") < 2 {
			t.Error("the supervisor gave up reading its label after a failed read")
		}
	})
}

func TestSupervisorPassesOnASIGTERMOfItsOwn(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", savingCommand)
		run.awaitFile(t, "out.ready")

		if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		run.awaitFile(t, "curl-calls")
		if saved, err := os.ReadFile(filepath.Join(run.dir, "out")); err != nil || string(saved) != "saved\n" {
			t.Errorf("the stop was asked for before the command finished its save: %q, %v", saved, err)
		}
	})
}

func TestSupervisorReadsTheMarkFromALabelWrittenWithoutSpaces(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", savingCommand)
		run.awaitFile(t, "out.ready")
		compact := `{"instances":{"extra_env":[],"label":"hl-exp1-a2.stop","status_msg":null}}`
		if err := os.WriteFile(filepath.Join(run.dir, "instance.json"), []byte(compact), 0o644); err != nil {
			t.Fatal(err)
		}

		run.awaitFile(t, "curl-calls")
		if terms := run.awaitFile(t, "out.terms"); terms != "term\n" {
			t.Errorf("the command got SIGTERM %d times, want once", strings.Count(terms, "term"))
		}
	})
}

func TestSupervisorStartsTheCommandWithoutTheInstanceKey(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", `env > "$OUT"`)

		if calls := run.awaitFile(t, "curl-calls"); !strings.Contains(calls, "Authorization: Bearer instance-key\n") {
			t.Errorf("the supervisor lost the key it stops the instance with: %q", calls)
		}
		seen := run.awaitFile(t, "out")
		if strings.Contains(seen, "CONTAINER_API_KEY") || strings.Contains(seen, "instance-key") {
			t.Errorf("the command's environment carries the instance key:\n%s", seen)
		}
		if !strings.Contains(seen, "CONTAINER_ID=9001") {
			t.Errorf("the rest of the environment did not reach the command:\n%s", seen)
		}
	})
}

// Without sleep the supervisor could not pace its reads, and without curl it could neither see a
// stop nor stop the instance, so a job would run with no way to end it but destroying it.
func TestSupervisorRefusesToRunTheCommandInAnImageWithoutItsTools(t *testing.T) {
	for _, missing := range []string{"curl", "sleep"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, present := range []string{"curl", "sleep"} {
				if present == missing {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, present), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			entrypoint, args := supervised([]string{"/bin/sh", "-c", `echo run > "$OUT"`})
			cmd := exec.Command(entrypoint, args...)
			cmd.Env = []string{"PATH=" + dir, "TMPDIR=" + dir, "OUT=" + filepath.Join(dir, "out")}

			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 127 {
				t.Fatalf("supervisor ended with %v, want exit code 127", err)
			}
			if !strings.Contains(string(output), "the image has no "+missing) {
				t.Errorf("output = %q, want the reason", output)
			}
			if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
				t.Error("the command ran in an image the supervisor cannot stop")
			}
		})
	}
}
