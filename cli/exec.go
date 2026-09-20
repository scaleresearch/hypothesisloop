package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

func cmdExec(args []string, stdout, stderr io.Writer) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runExec(args, stdout, stderr, signals)
}

// runExec is the runtime's half inside a rented instance. The runtime outside can only start the
// instance, send it SIGTERM and read its output, so the window a job gets to save its work is
// kept here, and the exit code leaves as a line of output.
func runExec(args []string, stdout, stderr io.Writer, signals <-chan os.Signal) int {
	fs := newFlags("exec", `usage: hl exec --stop-grace SECONDS -- COMMAND [ARG...]

Runs COMMAND as a job on a rented instance. SIGTERM is passed on to COMMAND, which then has
SECONDS to save its work before it is killed. When COMMAND ends, its exit code is written as the
last line of output and returned.`, stderr)
	stopGrace := fs.Int("stop-grace", -1, "seconds COMMAND gets between SIGTERM and SIGKILL")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	command := fs.Args()
	if *stopGrace < 0 || len(command) == 0 {
		fmt.Fprintln(stderr, "hl exec: --stop-grace SECONDS and a COMMAND are required")
		return 2
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "hl exec: %v\n", err)
		return report(stdout, 127)
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()

	var kill <-chan time.Time
	for {
		select {
		case <-signals:
			// The runtime repeats its stop for as long as the instance is running, and a second
			// SIGTERM would interrupt the save the first one started.
			if kill != nil {
				continue
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			kill = time.After(time.Duration(*stopGrace) * time.Second)
		case <-kill:
			_ = cmd.Process.Kill()
		case err := <-ended:
			code, err := exitCode(err)
			if err != nil {
				fmt.Fprintf(stderr, "hl exec: %v\n", err)
			}
			return report(stdout, code)
		}
	}
}

func report(stdout io.Writer, code int) int {
	fmt.Fprintln(stdout, exitline.Format(code))
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
