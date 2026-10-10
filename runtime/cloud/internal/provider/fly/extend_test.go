package fly

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/supervise/supervisetest"
)

const execPath = machinesPath + "/9001/exec"

// sentScript is the shell text the one exec request carried.
func sentScript(t *testing.T, api *fakeAPI) string {
	t.Helper()
	sent := api.sent(http.MethodPost, execPath)
	if len(sent) != 1 {
		t.Fatalf("exec requests = %d, want one", len(sent))
	}
	command, _ := sent[0].Body["command"].([]any)
	if len(command) != 3 || command[0] != "/bin/sh" || command[1] != "-c" || sent[0].Body["timeout"] != float64(execTimeoutSeconds) || sent[0].Auth != testToken {
		t.Fatalf("exec = %+v, want one command run by /bin/sh -c within the timeout", sent[0])
	}
	script, _ := command[2].(string)
	return script
}

// The script Extend sends is run here against a supervisor of this package: the Machine's end
// is moved by what the supervisor reads, and nothing else.
func TestExtendMovesTheEndTheSupervisorReads(t *testing.T) {
	supervisetest.EachShell(t, func(t *testing.T, shell []string) {
		dir := t.TempDir()
		defer func(was string) { endsAtPath = was }(endsAtPath)
		endsAtPath = filepath.Join(dir, "ends-at")

		api := newFakeAPI(t)
		api.on(http.MethodPost, execPath, answer(200, `{"stdout":""}`))
		p := newTestProvider(t, api)

		argv := supervised(2*time.Second, []string{"/bin/sh", "-c", `trap '' TERM; : > "$DIR/ready"; while :; do sleep 0.05 & wait $!; done`})
		cmd := supervisetest.Command(shell, argv[1:]...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + dir, "DIR=" + dir}
		cmd.Stdout = &bytes.Buffer{}
		began := time.Now()
		if err := cmd.Start(); err != nil {
			t.Fatalf("start supervisor: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		awaitFile(t, filepath.Join(dir, "ready"))

		if err := p.Extend(context.Background(), "9001", began.Add(5*time.Second)); err != nil {
			t.Fatalf("Extend: %v", err)
		}
		if out, err := exec.Command("/bin/sh", "-c", sentScript(t, api)).CombinedOutput(); err != nil {
			t.Fatalf("the script Extend sent failed in the Machine: %v: %s", err, out)
		}
		wantLines(t, finish(t, cmd), "hl-ended")
		if lived := time.Since(began); lived < 4*time.Second || lived > 9*time.Second {
			t.Errorf("the supervisor ended after %s, want the five seconds Extend set in place of its lifetime of two", lived.Round(10*time.Millisecond))
		}
	})
}

func TestExtendOfAMachineThatKeepsNoEndIsAnErrorThatSaysSo(t *testing.T) {
	dir := t.TempDir()
	defer func(was string) { endsAtPath = was }(endsAtPath)
	endsAtPath = filepath.Join(dir, "ends-at")

	api := newFakeAPI(t)
	api.on(http.MethodPost, execPath, answer(200, `{"exit_code":3}`))
	p := newTestProvider(t, api)
	err := p.Extend(context.Background(), "9001", time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "keeps no end") {
		t.Fatalf("Extend = %v, want it refused as a machine that keeps no end", err)
	}
	out, runErr := exec.Command("/bin/sh", "-c", sentScript(t, api)).CombinedOutput()
	var exit *exec.ExitError
	if runErr == nil || !errors.As(runErr, &exit) || exit.ExitCode() != noEndKept {
		t.Fatalf("the script run where no end is kept = %v: %s, want exit %d", runErr, out, noEndKept)
	}
	if _, err := os.Stat(endsAtPath); err == nil {
		t.Fatal("the script wrote an end that no supervisor reads")
	}
}

func TestExtendReportsWhatTheAPIAndTheMachineAnswer(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"gone":         answer(404, `{"error":"machine not found"}`),
		"not running":  answer(412, `{"error":"failed_precondition: machine not started"}`),
		"server error": answer(500, `oops`),
		"failed":       answer(200, `{"exit_code":1,"stderr":"mv: cannot move"}`),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, execPath, handler)
			p := newTestProvider(t, api)
			if err := p.Extend(context.Background(), "9001", time.Now().Add(time.Hour)); err == nil {
				t.Fatal("Extend succeeded")
			}
		})
	}
}
