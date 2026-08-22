package scheduler

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type lifecycleStore struct {
	transitioned bool
	// requeueBudget is what the fake pretends the row's remaining retry budget is: RequeueForRetry
	// succeeds while it is positive, mirroring the real store's "attempt_count < max" WHERE clause.
	requeueBudget int
	requeueCalls  []int
}

func (*lifecycleStore) ListExperimentsWithStatus(context.Context, domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return nil, nil
}
func (*lifecycleStore) UpdateExperimentStatus(context.Context, string, domain.ExperimentStatus) error {
	return nil
}
func (*lifecycleStore) MarkStarted(context.Context, string) (bool, error) { return true, nil }
func (s *lifecycleStore) TransitionStatus(context.Context, string, domain.ExperimentStatus, domain.ExperimentStatus) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (s *lifecycleStore) TransitionStatusFromNonTerminal(context.Context, string, domain.ExperimentStatus) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (s *lifecycleStore) TransitionTerminal(context.Context, string, domain.ExperimentStatus, domain.ExperimentStatus, string) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (*lifecycleStore) UpdateEvictionReason(context.Context, string, string) error { return nil }
func (*lifecycleStore) MarkQuotaSettled(context.Context, string) error             { return nil }
func (s *lifecycleStore) RequeueForRetry(_ context.Context, _ string, maxAttemptsBefore int) (bool, error) {
	s.requeueCalls = append(s.requeueCalls, maxAttemptsBefore)
	if s.requeueBudget <= 0 {
		return false, nil
	}
	s.requeueBudget--
	return true, nil
}

func TestTerminalObservationRequiresMetricsBeforeLifecycleTransition(t *testing.T) {
	store := &lifecycleStore{}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())
	exp := &domain.Experiment{ID: "exp-1", Status: domain.StatusSubmitted}

	if err := w.onFinished(context.Background(), exp, true); err == nil {
		t.Fatal("onFinished accepted a terminal observation without authoritative metrics")
	}
	if store.transitioned {
		t.Fatal("onFinished changed PostgreSQL desired state after metrics resolution failed")
	}
}

type noopSettler struct{}

func (noopSettler) Settle(context.Context, *domain.Experiment) error { return nil }

// gangExperiment is a RUNNING multi-node job — the only shape whose retry the control plane owns.
func gangExperiment(nodes, maxRetries int) *domain.Experiment {
	return &domain.Experiment{
		ID: "gang-1", Status: domain.StatusRunning,
		Job: domain.JobSpec{NumNodes: nodes, MaxRetries: &maxRetries},
	}
}

// A rank failure fails the whole Job (the runtime's FailJob policy), which is terminal in
// Kubernetes — BackoffLimit cannot restart a gang. So max_retries for a gang is spent here or
// nowhere, and a failed gang with budget left must go back to QUEUED rather than staying FAILED.
func TestFailedGangIsRequeuedWhileRetryBudgetRemains(t *testing.T) {
	store := &lifecycleStore{requeueBudget: 1}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	if err := w.onFinished(context.Background(), gangExperiment(3, 1), false); err != nil {
		t.Fatal(err)
	}
	if len(store.requeueCalls) != 1 {
		t.Fatalf("RequeueForRetry called %d times, want 1 — a failed gang with budget left was not retried", len(store.requeueCalls))
	}
	if store.requeueCalls[0] != 1 {
		t.Errorf("RequeueForRetry got maxAttemptsBefore=%d, want 1 (job.max_retries)", store.requeueCalls[0])
	}
}

// The budget is the store's to enforce, in the same UPDATE that spends it. When it reports the
// row was not requeued, the job is done and must settle — a watcher that skipped settlement here
// would leave the last attempt's hours unbilled.
func TestFailedGangWithNoBudgetLeftSettlesInsteadOfRetrying(t *testing.T) {
	store := &lifecycleStore{requeueBudget: 0}
	settler := &countingSettler{}
	w := NewJobWatcher(store, nil, settler, zap.NewNop())

	if err := w.onFinished(context.Background(), gangExperiment(3, 1), false); err != nil {
		t.Fatal(err)
	}
	if len(store.requeueCalls) != 1 {
		t.Fatalf("RequeueForRetry called %d times, want 1", len(store.requeueCalls))
	}
	if settler.settles == 0 {
		t.Fatal("an exhausted gang did not settle — its final attempt's hours would never be billed")
	}
}

// A single-pod job's retries are the runtime's BackoffLimit, and by the time JobPhaseFailed
// reaches here that budget is already spent. Retrying again in the control plane would be a
// second retry authority stacked on the first, silently doubling max_retries.
func TestFailedSingleNodeJobIsNotRequeuedByTheControlPlane(t *testing.T) {
	store := &lifecycleStore{requeueBudget: 5}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	if err := w.onFinished(context.Background(), gangExperiment(1, 3), false); err != nil {
		t.Fatal(err)
	}
	if len(store.requeueCalls) != 0 {
		t.Fatalf("RequeueForRetry called for a single-node job (%d times) — the runtime already retried it", len(store.requeueCalls))
	}
}

// A completed gang is not a failed one. Nothing about max_retries applies to success.
func TestSucceededGangIsNeverRequeued(t *testing.T) {
	store := &lifecycleStore{requeueBudget: 5}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	if err := w.onFinished(context.Background(), gangExperiment(3, 3), true); err != nil {
		t.Fatal(err)
	}
	if len(store.requeueCalls) != 0 {
		t.Fatalf("RequeueForRetry called for a COMPLETED gang (%d times)", len(store.requeueCalls))
	}
}

type countingSettler struct{ settles int }

func (s *countingSettler) Settle(context.Context, *domain.Experiment) error { s.settles++; return nil }
