package providertest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

const (
	// The command says once per start that it started and whether the file an earlier start
	// left is still there, and then counts in memory. A count that goes on after a resume is the
	// same process; a second start line is the command run again.
	pauseScript = `if [ -e /tmp/conformance-pause ]; then echo conformance-start disk-kept; else echo conformance-start disk-new; fi
: > /tmp/conformance-pause
n=0
while :; do n=$((n+1)); echo "conformance-count $n"; sleep 2; done`

	// pauseHold is how long the instance stays paused: long enough that a command which kept
	// running would have counted well past where it stood.
	pauseHold = 40 * time.Second
)

// ResumeIs is what an adapter says a resume does to the command.
type ResumeIs int

const (
	// SameProcess: the command goes on where it stood, always.
	SameProcess ResumeIs = iota
	// SameProcessOrRestart: the command goes on where it stood unless the vendor lost the
	// paused state, and then starts again. The check accepts both and says which it saw.
	SameProcessOrRestart
)

// RunPause is the check of the Pauser capability, for an adapter that implements it: a running
// instance is paused, shows as paused under its name and stands still, is resumed and runs
// again the way the adapter says, and both calls can be repeated.
func RunPause(t *testing.T, p provider.Provider, opts Options, resume ResumeIs) {
	opts.validate(t)
	pauser, ok := p.(provider.Pauser)
	if !ok {
		t.Fatalf("%T does not implement provider.Pauser", p)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	r := newRun(t, ctx, paced, opts)
	spec := r.spec("pause", pauseScript)
	id, _ := r.create(t, ctx, spec)
	r.awaitLine(t, ctx, id, "conformance-count 3", opts.StartTimeout)
	r.awaitState(t, ctx, id, provider.StateRunning, opts.SettleTimeout)

	before, beforeAt := lastCount(r.logs(t, ctx, id, logTail)), time.Now()
	asked := time.Now()
	if err := pauser.Pause(ctx, id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	paused := r.awaitState(t, ctx, id, provider.StatePaused, opts.SettleTimeout)
	t.Logf("paused %s after Pause", since(asked))
	if paused.Name != spec.Name {
		t.Errorf("the paused instance is named %q, want %q", paused.Name, spec.Name)
	}
	if err := pauser.Pause(ctx, id); err != nil {
		t.Errorf("Pause of a paused instance: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("the checks ran out of time before the test timeout: %v", ctx.Err())
	case <-time.After(pauseHold):
	}
	if i, listed := r.find(t, ctx, id); !listed || i.State != provider.StatePaused {
		t.Fatalf("%s into the pause the instance is listed=%v state=%q", since(asked), listed, i.State)
	}

	asked = time.Now()
	if err := pauser.Resume(ctx, id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r.awaitState(t, ctx, id, provider.StateRunning, opts.StartTimeout)
	t.Logf("running %s after Resume", since(asked))
	if err := pauser.Resume(ctx, id); err != nil {
		t.Errorf("Resume of a running instance: %v", err)
	}
	lines := r.logs(t, ctx, id, logTail)
	first, stood := lastCount(lines), time.Since(beforeAt)
	starts := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "conformance-start") {
			starts++
		}
	}
	switch restarted := starts > 1 || first < before; {
	case !restarted:
		// The command counts once every two seconds while it runs, so the time it did not
		// count for is the time it stood still.
		still := stood - time.Duration(first-before)*2*time.Second
		if still < pauseHold/2 {
			t.Errorf("the command counted from %d to %d in %s: it stood still for %s of a pause of %s", before, first, stood.Round(time.Second), still.Round(time.Second), pauseHold)
		}
		t.Logf("the same process went on after the resume, at count %d after %d, still for %s", first, before, still.Round(time.Second))
	case resume == SameProcess:
		t.Errorf("the command was started again by the resume, and the adapter says the same process goes on; output: %q", lines)
	default:
		t.Logf("the command was started again by the resume; output: %q", lines)
	}
	// Two more counts show the command runs, whichever way it came back.
	r.awaitLine(t, ctx, id, "conformance-count "+strconv.Itoa(lastCount(r.logs(t, ctx, id, logTail))+2), opts.StartTimeout)

	if err := paced.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r.awaitGone(t, ctx, id, opts.SettleTimeout)
	if err := pauser.Pause(ctx, id); err != nil {
		t.Errorf("Pause of an instance that is gone: %v", err)
	}
	if err := pauser.Resume(ctx, id); err == nil {
		t.Errorf("Resume of an instance that is gone returned no error")
	}
}

func (r *run) awaitGone(t *testing.T, ctx context.Context, id string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if _, listed := r.find(t, ctx, id); !listed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s is still listed %s after Destroy", id, limit)
		}
		pause(t, ctx)
	}
}

func lastCount(lines []string) int {
	last := 0
	for _, line := range lines {
		if rest, counted := strings.CutPrefix(line, "conformance-count "); counted {
			if n, err := strconv.Atoi(rest); err == nil {
				last = n
			}
		}
	}
	return last
}
