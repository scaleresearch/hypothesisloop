package providertest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// find returns the instance as List shows it under the run's prefix.
func (r *run) find(t *testing.T, ctx context.Context, id string) (provider.Instance, bool) {
	t.Helper()
	listed, err := r.p.List(ctx, r.prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", r.prefix, err)
	}
	for _, i := range listed {
		if i.ID == id {
			return i, true
		}
	}
	return provider.Instance{}, false
}

func (r *run) logs(t *testing.T, ctx context.Context, id string, tail int) []string {
	t.Helper()
	lines, err := r.p.Logs(ctx, id, tail)
	if err != nil {
		t.Fatalf("Logs(%s, %d): %v", id, tail, err)
	}
	return lines
}

// awaitLine waits until the command has written line and returns the output read then.
func (r *run) awaitLine(t *testing.T, ctx context.Context, id, line string, limit time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if i, listed := r.find(t, ctx, id); !listed {
			t.Fatalf("instance %s left List while waiting for %q", id, line)
		} else if i.State != provider.StateStarting {
			if lines := r.logs(t, ctx, id, logTail); slices.Contains(lines, line) {
				return lines
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s did not write %q within %s", id, line, limit)
		}
		pause(t, ctx)
	}
}

func (r *run) awaitState(t *testing.T, ctx context.Context, id string, want provider.State, limit time.Duration) provider.Instance {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		i, listed := r.find(t, ctx, id)
		if !listed {
			t.Fatalf("instance %s left List while waiting for state %s", id, want)
		}
		if i.State == want {
			return i
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s is %s after %s, want %s", id, i.State, limit, want)
		}
		pause(t, ctx)
	}
}

func pause(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("the checks ran out of time before the test timeout: %v", ctx.Err())
	case <-time.After(pollInterval):
	}
}

func count(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}
