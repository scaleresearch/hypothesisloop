package providertest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

func rentedOn(t *testing.T, names ...string) *fake.Provider {
	t.Helper()
	p := fake.New()
	for _, name := range names {
		if _, err := p.Create(context.Background(), provider.Spec{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func names(t *testing.T, p provider.Provider) []string {
	t.Helper()
	listed, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range listed {
		out = append(out, i.Name)
	}
	return out
}

func TestSweepDestroysTheSuitesLeftoversAndNothingElse(t *testing.T) {
	p := rentedOn(t, "hlconf-0a1b2c3d-lifecycle", "hlconf-ffffffff-ends", "hl-job-main-exp1-a1", "xhlconf-not-ours")

	if err := sweep(context.Background(), p); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if left := names(t, p); len(left) != 2 || left[0] != "hl-job-main-exp1-a1" || left[1] != "xhlconf-not-ours" {
		t.Errorf("left on the account: %q, want only the two instances the suite did not rent", left)
	}
}

func TestSweepNamesWhatItCouldNotDestroyAndStillDestroysTheRest(t *testing.T) {
	p := rentedOn(t, "hlconf-0a1b2c3d-lifecycle", "hlconf-ffffffff-ends")
	p.Fail = func(method, name string) error {
		if method == "Destroy" && name == "hlconf-0a1b2c3d-lifecycle" {
			return errors.New("vendor said no")
		}
		return nil
	}

	err := sweep(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "hlconf-0a1b2c3d-lifecycle") || !strings.Contains(err.Error(), "still rented") {
		t.Fatalf("err = %v, want one naming the instance still rented", err)
	}
	if left := names(t, p); len(left) != 1 || left[0] != "hlconf-0a1b2c3d-lifecycle" {
		t.Errorf("left on the account: %q, want only the one that could not be destroyed", left)
	}
}

func TestSweepFailsWhenTheAccountCannotBeListed(t *testing.T) {
	p := rentedOn(t, "hlconf-0a1b2c3d-lifecycle")
	p.Fail = func(method, _ string) error {
		if method == "List" {
			return errors.New("vendor is down")
		}
		return nil
	}

	if err := sweep(context.Background(), p); err == nil {
		t.Fatal("no error, although nothing could be checked")
	}
}

func TestAnInterruptDestroysTheSuitesInstancesBeforeTheProcessExits(t *testing.T) {
	p := rentedOn(t, "hlconf-0a1b2c3d-lifecycle", "hlconf-ffffffff-ends", "hl-job-main-exp1-a1")
	signals := make(chan os.Signal, 1)
	exited := make(chan []string, 1)
	go destroyOnSignal(signals, p, func(code int) {
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		exited <- names(t, p)
	})

	signals <- os.Interrupt
	select {
	case left := <-exited:
		if len(left) != 1 || left[0] != "hl-job-main-exp1-a1" {
			t.Errorf("on the account when the process exits: %q, want nothing of the suite", left)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit after the interrupt")
	}
}

func TestARunThatEndsNormallyLeavesTheSignalWatcherWithoutExiting(t *testing.T) {
	signals := make(chan os.Signal, 1)
	returned := make(chan struct{})
	go func() {
		destroyOnSignal(signals, fake.New(), func(int) { t.Error("exit was called with no signal") })
		close(returned)
	}()

	close(signals)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher is still waiting")
	}
}

func TestChecksEndWhileThereIsStillTimeToDestroy(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	end, err := checksEnd(now.Add(45*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := now.Add(45 * time.Minute).Sub(end); got != cleanupLimit {
		t.Errorf("the checks end %s before the test timeout, want %s", got, cleanupLimit)
	}

	for _, timeout := range []time.Duration{cleanupLimit, cleanupLimit - time.Second, 0} {
		if _, err := checksEnd(now.Add(timeout), now); err == nil || !strings.Contains(err.Error(), "-timeout") {
			t.Errorf("a test timeout %s away: err = %v, want a refusal that says to raise -timeout", timeout, err)
		}
	}
}

// guard is what Run and RunHold start with: an account that still holds an aborted run's
// instances is cleaned before anything new is rented, and the checks' context ends ahead of the
// test's own deadline.
func TestGuardSweepsAtTheStartAndBoundsTheChecks(t *testing.T) {
	p := rentedOn(t, "hlconf-0a1b2c3d-lifecycle", "hl-job-main-exp1-a1")

	ctx := guard(t, p)
	if left := names(t, p); len(left) != 1 || left[0] != "hl-job-main-exp1-a1" {
		t.Errorf("after the start of a run: %q, want the earlier run's instance gone", left)
	}
	testDeadline, bounded := t.Deadline()
	checksDeadline, set := ctx.Deadline()
	if bounded && (!set || testDeadline.Sub(checksDeadline) != cleanupLimit) {
		t.Errorf("checks end at %s, test at %s, want %s between them", checksDeadline, testDeadline, cleanupLimit)
	}
}
