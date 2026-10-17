package providertest

import (
	"fmt"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

const (
	beatInterval = 30 * time.Second

	holdScript = `echo conformance-started
n=0
while :; do echo "conformance-beat $n"; n=$((n+1)); sleep 30; done`
)

// RunHold rents one instance whose command only counts in the background, with no connection
// to it and nothing asked of it, and shows the vendor kept it running for the whole hold: it is
// running at every look, it counted without a gap, and it was not started a second time.
func RunHold(t *testing.T, p provider.Provider, opts Options, hold time.Duration) {
	opts.validate(t)
	p = &paced{Provider: p}
	ctx := guard(t, p)
	r := newRun(t, ctx, p, opts)
	id, _ := r.create(t, ctx, r.spec("hold", holdScript))
	r.awaitLine(t, ctx, id, "conformance-started", opts.StartTimeout)

	held := time.Now()
	for time.Since(held) < hold {
		select {
		case <-ctx.Done():
			t.Fatalf("the hold ran out of time before the test timeout: %v", ctx.Err())
		case <-time.After(beatInterval):
		}
		if i, listed := r.find(t, ctx, id); !listed || i.State != provider.StateRunning {
			t.Fatalf("%s into the hold the instance is listed=%v state=%q", since(held), listed, i.State)
		}
	}

	beats := int(hold / beatInterval)
	lines := r.logs(t, ctx, id, beats+50)
	if runs := count(lines, "conformance-started"); runs != 1 {
		t.Errorf("the command was started %d times, want once", runs)
	}
	for n := 0; n < beats; n++ {
		if beat := fmt.Sprintf("conformance-beat %d", n); count(lines, beat) != 1 {
			t.Errorf("output has %d lines %q, want 1: the command did not run through the hold", count(lines, beat), beat)
		}
	}
	t.Logf("held %s with %d beats", since(held), beats)
}
