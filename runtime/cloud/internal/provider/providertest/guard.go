package providertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// suitePrefix starts the name of every instance the suite rents, on any account and in any run.
const suitePrefix = "hlconf-"

// guard makes a run give back what it rents however it ends, and returns the context its checks
// work under. A check that fails is cleaned up by the test itself. The three ends a test's own
// cleanup never sees are covered here: the test timeout, by ending the checks while there is
// still time to destroy; an interrupt, by destroying before the process exits; and a process that
// was killed outright, by destroying its leftovers when the next run starts.
//
// The last one means one run per account at a time: a second run starting would destroy the
// first one's instances.
func guard(t *testing.T, p provider.Provider) context.Context {
	t.Helper()
	testDeadline, bounded := t.Deadline()
	return guardUntil(t, p, testDeadline, bounded)
}

// guardUntil is guard for a test binary that is killed at testDeadline, when bounded.
func guardUntil(t testing.TB, p provider.Provider, testDeadline time.Time, bounded bool) context.Context {
	t.Helper()
	ctx := context.Background()
	if bounded {
		end, err := checksEnd(testDeadline, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, end)
		t.Cleanup(cancel)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go destroyOnSignal(signals, p, os.Exit)
	t.Cleanup(func() {
		signal.Stop(signals)
		close(signals)
	})

	if err := sweep(ctx, p); err != nil {
		t.Fatalf("instances left by an earlier run: %v", err)
	}
	return ctx
}

// checksEnd is when the checks must stop so that destroying what they rented still fits before
// the test binary is killed at testDeadline.
func checksEnd(testDeadline, now time.Time) (time.Time, error) {
	end := testDeadline.Add(-cleanupLimit)
	if !end.After(now) {
		return time.Time{}, fmt.Errorf("providertest: the test timeout ends in %s, which leaves no time to rent anything and destroy it again; run with a longer -timeout", testDeadline.Sub(now).Round(time.Second))
	}
	return end, nil
}

// sweep destroys every instance of the suite on the account.
func sweep(ctx context.Context, p provider.Provider) error {
	left, err := p.List(ctx, suitePrefix)
	if err != nil {
		return fmt.Errorf("list %q: %w", suitePrefix, err)
	}
	var failed []error
	for _, i := range left {
		if err := p.Destroy(ctx, i.ID); err != nil {
			failed = append(failed, fmt.Errorf("instance %s (%s) is still rented: %w", i.ID, i.Name, err))
		}
	}
	return errors.Join(failed...)
}

// destroyOnSignal ends the process on the first signal, after destroying what the suite rented.
// It returns when signals is closed.
func destroyOnSignal(signals <-chan os.Signal, p provider.Provider, exit func(int)) {
	received, open := <-signals
	if !open {
		return
	}
	fmt.Fprintf(os.Stderr, "providertest: %s, destroying the suite's instances before exiting\n", received)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
	defer cancel()
	if err := sweep(ctx, p); err != nil {
		fmt.Fprintf(os.Stderr, "providertest: %v\n", err)
	}
	exit(1)
}
