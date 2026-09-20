package e2b

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

// startLaunched runs the launch file of command and env the way envd does, with the paths of a
// sandbox moved under dir, and imageEnv as the file the template script leaves in a template.
func startLaunched(t *testing.T, dir, imageEnv string, env map[string]string, command ...string) *exec.Cmd {
	t.Helper()
	file := strings.ReplaceAll(launchFile(command, env), superviseDir, dir)
	file = strings.ReplaceAll(file, imageEnvPath, filepath.Join(dir, "image.env"))
	if err := os.WriteFile(filepath.Join(dir, "launch"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if imageEnv != "-" {
		if err := os.WriteFile(filepath.Join(dir, "image.env"), []byte(imageEnv), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", filepath.Join(dir, "launch"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DIR=" + dir}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the launch file: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func exitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("launch file: %v", err)
	}
	return cmd.ProcessState.ExitCode()
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func awaitContent(t *testing.T, dir, name, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && string(raw) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %q, want %q", name, raw, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const hardValue = "two  spaces = 'single' \"double\" $HOME `tick` \\ back\nsecond line"

func TestLaunchFileHandsTheCommandItsArgvAndEnvironmentUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argv := []string{"/bin/sh", "-c", `printf '%s|' "$@"; printf '\n%s\n' "$VALUE"; echo to-stderr >&2`, "argv0", "two  spaces", hardValue, "", "--flag=a b", "-n"}
	cmd := startLaunched(t, dir, "", map[string]string{"VALUE": hardValue}, argv...)

	if code := exitCode(t, cmd); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	want := strings.Join(argv[4:], "|") + "|\n" + hardValue + "\nto-stderr\n"
	if got := read(t, dir, "out"); got != want {
		t.Errorf("the command wrote\n%q\nwant\n%q", got, want)
	}
}

func TestLaunchFileSetsTheImagesEnvironmentAndTheSpecsOverIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	imageEnv := "export FROM_IMAGE='image value'\nexport BOTH='from the image'\n"
	cmd := startLaunched(t, dir, imageEnv, map[string]string{"BOTH": "from the spec", "FROM_SPEC": "spec value"},
		"/bin/sh", "-c", `echo "$FROM_IMAGE|$BOTH|$FROM_SPEC"`)

	exitCode(t, cmd)
	if got := read(t, dir, "out"); got != "image value|from the spec|spec value\n" {
		t.Errorf("the command saw %q", got)
	}
}

func TestSupervisorWritesRunningThenExitedWithTheCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := startLaunched(t, dir, "", nil, "/bin/sh", "-c", `: > "$DIR/ready"; while [ ! -e "$DIR/go" ]; do sleep 0.05; done; printf final; exit 3`)

	awaitContent(t, dir, "ready", "")
	awaitContent(t, dir, "state", "running\n")
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd); code != 3 {
		t.Errorf("exit code = %d, want the command's 3", code)
	}
	if got := read(t, dir, "state"); got != "running\nexited 3\n" {
		t.Errorf("state = %q", got)
	}
	if got := read(t, dir, "out"); got != "final" {
		t.Errorf("output = %q, want the last line as the command left it, without a newline", got)
	}
}

func TestSupervisorMarksACommandThatCannotBeRunAsExited(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := startLaunched(t, dir, "", nil, "/no/such/program")

	if code := exitCode(t, cmd); code != 127 {
		t.Errorf("exit code = %d, want 127", code)
	}
	if got := read(t, dir, "state"); got != "running\nexited 127\n" {
		t.Errorf("state = %q", got)
	}
	if got := read(t, dir, "out"); !strings.Contains(got, "/no/such/program") {
		t.Errorf("output = %q, want the shell's complaint", got)
	}
}

const savingCommand = `trap 'echo term >> "$DIR/terms"; sleep 1; echo saved; exit 0' TERM; : > "$DIR/ready"; while :; do sleep 0.05 & wait $!; done`

func TestSupervisorPassesTheFirstSIGTERMOnAndLetsTheCommandFinish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := startLaunched(t, dir, "", nil, "/bin/sh", "-c", savingCommand)
	awaitContent(t, dir, "ready", "")

	for range 3 {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		if got := read(t, dir, "state"); got != "running\n" {
			t.Fatalf("state = %q while the command saves, want running", got)
		}
	}
	if code := exitCode(t, cmd); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if got := read(t, dir, "out"); got != "saved\n" {
		t.Errorf("output = %q, want the save finished", got)
	}
	if got := read(t, dir, "terms"); got != "term\n" {
		t.Errorf("the command got SIGTERM %d times, want once: Stop is repeated while it saves", strings.Count(got, "term"))
	}
	if got := read(t, dir, "state"); got != "running\nexited 0\n" {
		t.Errorf("state = %q", got)
	}
}

// A template the build script did not make has no environment file, and a command started
// without the image's PATH and settings would fail in ways nobody could place.
func TestLaunchFileRunsNothingInATemplateWithoutTheImagesEnvironment(t *testing.T) {
	dir := t.TempDir()
	cmd := startLaunched(t, dir, "-", nil, "/bin/sh", "-c", `: > "$DIR/ran"`)

	if code := exitCode(t, cmd); code == 0 {
		t.Error("the launch file succeeded without the image's environment file")
	}
	for _, name := range []string{"ran", "state"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written, so the command was started", name)
		}
	}
}
