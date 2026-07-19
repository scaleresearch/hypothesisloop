package scheduler

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// fakeJobStatusStore is a minimal in-memory JobStatusStore double sufficient to exercise
// onFinished's transition-race behaviour without a real Postgres instance.
type fakeJobStatusStore struct {
	status               domain.ExperimentStatus
	reservationExists    bool
	transitionStatusCall func(from, to domain.ExperimentStatus) (bool, error)
}

func (f *fakeJobStatusStore) ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return nil, nil
}
func (f *fakeJobStatusStore) UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error {
	f.status = status
	return nil
}
func (f *fakeJobStatusStore) MarkStarted(ctx context.Context, id string) (bool, error) {
	return false, nil
}
func (f *fakeJobStatusStore) TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error) {
	if f.transitionStatusCall != nil {
		return f.transitionStatusCall(from, to)
	}
	if f.status != from {
		return false, nil
	}
	f.status = to
	return true, nil
}
func (f *fakeJobStatusStore) TransitionStatusFromNonTerminal(ctx context.Context, id string, to domain.ExperimentStatus) (bool, error) {
	switch f.status {
	case domain.StatusCompleted, domain.StatusFailed, domain.StatusEvicted:
		return false, nil
	}
	f.status = to
	return true, nil
}
func (f *fakeJobStatusStore) UpdateAdmittedFlavor(ctx context.Context, id string, acceleratorType domain.AcceleratorType, estimatedCostAccH float64) error {
	return nil
}
func (f *fakeJobStatusStore) UpdateEvictionReason(ctx context.Context, id, reason string) error {
	return nil
}
func (f *fakeJobStatusStore) MarkQuotaSettled(ctx context.Context, id string) error { return nil }
func (f *fakeJobStatusStore) DeletePendingReservation(ctx context.Context, id string) error {
	f.reservationExists = false
	return nil
}

// TestOnFinishedReleasesReservationEvenWhenTransitionRaceIsLost reproduces the leak this fix
// closes: a watch goroutine that has fallen out of sync with the DB's current status (e.g. a
// stale watcher left over from before a preemption/resubmission cycle re-used the same
// deterministic backend job name — see workload.jobName) must still release any pending
// reservation when the backend reports a terminal phase, even though its own attempt to
// transition RUNNING -> COMPLETED loses the race because the row is no longer RUNNING. Before
// this fix, DeletePendingReservation was only reached after a *won* transition, so this exact
// case left the reservation permanently stuck (see pending_capacity_reservations).
func TestOnFinishedReleasesReservationEvenWhenTransitionRaceIsLost(t *testing.T) {
	store := &fakeJobStatusStore{
		status:            domain.StatusSubmitted, // not RUNNING: the transition below must lose
		reservationExists: true,
	}
	w := NewJobWatcher(store, nil, zap.NewNop())

	exp := &domain.Experiment{ID: "exp-1"}
	// startedAt non-zero forces the TransitionStatus(RUNNING, ...) branch, which will fail
	// since the fake store's current status is SUBMITTED, not RUNNING.
	w.onFinished(context.Background(), exp, true, time.Now())

	if store.reservationExists {
		t.Fatal("onFinished left the pending reservation in place after losing the transition race — leak reproduced")
	}
}
