package settlement

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// fakeStore is a minimal in-memory settlement.Store double sufficient to exercise
// Reconciler.reconcileOnce's pending-reservation retry sweep without a real Postgres instance.
type fakeStore struct {
	reservations map[string]bool // experiment_id -> reservation still present
	deleteErrs   map[string]int  // experiment_id -> number of times DeletePendingReservation should fail before succeeding
}

func (f *fakeStore) ListUnsettledTerminalExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}
func (f *fakeStore) MarkQuotaSettled(ctx context.Context, id string) error { return nil }

func (f *fakeStore) ListTerminalExperimentIDsWithPendingReservation(ctx context.Context) ([]string, error) {
	var ids []string
	for id, present := range f.reservations {
		if present {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStore) DeletePendingReservation(ctx context.Context, id string) error {
	if f.deleteErrs[id] > 0 {
		f.deleteErrs[id]--
		return errors.New("transient postgres error")
	}
	f.reservations[id] = false
	return nil
}

// TestReconcilePendingReservationsRetriesAfterTransientFailure reproduces the second leak
// mechanism: every terminal-transition path (onFinished, onStuckPending, controller eviction/
// cancel, checkQuotaExhaustion) releases a pending_capacity_reservations row inline, but only as
// a single best-effort attempt with no retry of its own — a transient Postgres error there used
// to leave the row stuck forever, since nothing else ever looks at that table again once an
// experiment leaves RUNNING (see loop_tick.go's tick(), which keeps subtracting it from live
// capacity on every admission pass indefinitely). This test proves the settlement reconciler's
// periodic sweep (reconcilePendingReservations) now retries and eventually clears it.
func TestReconcilePendingReservationsRetriesAfterTransientFailure(t *testing.T) {
	store := &fakeStore{
		reservations: map[string]bool{"exp-1": true},
		deleteErrs:   map[string]int{"exp-1": 1}, // fails once, succeeds on retry
	}
	settler := New(nil, "", time.Minute, time.Second, time.Hour)
	r := NewReconciler(store, settler, time.Second, zap.NewNop())

	// First sweep: delete fails transiently — reservation must still be present afterward.
	r.reconcileOnce(context.Background())
	if !store.reservations["exp-1"] {
		t.Fatalf("expected reservation to still be present after a transient delete failure")
	}

	// Second sweep (the reconciler's retry): delete succeeds this time.
	r.reconcileOnce(context.Background())
	if store.reservations["exp-1"] {
		t.Fatalf("expected reservation to be released after the reconciler retried the delete")
	}
}

// TestReconcilePendingReservationsNoopWhenNoneOutstanding is a sanity check that a clean sweep
// (no terminal experiment holding a stray reservation) does nothing and reports no error.
func TestReconcilePendingReservationsNoopWhenNoneOutstanding(t *testing.T) {
	store := &fakeStore{reservations: map[string]bool{}, deleteErrs: map[string]int{}}
	settler := New(nil, "", time.Minute, time.Second, time.Hour)
	r := NewReconciler(store, settler, time.Second, zap.NewNop())
	r.reconcileOnce(context.Background())
}
