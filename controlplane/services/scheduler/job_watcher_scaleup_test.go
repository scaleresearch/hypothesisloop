package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// scaleUpBackend is the minimal JobBackendClient fake needed to drive reconcileOne/
// checkScaleUpDeadline through the watcher's public entry points.
type scaleUpBackend struct {
	phase             workload.JobPhase
	scheduledNodes    int32
	schedulingReason  string
	autoscalerEnabled map[string]bool
	clusterIDs        map[string]string
	admittedType      domain.AcceleratorType
	// phaseDetailFound defaults to true (found) when the zero value is used by every existing
	// test; set to false to simulate no phase-detail row ever having been written for this
	// experiment's current cluster/attempt (see TestScaleUpDeadlineAppliesWhenPhaseDetailNeverFound).
	phaseDetailFound *bool
	phaseErr         error
	// phaseUnreported is a cluster with no status snapshot inside the liveness window.
	phaseUnreported bool
	// reason is what the runtime reports the job waits for, and firstReported when the attempt
	// first reported each reason.
	reason        string
	message       string
	firstReported map[string]time.Time
	logTail       []string
}

func (b *scaleUpBackend) LogTail(_ context.Context, _ *domain.Experiment, n int) ([]string, error) {
	return b.logTail[max(0, len(b.logTail)-n):], nil
}

func (b *scaleUpBackend) PollJobPhase(context.Context, *domain.Experiment) (workload.JobPhase, bool, error) {
	return b.phase, !b.phaseUnreported, b.phaseErr
}
func (b *scaleUpBackend) GetAdmittedAcceleratorType(context.Context, *domain.Experiment) (domain.AcceleratorType, bool, error) {
	return b.admittedType, true, nil
}
func (b *scaleUpBackend) PollPhaseDetail(context.Context, *domain.Experiment) (reason, message string, restartCount, scheduledNodes int32, schedulingReason string, found bool, err error) {
	found = true
	if b.phaseDetailFound != nil {
		found = *b.phaseDetailFound
	}
	return b.reason, b.message, 0, b.scheduledNodes, b.schedulingReason, found, nil
}
func (b *scaleUpBackend) PhaseReportedSince(_ context.Context, _ *domain.Experiment, reason string) (time.Time, bool, error) {
	since, found := b.firstReported[reason]
	return since, found, nil
}
func (b *scaleUpBackend) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return b.autoscalerEnabled, nil
}
func (b *scaleUpBackend) GetClusterIDs(context.Context) (map[string]string, error) {
	return b.clusterIDs, nil
}

func scaleUpExperiment(nodes int, submittedAgo time.Duration) *domain.Experiment {
	at := time.Now().Add(-submittedAgo)
	return &domain.Experiment{
		ID: "exp-su", Status: domain.StatusSubmitted, ClusterName: "cluster-a",
		Job:         domain.JobSpec{Image: "registry.test/job:1", NumNodes: nodes},
		SubmittedAt: &at,
	}
}

// A scale-up that timed out is the environment's fault: the job goes back to the queue for free
// while its infrastructure allowance lasts, and each such requeue spends one of it. The store
// records the cluster in the job's tried list in the same write.
func TestScaleUpDeadlinePastTimeoutRequeuesOnTheInfrastructureAllowance(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 1}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)
	exp := scaleUpExperiment(1, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want [scale_up_timeout]", store.requeuedReasons)
	}
	if store.infraRequeues != 1 || len(store.terminalReasons) != 0 {
		t.Fatalf("infra requeues spent = %d, terminal = %v, want one spent and the job still alive", store.infraRequeues, store.terminalReasons)
	}
}

// The allowance is the bound on a job going round its clusters: with it spent, a scale-up that
// times out ends the job, saying why.
func TestScaleUpDeadlineWithTheAllowanceSpentEndsTheJob(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 2, infraRequeues: 2}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)
	exp := scaleUpExperiment(1, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 0 || len(store.terminalReasons) != 1 || store.terminalReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeued %v, terminal %v, want the job ended as scale_up_timeout", store.requeuedReasons, store.terminalReasons)
	}
}

func TestScaleUpDeadlineNotYetPastDoesNotEvict(t *testing.T) {
	store := &lifecycleStore{}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(5 * time.Minute)
	exp := scaleUpExperiment(1, 1*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 0 {
		t.Fatalf("terminalReasons = %v, want none — deadline not yet passed", store.terminalReasons)
	}
}

func TestScaleUpEarlyExitOnTerminalSchedulingReason(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 1}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		schedulingReason:  "NotTriggerScaleUp: max node group size reached",
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(10 * time.Minute)
	exp := scaleUpExperiment(1, 1*time.Second)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want early exit with scale_up_timeout", store.requeuedReasons)
	}
}

func TestScaleUpDeadlinePhaseIndependentPartialGangWhileRunning(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 1}
	backend := &scaleUpBackend{
		// Aggregate phase already reads Running (one landed rank), but only 2 of 3 ranks bound.
		phase: workload.JobPhaseRunning, scheduledNodes: 2,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)
	exp := scaleUpExperiment(3, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want a partial-gang eviction even though phase is Running", store.requeuedReasons)
	}
}

// One landed rank reads as Running. Marking the job RUNNING there would take it out from under
// the placement deadline with part of the gang never placed.
func TestAPartialGangStaysSubmittedWhileItsPhaseReadsRunning(t *testing.T) {
	store := &lifecycleStore{}
	backend := &scaleUpBackend{phase: workload.JobPhaseRunning, scheduledNodes: 2}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(10 * time.Minute)

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(3, time.Minute), nil, nil); err != nil {
		t.Fatal(err)
	}
	if store.started != 0 || store.transitioned {
		t.Fatalf("started = %d, transitioned = %v, want the gang left SUBMITTED", store.started, store.transitioned)
	}
}

// The placement deadline counts from submitted_at. A job that has run for hours and shows one
// pod unscheduled, because it is being recreated, is past placement.
func TestARunningJobIsNotHeldToThePlacementDeadline(t *testing.T) {
	for name, found := range map[string]bool{"one rank unscheduled": true, "no phase detail": false} {
		t.Run(name, func(t *testing.T) {
			store := &lifecycleStore{}
			backend := &scaleUpBackend{
				phase: workload.JobPhaseRunning, scheduledNodes: 2, phaseDetailFound: &found,
				autoscalerEnabled: map[string]bool{"cluster-a": true},
				clusterIDs:        map[string]string{"cluster-a": "cid-a"},
			}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
				WithStuckPendingTimeout(10 * time.Minute).
				WithScaleUpTimeout(1 * time.Minute)
			exp := scaleUpExperiment(3, 5*time.Hour)
			exp.Status = domain.StatusRunning

			if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
				t.Fatal(err)
			}
			if store.transitioned {
				t.Fatalf("a RUNNING job was evicted: requeued %v, terminal %v", store.requeuedReasons, store.terminalReasons)
			}
		})
	}
}

func TestScaleUpDeadlineNonAutoscalerClusterFallsBackToStuckPending(t *testing.T) {
	store := &lifecycleStore{}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{}, // cluster-a not autoscaler-enabled
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(1 * time.Minute).
		WithScaleUpTimeout(10 * time.Minute) // would NOT be past if this timeout applied
	exp := scaleUpExperiment(1, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != string(domain.EvictionStuckPending) {
		t.Fatalf("terminalReasons = %v, want [stuck_pending] on a non-autoscaler cluster", store.terminalReasons)
	}
}

func TestScaleUpDeadlineDisabledWithoutOptIn(t *testing.T) {
	store := &lifecycleStore{}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	// WithScaleUpTimeout never called: falls back to stuckPendingTimeout for every cluster.
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute)
	exp := scaleUpExperiment(1, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 0 {
		t.Fatalf("terminalReasons = %v, want none — stuckPendingTimeout not yet passed", store.terminalReasons)
	}
}

// A job whose phase-detail row was never written (quota-blocked pod, or an agent whose
// PollPhaseDetail has errored every poll) must still be bounded by the placement deadline — not
// held forever. found=false used to be read as "we don't know, skip the check entirely", which
// made such a job immortal: this proves not-found now degenerates to "not placed yet" and the
// deadline still fires once it has genuinely passed.
func TestScaleUpDeadlineAppliesWhenPhaseDetailNeverFound(t *testing.T) {
	notFound := false
	store := &lifecycleStore{infraCeiling: 1}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, phaseDetailFound: &notFound,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)
	exp := scaleUpExperiment(1, 2*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want [scale_up_timeout] — a never-reported job must still hit the deadline", store.requeuedReasons)
	}
}

// The mirror case: not yet past the deadline, and no phase-detail row has arrived yet. This must
// not evict early just because found=false — only once the deadline has actually passed.
func TestScaleUpDeadlineNotYetPastWhenPhaseDetailNeverFound(t *testing.T) {
	notFound := false
	store := &lifecycleStore{}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, phaseDetailFound: &notFound,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(10 * time.Minute).
		WithScaleUpTimeout(5 * time.Minute)
	exp := scaleUpExperiment(1, 1*time.Minute)

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 0 || len(store.requeuedReasons) != 0 {
		t.Fatalf("terminalReasons=%v requeuedReasons=%v, want none — deadline not yet passed", store.terminalReasons, store.requeuedReasons)
	}
}

func TestIsTerminalSchedulingRefusal(t *testing.T) {
	cases := map[string]bool{
		"":                           false,
		"TriggeredScaleUp":           false,
		"still waiting for capacity": false,
		"NotTriggerScaleUp: max node group size reached": true,
		"pod is incompatible with nodepool defaults":     true,
		"nodeclaim failure: insufficient capacity":       true,
	}
	for reason, want := range cases {
		if got := isTerminalSchedulingRefusal(reason); got != want {
			t.Errorf("isTerminalSchedulingRefusal(%q) = %v, want %v", reason, got, want)
		}
	}
}

// A requeued job re-admitted while its previous generation checkpoints has no workload until that
// pod is gone, which may take the whole window.
func TestAJobIsNotStuckPendingWhileItsCheckpointWindowMayStillBeRunning(t *testing.T) {
	window := int64(600)
	for _, tc := range []struct {
		submittedAgo time.Duration
		wantEvicted  bool
	}{
		{submittedAgo: 12 * time.Minute, wantEvicted: false},
		{submittedAgo: 16 * time.Minute, wantEvicted: true},
	} {
		for _, phase := range []workload.JobPhase{workload.JobPhaseGone, workload.JobPhasePending} {
			store := &lifecycleStore{}
			found := phase == workload.JobPhasePending
			backend := &scaleUpBackend{phase: phase, phaseDetailFound: &found}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
			exp := scaleUpExperiment(1, tc.submittedAgo)
			exp.ResolvedJob = &domain.JobSpec{Image: "registry.test/job:1", NumNodes: 1, CheckpointGraceSeconds: &window}

			if err := w.reconcileOne(context.Background(), exp, nil, clusterA); err != nil {
				t.Fatal(err)
			}
			if evicted := len(store.requeuedReasons)+len(store.terminalReasons) > 0; evicted != tc.wantEvicted {
				t.Errorf("phase %s, admitted %s ago with a %ds window: evicted = %v, want %v",
					phase, tc.submittedAgo, window, evicted, tc.wantEvicted)
			}
		}
	}
}

// A job whose every rank bound is past the placement deadline. A cluster that stops reporting its
// phase releases the reservation at the admission deadline. A job the runtime reports as pending
// and placed is starting and has the longer start timeout. A cluster with no report yet is where
// every placed job is for a moment, so it is no error; a phase that cannot be read is one, under
// the same deadline.
func TestABoundJobThatNeverStartsIsEvictedAtItsDeadline(t *testing.T) {
	window := int64(600)
	unreadable := errors.New("metrics store unreachable")
	for _, tc := range []struct {
		name         string
		unreported   bool
		phaseErr     error
		submittedAgo time.Duration
		checkpoint   *int64
		status       domain.ExperimentStatus
		wantEvicted  bool
	}{
		{name: "no phase inside the admission deadline", unreported: true, submittedAgo: 2 * time.Minute, status: domain.StatusSubmitted, wantEvicted: false},
		{name: "no phase past the admission deadline", unreported: true, submittedAgo: 6 * time.Minute, status: domain.StatusSubmitted, wantEvicted: true},
		{name: "no phase inside the checkpoint window", unreported: true, submittedAgo: 12 * time.Minute, checkpoint: &window, status: domain.StatusSubmitted, wantEvicted: false},
		{name: "unreadable phase inside the admission deadline", phaseErr: unreadable, submittedAgo: 2 * time.Minute, status: domain.StatusSubmitted, wantEvicted: false},
		{name: "unreadable phase past the admission deadline", phaseErr: unreadable, submittedAgo: 6 * time.Minute, status: domain.StatusSubmitted, wantEvicted: true},
		{name: "pulling its image past the admission deadline", submittedAgo: 20 * time.Minute, status: domain.StatusSubmitted, wantEvicted: false},
		{name: "pending past the start timeout", submittedAgo: 31 * time.Minute, status: domain.StatusSubmitted, wantEvicted: true},
		{name: "pending past the start timeout inside the checkpoint window", submittedAgo: 35 * time.Minute, checkpoint: &window, status: domain.StatusSubmitted, wantEvicted: false},
		{name: "running job with no phase", unreported: true, submittedAgo: time.Hour, status: domain.StatusRunning, wantEvicted: false},
		{name: "running job reading pending", submittedAgo: time.Hour, status: domain.StatusRunning, wantEvicted: false},
	} {
		store := &lifecycleStore{}
		backend := &scaleUpBackend{phase: workload.JobPhasePending, phaseUnreported: tc.unreported, phaseErr: tc.phaseErr, scheduledNodes: 1}
		w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
			WithStuckPendingTimeout(5 * time.Minute).WithStartTimeout(30 * time.Minute)
		exp := scaleUpExperiment(1, tc.submittedAgo)
		exp.Status = tc.status
		exp.ResolvedJob = &domain.JobSpec{Image: "registry.test/job:1", NumNodes: 1, CheckpointGraceSeconds: tc.checkpoint}

		err := w.reconcileOne(context.Background(), exp, nil, clusterA)
		if !errors.Is(err, tc.phaseErr) {
			t.Errorf("%s: reconcile error = %v, want %v", tc.name, err, tc.phaseErr)
		}
		reasons := append(append([]string{}, store.requeuedReasons...), store.terminalReasons...)
		if evicted := len(reasons) > 0; evicted != tc.wantEvicted {
			t.Fatalf("%s: evicted = %v, want %v", tc.name, evicted, tc.wantEvicted)
		}
		if tc.wantEvicted && (domain.EvictionReason(reasons[0]).Code() != domain.EvictionStuckPending || !strings.Contains(reasons[0], ": admitted and not running within ")) {
			t.Errorf("%s: reason = %q, want %q and its sentence", tc.name, reasons[0], domain.EvictionStuckPending)
		}
	}
}
