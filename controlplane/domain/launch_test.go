package domain

import (
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
)

const launchToken = "job-token-1"

// gitServer serves the repository whose commit job.sh holds over smart HTTP, answering only a
// request that carries the job's token as a Bearer header, as the platform's git proxy does.
func gitServer(t *testing.T, jobScript string) (url, commit string) {
	t.Helper()
	gitExec, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	run(src, "init", "-q")
	if err := os.WriteFile(filepath.Join(src, "job.sh"), []byte(jobScript), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", "job.sh")
	run(src, "commit", "-q", "-m", "job")
	commit = run(src, "rev-parse", "HEAD")
	run(root, "clone", "-q", "--bare", src, filepath.Join(root, "repo.git"))

	backend := &cgi.Handler{
		Path:   filepath.Join(strings.TrimSpace(string(gitExec)), "git-http-backend"),
		Env:    []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
		Stderr: io.Discard,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+launchToken || strings.Contains(r.URL.String(), launchToken) {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/repo.git", commit
}

type launched struct {
	output string
	code   int
	sleeps []string
	took   time.Duration
}

func (l launched) exitLine(t *testing.T) clusterapi.ExitLine {
	t.Helper()
	lines := strings.Split(strings.TrimRight(l.output, "\n"), "\n")
	line, ok := clusterapi.FindExitLine(lines)
	if !ok {
		t.Fatalf("no exit line in:\n%s", l.output)
	}
	if !strings.HasPrefix(lines[len(lines)-1], clusterapi.ExitLinePrefix+" code=") {
		t.Fatalf("the last line is %q, want the exit line:\n%s", lines[len(lines)-1], l.output)
	}
	return line
}

// eachShell runs test under each shell images commonly hold: their sh, dash and bash.
func eachShell(t *testing.T, test func(t *testing.T, shell string)) {
	for _, shell := range []string{"sh", "dash", "bash"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			test(t, path)
		})
	}
}

// launch runs the launch script under shell in a process group of its own, as a runtime runs it as
// a container's first process, with its code directory moved under a temporary one. Its sleeps
// of five seconds and more, the pauses between fetches, return at once and are recorded. signal,
// when set, is sent to the script once the file named by READY exists.
func launch(t *testing.T, shell, gitURL, commit, grace string, signal syscall.Signal, command ...string) launched {
	t.Helper()
	dir := t.TempDir()
	shims := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\necho \"$1\" >> \"$SLEEPS\"\ncase \"$1\" in 5|10|20|40|60) exit 0;; esac\nexec " + realSleep + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shims, "sleep"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	ready, sleeps, out := filepath.Join(dir, "ready"), filepath.Join(dir, "sleeps"), filepath.Join(dir, "out")
	output, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	script := strings.ReplaceAll(LaunchScript, JobCodeDir, filepath.Join(dir, "workspace", "code"))
	cmd := exec.Command(shell, clusterapi.LaunchCommand(script, command, nil)[1:]...)
	cmd.Env = []string{
		"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"READY=" + ready, "SLEEPS=" + sleeps,
		jobenv.Token + "=" + launchToken,
		jobenv.GitURL + "=" + gitURL,
		jobenv.CodeRef + "=https://github.com/acme/model.git@" + commit,
		jobenv.StopGrace + "=" + grace,
	}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	group := -cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(group, syscall.SIGKILL)
		_ = cmd.Wait()
		if !groupEnds(group, 10*time.Second) {
			t.Errorf("process group %d outlived SIGKILL", -group)
		}
	})
	if signal != 0 {
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(group, syscall.SIGKILL)
				_ = cmd.Wait()
				raw, _ := os.ReadFile(out)
				t.Fatalf("the command never started:\n%s", raw)
			}
		}
		if err := cmd.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
	}
	_ = cmd.Wait()
	took := time.Since(start)
	if !groupEnds(group, 2*time.Second) {
		left, _ := exec.Command("ps", "-o", "pid,ppid,pgid,command", "-g", strconv.Itoa(-group)).CombinedOutput()
		t.Errorf("a process of the launch outlived it by 2s:\n%s", left)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	result := launched{output: string(raw), code: cmd.ProcessState.ExitCode(), took: took}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.code = 128 + int(status.Signal())
	}
	if recorded, err := os.ReadFile(sleeps); err == nil {
		result.sleeps = strings.Fields(string(recorded))
	}
	return result
}

// groupEnds reports whether no process is left in group, negative as kill takes it, within wait.
func groupEnds(group int, wait time.Duration) bool {
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(group, 0) == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// The commit is fetched with the token as a header and checked out, the command runs in it with
// its arguments, and its exit code ends the script and is the exit line's, timed by this clock.
func TestTheLaunchScriptRunsTheCommandInTheJobsCommitAndEndsWithItsExitLine(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		url, commit := gitServer(t, "pwd\necho \"args: $*\"\nexit 3\n")
		before := time.Now().Add(-time.Second).UTC().Truncate(time.Second)
		got := launch(t, shell, url, commit, "30", 0, "sh", "job.sh", "--lr", "0.1")

		line := got.exitLine(t)
		if got.code != 3 || line.Code != 3 {
			t.Fatalf("exit %d, exit line %+v, want 3:\n%s", got.code, line, got.output)
		}
		if !strings.Contains(got.output, filepath.Join("workspace", "code")+"\n") || !strings.Contains(got.output, "args: --lr 0.1\n") {
			t.Errorf("the command did not run in the code directory with its arguments:\n%s", got.output)
		}
		if line.StartedAt.Before(before) || line.EndedAt.Before(line.StartedAt) || line.EndedAt.After(time.Now().Add(time.Second)) {
			t.Errorf("exit line times %s to %s, want within the run that began at %s", line.StartedAt, line.EndedAt, before)
		}
	})
}

// A repository that never answers is tried seven times over about three minutes, the pauses
// doubling to a minute, and ends the script with the code that tells the platform the command
// never ran.
func TestALaunchWhoseRepositoryNeverAnswersEndsWithCodeUnreachable(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		listener := httptest.NewServer(http.NotFoundHandler())
		url := listener.URL + "/repo.git"
		listener.Close()

		got := launch(t, shell, url, strings.Repeat("a", 40), "30", 0, "sh", "-c", "echo the-command-ran")

		if line := got.exitLine(t); got.code != clusterapi.ExitCodeCodeUnreachable || line.Code != clusterapi.ExitCodeCodeUnreachable {
			t.Fatalf("exit %d, exit line %+v, want %d:\n%s", got.code, line, clusterapi.ExitCodeCodeUnreachable, got.output)
		}
		if want := []string{"5", "10", "20", "40", "60", "60"}; !slices.Equal(got.sleeps, want) {
			t.Errorf("paused %v, want %v", got.sleeps, want)
		}
		if strings.Contains(got.output, "the-command-ran") || !strings.Contains(got.output, "did not answer in 7 tries") {
			t.Errorf("output:\n%s", got.output)
		}
	})
}

// A repository that answers without the commit ends the script with 1: queueing the job again
// would fetch the same nothing.
func TestALaunchOfACommitTheRepositoryLacksEndsWith1(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		url, _ := gitServer(t, "exit 0\n")
		missing := strings.Repeat("b", 40)

		got := launch(t, shell, url, missing, "30", 0, "sh", "job.sh")

		if line := got.exitLine(t); got.code != 1 || line.Code != 1 {
			t.Fatalf("exit %d, exit line %+v, want 1:\n%s", got.code, line, got.output)
		}
		if !strings.Contains(got.output, "commit "+missing+" is not in the repository") {
			t.Errorf("output does not say the commit is missing:\n%s", got.output)
		}
	})
}

// SIGTERM reaches every process the command started, a grandchild behind a shell included, and
// a command that ends within its grace ends the script with its own code.
func TestSIGTERMReachesEveryProcessOfTheCommand(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		job := `( trap 'echo grandchild-stopped; exit 0' TERM; while :; do /bin/sleep 0.05; done ) &
	trap 'wait; exit 7' TERM
	touch "$READY"
	while :; do /bin/sleep 0.05; done
	`
		url, commit := gitServer(t, job)

		got := launch(t, shell, url, commit, "30", syscall.SIGTERM, "sh", "job.sh")

		if line := got.exitLine(t); got.code != 7 || line.Code != 7 {
			t.Fatalf("exit %d, exit line %+v, want the command's 7:\n%s", got.code, line, got.output)
		}
		if !strings.Contains(got.output, "grandchild-stopped") {
			t.Errorf("the grandchild never saw SIGTERM:\n%s", got.output)
		}
		if got.took > 15*time.Second {
			t.Errorf("took %s: the command was not stopped by SIGTERM", got.took)
		}
	})
}

// A command that outlives its stop grace is killed with every process of the job, after the exit
// line says 137.
func TestACommandPastItsStopGraceIsKilledWithExitLine137(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		url, commit := gitServer(t, "trap '' TERM\ntouch \"$READY\"\nexec /bin/sleep 60\n")

		got := launch(t, shell, url, commit, "1", syscall.SIGTERM, "sh", "job.sh")

		if line := got.exitLine(t); line.Code != 137 || got.code != 137 {
			t.Fatalf("exit %d, exit line %+v, want 137:\n%s", got.code, line, got.output)
		}
		if got.took > 15*time.Second || !strings.Contains(got.output, "did not end within 1s of SIGTERM") {
			t.Errorf("took %s, output:\n%s", got.took, got.output)
		}
	})
}

func TestTheExitLineIsWhatTheRuntimesParse(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		url, commit := gitServer(t, "echo 'hl-launch: code=0 started=1 ended=2'\nexit 4\n")
		got := launch(t, shell, url, commit, "30", 0, "sh", "job.sh")
		if line := got.exitLine(t); line.Code != 4 || line.StartedAt.Unix() <= 2 {
			t.Fatalf("exit line %+v, want the script's own with code 4, not the one the command printed:\n%s", line, got.output)
		}
		if !strings.Contains(got.output, "\n"+clusterapi.ExitLinePrefix+" code=4 started="+strconv.FormatInt(got.exitLine(t).StartedAt.Unix(), 10)+" ended=") {
			t.Errorf("exit line not in the form %q:\n%s", clusterapi.ExitLinePrefix+" code=<n> started=<s> ended=<s>", got.output)
		}
	})
}

// Only the script reports its code unreachable: the command's own exit with that code is a
// failure of the command, which queueing it again for free would only repeat.
func TestACommandThatExitsWithCodeUnreachableEndsTheLaunchWith1(t *testing.T) {
	if codeUnreachable != strconv.Itoa(clusterapi.ExitCodeCodeUnreachable) {
		t.Fatalf("the script writes %s for clusterapi.ExitCodeCodeUnreachable %d", codeUnreachable, clusterapi.ExitCodeCodeUnreachable)
	}
	eachShell(t, func(t *testing.T, shell string) {
		url, commit := gitServer(t, "exit "+codeUnreachable+"\n")
		got := launch(t, shell, url, commit, "30", 0, "sh", "job.sh")
		if line := got.exitLine(t); got.code != 1 || line.Code != 1 {
			t.Fatalf("exit %d, exit line %+v, want 1:\n%s", got.code, line, got.output)
		}
	})
}
