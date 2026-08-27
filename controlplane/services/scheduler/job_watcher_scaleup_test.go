package scheduler

import (
	"context"
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
}

func (b *scaleUpBackend) PollJobPhase(context.Context, *domain.Experiment) (workload.JobPhase, error) {
	return b.phase, nil
}
func (b *scaleUpBackend) GetAdmittedAcceleratorType(context.Context, *domain.Experiment) (domain.AcceleratorType, error) {
	return b.admittedType, nil
}
func (b *scaleUpBackend) PollPhaseDetail(context.Context, *domain.Experiment) (reason, message string, restartCount, scheduledNodes int32, schedulingReason string, found bool, err error) {
	found = true
	if b.phaseDetailFound != nil {
		found = *b.phaseDetailFound
	}
	return "", "", 0, b.scheduledNodes, b.schedulingReason, found, nil
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
		Job:         domain.JobSpec{NumNodes: nodes},
		SubmittedAt: &at,
	}
}

func TestScaleUpDeadlinePastTimeoutEvictsWithFailover(t *testing.T) {
	store := &lifecycleStore{}
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
	// A scale-up-timeout failover requeues for free (triedClusterID set) rather than terminally
	// evicting — see ResolveTermination's doc comment.
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want [scale_up_timeout]", store.requeuedReasons)
	}
	if len(store.triedClusters) != 1 || store.triedClusters[0] != "cid-a" {
		t.Fatalf("triedClusters = %v, want [cid-a]", store.triedClusters)
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
	store := &lifecycleStore{}
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
	store := &lifecycleStore{}
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
	exp.Status = domain.StatusRunning

	if err := w.reconcileOne(context.Background(), exp, backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeuedReasons = %v, want a partial-gang eviction even though phase is Running", store.requeuedReasons)
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

func TestScaleUpDeadlineInfraRequeueCountUntouchedOnFailover(t *testing.T) {
	store := &lifecycleStore{infraCeiling: 3}
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
	if store.infraRequeues != 0 {
		t.Fatalf("infra_requeue_count incremented on scale-up failover: got %d, want 0", store.infraRequeues)
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
	store := &lifecycleStore{}
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
