package runpod

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/supervise/supervisetest"
)

// fakeCurl stands in for the API with the pod's name in a file. A rename records its arguments,
// one per line, and writes the name; a read answers with the pod, its name after a decoy that
// only an escaped quote tells apart and an env key that only the instance name does, or fails
// while there is no name, as an unreachable API does.
const fakeCurl = `#!/bin/sh
record="$TMPDIR/record.$$"
printf '%s\n' "$@" > "$record"
case " $* " in
*" PATCH "*)
  cat "$record" >> "$CURL_CALLS"
  while [ $# -gt 0 ]; do [ "$1" = -d ] && body=$2; shift; done
  name=${body#*\"name\":\"}
  printf '%s' "${name%%\"*}" > "$POD_NAME.tmp" && mv "$POD_NAME.tmp" "$POD_NAME" ;;
*)
  rm "$record"
  [ -s "$POD_NAME" ] || exit 22
  printf '{"args":"{\\"cmd\\":[\\"\\"name\\":\\"decoy.stop\\"\\"]}","env":{"name":"decoy.stop"},"id":"pod1","name":"%s","status":"RUNNING"}' "$(cat "$POD_NAME")" ;;
esac
rm -f "$record"
`

type supervisedRun struct {
	dir string
	cmd *exec.Cmd
}

func startSupervised(t *testing.T, shell []string, dir string, command ...string) *supervisedRun {
	t.Helper()
	return startScript(t, shell, dir, supervisedCommand("hl-job-a", command, false)[1:])
}

func startScript(t *testing.T, shell []string, dir string, args []string) *supervisedRun {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	run := &supervisedRun{dir: dir}
	if _, err := os.Stat(filepath.Join(dir, "name")); err != nil {
		run.name(t, "hl-job-a")
	}
	cmd := supervisetest.Command(shell, args...)
	cmd.Env = []string{
		"PATH=" + dir + ":" + os.Getenv("PATH"),
		"TMPDIR=" + dir,
		"OUT=" + filepath.Join(dir, "out"),
		"CURL_CALLS=" + filepath.Join(dir, "curl-calls"),
		"POD_NAME=" + filepath.Join(dir, "name"),
		"RUNPOD_API_KEY=pod-key",
		"RUNPOD_POD_ID=pod1",
	}
	output, err := os.Create(filepath.Join(dir, "container-output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	cmd.Stdout, cmd.Stderr = output, output
	supervisetest.Start(t, cmd)
	run.cmd = cmd
	return run
}

// name writes the pod's name, as a Stop or the console renames it.
func (r *supervisedRun) name(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "name"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

// awaitName waits for the pod's name to be want.
func (r *supervisedRun) awaitName(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		raw, _ := os.ReadFile(filepath.Join(r.dir, "name"))
		if string(raw) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pod is named %q, want %q", raw, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
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

const wantUpCall = `-s
-f
-m
20
-o
/dev/null
-X
PATCH
-H
Authorization: Bearer pod-key
-H
Content-Type: application/json
-d
{"name":"hl-job-a.up"}
https://api.runpod.io/v2/pods/pod1
`

func TestSupervisorRunsTheArgvUnchangedMarksThePodUpThenExitedAndKeepsIt(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		argv := []string{"/bin/sh", "-c", `printf '%s\n' "$@" > "$OUT"; sleep 0.5`, "argv0", "two  spaces", `quotes ' " $HOME \ kept`, "", "--flag=a b"}
		run := startSupervised(t, shell, t.TempDir(), argv...)

		run.awaitName(t, "hl-job-a.up", 10*time.Second)
		if calls := run.awaitFile(t, "curl-calls"); !strings.HasPrefix(calls, wantUpCall) {
			t.Errorf("rename\n got %q\nwant %q", calls, wantUpCall)
		}
		if got, want := run.awaitFile(t, "out"), strings.Join(argv[4:], "\n")+"\n"; got != want {
			t.Errorf("the command saw arguments %q, want %q", got, want)
		}
		run.awaitName(t, "hl-job-a.exited", 10*time.Second)
		time.Sleep(200 * time.Millisecond)
		if err := run.cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the supervisor exited after the command, which lets the vendor start it again: %v", err)
		}
	})
}

func TestSupervisorPassesOnEveryLineOfTheCommandALastOneWithoutNewlineIncluded(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", `echo '  out \ kept'; echo err >&2; printf final`)
		run.awaitName(t, "hl-job-a.exited", 15*time.Second)
		if got, want := run.awaitFile(t, "container-output"), "  out \\ kept\nerr\nfinal\n"; got != want {
			t.Errorf("container output %q, want %q", got, want)
		}
	})
}

func TestSupervisorMarksThePodExitedAgainWhenARenameDroppedTheMark(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", "true")
		run.awaitName(t, "hl-job-a.exited", 10*time.Second)
		run.name(t, "hl-job-a.stop.up")
		run.awaitName(t, "hl-job-a.stop.exited", 20*time.Second)
	})
}

func TestSupervisorDoesNotRunTheCommandOnASecondStart(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		dir := t.TempDir()
		first := startSupervised(t, shell, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
		first.awaitName(t, "hl-job-a.exited", 10*time.Second)
		_ = first.cmd.Process.Kill()
		_ = first.cmd.Wait()
		first.name(t, "hl-job-a.up")

		second := startSupervised(t, shell, dir, "/bin/sh", "-c", `echo run >> "$OUT"`)
		second.awaitName(t, "hl-job-a.exited", 10*time.Second)
		if got := second.awaitFile(t, "out"); got != "run\n" {
			t.Errorf("the command ran %d times, want once", strings.Count(got, "run"))
		}
	})
}

const savingCommand = `trap 'echo term >> "$OUT.terms"; sleep 1; echo saved > "$OUT"; exit 0' TERM; echo ready > "$OUT.ready"; while :; do sleep 0.05 & wait $!; done`

func TestSupervisorSendsSIGTERMOnceWhenItsNameIsMarkedAndLetsTheCommandFinish(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", savingCommand)
		run.awaitFile(t, "out.ready")
		run.awaitName(t, "hl-job-a.up", 10*time.Second)
		time.Sleep(6 * time.Second)
		if _, err := os.Stat(filepath.Join(run.dir, "out.terms")); err == nil {
			t.Fatal("the command was signalled while its name carried no mark, though the decoy did")
		}

		run.name(t, "hl-job-a.stop.up")
		run.awaitName(t, "hl-job-a.stop.exited", 15*time.Second)
		if saved, err := os.ReadFile(filepath.Join(run.dir, "out")); err != nil || string(saved) != "saved\n" {
			t.Errorf("the pod was marked exited before the command finished its save: %q, %v", saved, err)
		}
		time.Sleep(6 * time.Second)
		if terms := run.awaitFile(t, "out.terms"); terms != "term\n" {
			t.Errorf("the command got SIGTERM %d times, want once", strings.Count(terms, "term"))
		}
	})
}

func TestSupervisorKeepsTheCommandRunningWhileItsNameCannotBeRead(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "name"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		run := startSupervised(t, shell, dir, "/bin/sh", "-c", savingCommand)
		run.awaitFile(t, "out.ready")
		time.Sleep(7 * time.Second)
		if _, err := os.Stat(filepath.Join(run.dir, "out.terms")); err == nil {
			t.Error("the command was signalled because a read failed")
		}
		run.name(t, "hl-job-a")
		run.awaitName(t, "hl-job-a.up", 10*time.Second)
	})
}

func TestSupervisorStartsTheCommandWithoutThePodKey(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		t.Parallel()
		run := startSupervised(t, shell, t.TempDir(), "/bin/sh", "-c", `env > "$OUT"`)
		run.awaitName(t, "hl-job-a.exited", 10*time.Second)
		if calls := run.awaitFile(t, "curl-calls"); !strings.Contains(calls, "Authorization: Bearer pod-key\n") {
			t.Errorf("the supervisor lost the key it renames the pod with: %q", calls)
		}
		seen := run.awaitFile(t, "out")
		if strings.Contains(seen, "RUNPOD_API_KEY") || strings.Contains(seen, "pod-key") {
			t.Errorf("the command's environment carries the pod key:\n%s", seen)
		}
		if !strings.Contains(seen, "RUNPOD_POD_ID=pod1") {
			t.Errorf("the rest of the environment did not reach the command:\n%s", seen)
		}
	})
}

func TestSupervisorRefusesToRunTheCommandInAnImageWithoutItsTools(t *testing.T) {
	for _, missing := range []string{"curl", "mkfifo", "sleep"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, present := range []string{"curl", "mkfifo", "sleep"} {
				if present == missing {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, present), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			argv := supervisedCommand("hl-job-a", []string{"/bin/sh", "-c", `echo run > "$OUT"`}, false)
			cmd := exec.Command(argv[0], argv[1:]...)
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
				t.Error("the command ran in an image the supervisor cannot end")
			}
		})
	}
}

func TestTheImageNeedsWhatTheSupervisorRuns(t *testing.T) {
	p := &Provider{}
	for _, networked := range []bool{false, true} {
		for _, program := range p.ImageNeeds() {
			if !strings.Contains(supervisor("hl-job-a", networked), program+" ") {
				t.Errorf("the supervisor does not run %s", program)
			}
		}
	}
	if got := p.ImageNeeds(); !reflect.DeepEqual(got, []string{"curl", "mkfifo", "sleep"}) {
		t.Errorf("ImageNeeds = %v", got)
	}
}
