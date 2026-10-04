package providertest

import (
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// RunWarm is the check of the Warmer capability, for an adapter that implements it: a warmed
// instance is listed under its name, becomes warm without running anything, and StartWarm runs a
// spec's command with its environment under the spec's name, which then ends as any instance
// does. StartWarm of an instance that is gone fails.
func RunWarm(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	warmer, ok := p.(provider.Warmer)
	if !ok {
		t.Fatalf("%T does not implement provider.Warmer", p)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	r := newRun(t, ctx, paced, opts)

	pool := r.spec("pool", "exit 0")
	asked := time.Now()
	id, err := warmer.Warm(ctx, pool)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if listed, found := r.find(t, ctx, id); !found || listed.Name != pool.Name || (listed.State != provider.StateStarting && listed.State != provider.StateWarm) {
		t.Fatalf("when Warm returns the instance is listed=%v as %+v, want %q starting or warm", found, listed, pool.Name)
	}
	r.awaitState(t, ctx, id, provider.StateWarm, opts.StartTimeout)
	t.Logf("warm %s after Warm", since(asked))
	if lines := r.logs(t, ctx, id, logTail); len(lines) != 0 {
		t.Errorf("a warm instance wrote %q", lines)
	}

	spec := r.spec("started", `printf 'conformance-env=%s\n' "$HL_CONFORMANCE_VALUE"; echo conformance-ready`)
	spec.Env = map[string]string{"HL_CONFORMANCE_VALUE": envValue}
	spec.Lifetime = time.Hour
	asked = time.Now()
	if err := warmer.StartWarm(ctx, id, spec); err != nil {
		t.Fatalf("StartWarm: %v", err)
	}
	if listed, found := r.find(t, ctx, id); !found || listed.Name != spec.Name {
		t.Fatalf("when StartWarm returns the instance is listed=%v as %q, want %q", found, listed.Name, spec.Name)
	}
	lines := r.awaitLine(t, ctx, id, "conformance-ready", opts.StartTimeout)
	t.Logf("command output readable %s after StartWarm", since(asked))
	if !slices.Contains(lines, "conformance-env="+envValue) {
		t.Errorf("the environment did not arrive as it was given: %q", lines)
	}
	r.awaitState(t, ctx, id, provider.StateExited, opts.SettleTimeout)

	if err := paced.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r.awaitGone(t, ctx, id, opts.SettleTimeout)
	if err := warmer.StartWarm(ctx, id, spec); err == nil {
		t.Errorf("StartWarm of an instance that is gone returned no error")
	}
}
