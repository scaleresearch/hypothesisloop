package jobexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/exitline"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func Run(args []string, stdout, stderr io.Writer) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runExec(args, stdout, stderr, signals)
}

// runExec is the runtime's half inside a rented instance. The runtime outside can only start the
// instance, send it SIGTERM and read its output, so the window a job gets to save its work is
// kept here, and the exit code leaves as a line of output.
//
// The command runs in its own process group and the group is what gets signalled: a job written
// as `sh -c 'prepare && train'` has a shell between this wrapper and the process that saves, and
// that shell dies on SIGTERM without passing it on.
func runExec(args []string, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	fs := command.NewFlags("exec", `usage: hl exec --stop-grace SECONDS -- COMMAND [ARG...]

Runs COMMAND as a job on a rented instance. SIGTERM is passed on to COMMAND and everything it
started, which then have SECONDS to save their work before they are killed. COMMAND's exit
code is the job's: once COMMAND has ended and what it started has ended or been killed, the code
is written as the last line of output and returned.`, stderr)
	stopGrace := fs.Int("stop-grace", -1, "seconds COMMAND gets between SIGTERM and SIGKILL")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	commandLine := fs.Args()
	if *stopGrace < 0 || len(commandLine) == 0 {
		fmt.Fprintln(stderr, "hl exec: --stop-grace SECONDS and a COMMAND are required")
		return 2
	}

	cmd := exec.Command(commandLine[0], commandLine[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "hl exec: %v\n", err)
		return report(stdout, 127)
	}
	group := -cmd.Process.Pid
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()

	// A nil kill is a stop not yet asked for, and never fires.
	var kill <-chan time.Time
	stopping, killed := false, false
	var waitErr error
	for running := true; running; {
		select {
		case <-signals:
			// The runtime repeats its stop for as long as the instance is running, and a second
			// SIGTERM would interrupt the save the first one started.
			if stopping {
				continue
			}
			_ = syscall.Kill(group, syscall.SIGTERM)
			stopping, kill = true, time.After(time.Duration(*stopGrace)*time.Second)
		case <-kill:
			_ = syscall.Kill(group, syscall.SIGKILL)
			killed, kill = true, nil
		case waitErr = <-ended:
			running = false
		}
	}
	// COMMAND has ended, and its exit status is the job's result. What it started may still be
	// running: the process saving a checkpoint under a shell that died on the signal, or a helper
	// left in the background. Those keep the rest of the stop grace while a stop is running, and
	// are killed at once otherwise, so nothing writes after the exit line.
	if !stopping {
		_ = syscall.Kill(group, syscall.SIGKILL)
		killed = true
	}
	drain(group, kill, killed)

	code, err := exitCode(waitErr)
	if err != nil {
		fmt.Fprintf(stderr, "hl exec: %v\n", err)
	}
	// The runtime outside sees the instance through its vendor, which says late that the command
	// runs and that it ended, so when it ran is measured here.
	fmt.Fprintf(stdout, "\n%s", exitline.FormatRun(startedAt, time.Now()))
	return report(stdout, code)
}

const drainPoll = 20 * time.Millisecond

// leftoverWait is how long the group has to disappear once it has been sent SIGKILL. A process
// that is killed is gone well within it. What is still there afterwards is a process that has
// died and that its parent never collects, which this wrapper can only do for its own children
// and for orphans when it is the first process of the container. Such a process holds its group
// for good and can no longer write, so the exit line is written without it.
var leftoverWait = 5 * time.Second

// groupGone reports whether no process is left in group.
var groupGone = func(group int) bool {
	return errors.Is(syscall.Kill(group, 0), syscall.ESRCH)
}

// drain returns once no process is left in group. It kills what remains when kill fires, and
// after the group has been killed, by then or before the call, waits no longer than
// leftoverWait. It reaps as it waits: as the first process of a container this wrapper inherits
// the group's orphans.
func drain(group int, kill <-chan time.Time, killed bool) {
	var giveUp <-chan time.Time
	if killed {
		giveUp = time.After(leftoverWait)
	}
	for {
		for {
			var status syscall.WaitStatus
			if pid, err := syscall.Wait4(group, &status, syscall.WNOHANG, nil); pid <= 0 || err != nil {
				break
			}
		}
		if groupGone(group) {
			return
		}
		select {
		case <-kill:
			_ = syscall.Kill(group, syscall.SIGKILL)
			kill, giveUp = nil, time.After(leftoverWait)
		case <-giveUp:
			return
		case <-time.After(drainPoll):
		}
	}
}

// report writes the exit line on a line of its own. The command's output may have ended without
// a newline, and the line is only found at the start of one.
func report(stdout io.Writer, code int) int {
	fmt.Fprintf(stdout, "\n%s\n", exitline.Format(code))
	return code
}

// exitCode is the shell's reading of how a command ended: its own code, or 128 plus the signal
// that killed it.
func exitCode(waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		return 1, waitErr
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return exit.ExitCode(), nil
}
