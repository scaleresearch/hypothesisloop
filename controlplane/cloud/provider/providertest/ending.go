package providertest

import (
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

const (
	endingScript = `echo conformance-started
sleep 5
printf conformance-final
exit 3`

	// endedHold is how long an exited instance is watched for the vendor starting it again.
	endedHold = 90 * time.Second

	savingScript = `trap 'echo conformance-saving; sleep 20; echo conformance-saved; exit 0' TERM
echo conformance-ready
while :; do sleep 1 & wait $!; done`
)

// commandThatEnds holds the vendor to StateExited: a command that ends on its own leaves an
// exited instance, the first look that shows it exited can read its output to the last byte (a
// final line with no newline is the hardest to get), and it is not run a second time.
func commandThatEnds(t *testing.T, r *run) {
	ctx := r.ctx
	id, asked := r.create(t, ctx, r.spec("ends", endingScript))

	deadline := time.Now().Add(r.opts.StartTimeout + r.opts.SettleTimeout)
	var running time.Time
	for {
		i, listed := r.find(t, ctx, id)
		if !listed {
			t.Fatalf("instance %s left List after its command ended", id)
		}
		if i.State == provider.StateExited {
			break
		}
		if i.State == provider.StateRunning && running.IsZero() {
			running = time.Now()
		}
		if time.Now().After(deadline) {
			runs := 0
			if i.State != provider.StateStarting && i.State != provider.StateUnknown {
				runs = count(r.logs(t, ctx, id, logTail), "conformance-started")
			}
			t.Fatalf("instance %s never showed as exited (now %s); its command has been started %d times", id, i.State, runs)
		}
		pause(t, ctx)
	}
	lines := r.logs(t, ctx, id, logTail)
	if len(lines) == 0 || lines[len(lines)-1] != "conformance-final" {
		t.Errorf("the instance showed as exited before its last line was readable: %q", lines)
	}
	t.Logf("exited with complete output %s after Create returned", since(asked))
	if !running.IsZero() {
		t.Logf("the five second command showed as running for %s", since(running))
	}

	held := time.Now()
	for time.Since(held) < endedHold {
		pause(t, ctx)
		if i, listed := r.find(t, ctx, id); !listed || i.State != provider.StateExited {
			t.Errorf("%s after exiting, the instance is listed=%v state=%q, want it to stay exited", since(held), listed, i.State)
			break
		}
	}
	if runs := count(r.logs(t, ctx, id, logTail), "conformance-started"); runs != 1 {
		t.Errorf("the command was started %d times, want once", runs)
	}
}

// stopLetsTheCommandFinish holds the vendor to Stop's sentence that the instance is running
// until the command exits: a command that takes twenty seconds to save after SIGTERM gets them.
func stopLetsTheCommandFinish(t *testing.T, r *run) {
	ctx := r.ctx
	id, _ := r.create(t, ctx, r.spec("saves", savingScript))
	r.awaitLine(t, ctx, id, "conformance-ready", r.opts.StartTimeout)

	stopped := time.Now()
	if err := r.p.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	r.awaitLine(t, ctx, id, "conformance-saving", r.opts.SettleTimeout)
	t.Logf("SIGTERM seen in the output %s after Stop", since(stopped))
	if err := r.p.Stop(ctx, id); err != nil {
		t.Errorf("Stop repeated while the command saves: %v", err)
	}
	r.awaitState(t, ctx, id, provider.StateExited, r.opts.SettleTimeout)
	t.Logf("exited %s after Stop", since(stopped))
	lines := r.logs(t, ctx, id, logTail)
	if count(lines, "conformance-saving") != 1 {
		t.Fatalf("Stop did not reach the command as SIGTERM; output: %q", lines)
	}
	if count(lines, "conformance-saved") != 1 {
		t.Errorf("the command was killed before it finished saving; output: %q", lines)
	}
}
