package quota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// GET /resource-catalog/capacity used to answer 200 with an empty cluster list when the handler
// was never wired to a metrics DB (metricsDBURL == ""), which reads exactly like "no cluster
// currently has this type free" — a job submitted against a real type then queues forever with no
// error (see the endpoint's own doc). That silently converts an operator misconfiguration into
// starvation nobody can diagnose from the API. Fail fast instead (important.md: "no fallbacks -
// one path or error, fail fast").
func TestLiveCapacityFailsFastWhenUnconfigured(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, nil)
	// Deliberately never call WithLiveCapacity.

	clusters, err := h.liveCapacity(context.Background())
	if err == nil {
		t.Fatalf("liveCapacity() with no metrics DB configured returned nil error and clusters=%v, want ErrLiveCapacityUnconfigured", clusters)
	}
	if !errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() error = %v, want ErrLiveCapacityUnconfigured", err)
	}
	if clusters != nil {
		t.Fatalf("liveCapacity() clusters = %v, want nil on error", clusters)
	}
}

// Once wired, an empty result must still be possible on its own merits (e.g. a metrics DB that is
// reachable but simply has no fresh reports) — this test just pins that WithLiveCapacity flips the
// handler out of the unconfigured error path; the live-read behavior itself is metricsdb's own.
func TestLiveCapacityConfiguredDoesNotFailFast(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, nil).WithLiveCapacity("postgres://example-not-dialed/db", time.Minute)
	if h.metricsDBURL == "" {
		t.Fatalf("WithLiveCapacity did not set metricsDBURL")
	}
	// Not asserting on the outcome of an actual dial here — that's metricsdb's own concern and
	// would require a live DB. This only pins that liveCapacity no longer takes the "unconfigured"
	// branch once WithLiveCapacity has been called.
	_, err := h.liveCapacity(context.Background())
	if errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() still reports unconfigured after WithLiveCapacity was called")
	}
}
