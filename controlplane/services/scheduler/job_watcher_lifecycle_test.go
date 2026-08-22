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
	// succeeds while it is positive, mirroring the real store's
	// "attempt_count - infra_requeue_count < max" WHERE clause.
	requeueBudget int
	requeueCalls  []int
	// infraRequeues counts what the real store does inside ResolveTermination when the reason
	// names an infrastructure fault, bounded by infraCeiling exactly as the real WHERE clause's
	// "infra_requeue_count < max" is. attempt_count and the retry budget are tracked separately
	// so a test can assert the infrastructure path never touches the latter.
	infraCeiling    int
	infraRequeues   int
	attemptCount    int
	terminalReasons []string
	requeuedReasons []string
	markedSettled   int
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
func (s *lifecycleStore) ResolveTermination(_ context.Context, _ string, _, _ domain.ExperimentStatus, reason string) (domain.Termination, error) {
	s.transitioned = true
	if domain.IsInfrastructureFault(domain.EvictionReason(reason)) && s.infraRequeues < s.infraCeiling {
		s.infraRequeues++
		s.attemptCount++
		s.requeuedReasons = append(s.requeuedReasons, reason)
		return domain.TerminationRequeued, nil
	}
	s.terminalReasons = append(s.terminalReasons, reason)
	return domain.TerminationWritten, nil
}
func (*lifecycleStore) UpdateEvictionReason(context.Context, string, string) error { return nil }
func (s *lifecycleStore) MarkQuotaSettled(context.Context, string) error {
	s.markedSettled++
	return nil
}
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

// The whole point of fault attribution: the platform's output is a ranking of agents, so an
// agent must never be charged an attempt of its own max_retries budget for a node that broke.
// The retry budget and the infrastructure-requeue allowance are separate counters precisely so
// this path cannot touch the former.
func TestAnInfrastructureFaultRequeuesWithoutSpendingARetryAttempt(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3, requeueBudget: 2}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())
	exp := &domain.Experiment{ID: "exp-1", Status: domain.StatusAdmitted}

	w.evictNotYetRunning(context.Background(), exp, domain.EvictionClusterUnreachable)

	if len(store.requeuedReasons) != 1 {
		t.Fatalf("infrastructure requeues = %v, want 1 — the environment's failure ended the job for good", len(store.requeuedReasons))
	}
	if len(store.terminalReasons) != 0 {
		t.Errorf("terminal writes = %v, want 0", len(store.terminalReasons))
	}
	if store.requeueBudget != 2 {
		t.Errorf("remaining retry budget got = %v, want %v", store.requeueBudget, 2)
	}
	if len(store.requeueCalls) != 0 {
		t.Errorf("RequeueForRetry calls got = %v, want %v — an infrastructure fault must not spend max_retries", len(store.requeueCalls), 0)
	}
	// The attempt generation must still advance: a requeue reuses the experiment id, so without
	// it the rebuilt workload is byte-identical to the dead one and the runtime never recreates it.
	if store.attemptCount != 1 {
		t.Errorf("attempt_count got = %v, want %v", store.attemptCount, 1)
	}
}

// A requeued row is not terminal, so marking it settled would tell the settlement reconciler
// this experiment's final usage is written — and the attempt that follows would never be billed.
func TestAnInfrastructureRequeueIsNeverMarkedQuotaSettled(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	w.evictNotYetRunning(context.Background(), &domain.Experiment{ID: "exp-1", Status: domain.StatusAdmitted}, domain.EvictionWorkloadGone)

	if store.markedSettled != 0 {
		t.Errorf("MarkQuotaSettled calls got = %v, want %v", store.markedSettled, 0)
	}
}

// The ceiling exists so a job landing repeatedly on broken hardware stops and says so instead of
// looping forever. It must bind exactly: one requeue below it still goes back to the queue, and
// at it the job stays EVICTED carrying the reason that kept ending it.
func TestInfrastructureRequeuesStopExactlyAtTheConfiguredCeiling(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 2}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		w.evictNotYetRunning(ctx, &domain.Experiment{ID: "exp-1", Status: domain.StatusAdmitted}, domain.EvictionStuckPending)
	}

	if len(store.requeuedReasons) != 2 {
		t.Errorf("free requeues got = %v, want %v", len(store.requeuedReasons), 2)
	}
	if len(store.terminalReasons) != 1 {
		t.Fatalf("terminal writes got = %v, want %v — the job never stopped", len(store.terminalReasons), 1)
	}
	if store.terminalReasons[0] != string(domain.EvictionStuckPending) {
		t.Errorf("terminal reason got = %v, want %v", store.terminalReasons[0], string(domain.EvictionStuckPending))
	}
}

// A policy termination is the platform's own decision and the job was fine, but that is a reason
// to report it separately — not to run it again. Requeuing a stage-cut job would put work back on
// the cluster the ladder just decided to stop paying for.
func TestAPolicyTerminationIsNotRequeued(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	w.evictNotYetRunning(context.Background(), &domain.Experiment{ID: "exp-1", Status: domain.StatusAdmitted}, domain.EvictionStageCut)

	if len(store.requeuedReasons) != 0 {
		t.Errorf("free requeues got = %v, want %v", len(store.requeuedReasons), 0)
	}
	if len(store.terminalReasons) != 1 {
		t.Errorf("terminal writes got = %v, want %v", len(store.terminalReasons), 1)
	}
}

// A workload fault is the agent's own — a bad image reference here. Requeuing it for free would
// hand the agent unlimited attempts at a spec that cannot work, and hide the bug from its record.
func TestAWorkloadTerminationIsNotRequeued(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
	w := NewJobWatcher(store, nil, noopSettler{}, zap.NewNop())

	w.evictNotYetRunning(context.Background(), &domain.Experiment{ID: "exp-1", Status: domain.StatusAdmitted}, domain.EvictionUnschedulable)

	if len(store.requeuedReasons) != 0 {
		t.Errorf("free requeues got = %v, want %v", len(store.requeuedReasons), 0)
	}
	if store.markedSettled != 1 {
		t.Errorf("MarkQuotaSettled calls got = %v, want %v — a terminal workload failure must settle", store.markedSettled, 1)
	}
}
