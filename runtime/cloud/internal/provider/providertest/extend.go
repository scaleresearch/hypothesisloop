package providertest

import (
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// extendLifetime is the lifetime the extended instance is rented with, and extendBy how far
// past it Extend moves its end.
const (
	extendLifetime = time.Minute
	extendBy       = 2 * time.Minute
)

// RunExtend is the check of the Extender capability, for an adapter that implements it: an
// instance whose end Extend moved later runs past the lifetime it was rented with, and one whose
// end Extend moved to now ends by itself. Extend of an instance rented with no lifetime fails.
func RunExtend(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	extender, ok := p.(provider.Extender)
	if !ok {
		t.Fatalf("%T does not implement provider.Extender", p)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	r := newRun(t, ctx, paced, opts)

	spec := r.spec("extended", "echo conformance-ready; while :; do sleep 5; done")
	spec.Lifetime = extendLifetime
	id, asked := r.create(t, ctx, spec)
	r.awaitLine(t, ctx, id, "conformance-ready", opts.StartTimeout)
	if err := extender.Extend(ctx, id, asked.Add(extendLifetime+extendBy)); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("the checks ran out of time before the test timeout: %v", ctx.Err())
	case <-time.After(time.Until(asked.Add(extendLifetime + extendBy/2))):
	}
	if i, listed := r.find(t, ctx, id); !listed || i.State != provider.StateRunning {
		t.Fatalf("%s after Create, past the lifetime it was rented with and inside the one Extend set, the instance is listed=%v state=%q", since(asked), listed, i.State)
	}

	moved := time.Now()
	if err := extender.Extend(ctx, id, moved); err != nil {
		t.Fatalf("Extend to now: %v", err)
	}
	r.awaitState(t, ctx, id, provider.StateExited, opts.SettleTimeout)
	t.Logf("exited %s after its end was moved to now", since(moved))
	if err := paced.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	endless := r.spec("endless", "echo conformance-ready; while :; do sleep 5; done")
	id, _ = r.create(t, ctx, endless)
	r.awaitLine(t, ctx, id, "conformance-ready", opts.StartTimeout)
	if err := extender.Extend(ctx, id, time.Now().Add(time.Hour)); err == nil {
		t.Errorf("Extend of an instance rented with no lifetime returned no error")
	}
	if err := paced.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}
