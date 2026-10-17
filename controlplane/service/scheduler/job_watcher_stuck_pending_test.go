package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// settingsStore is lifecycleStore with clusters' settings and a list of SUBMITTED experiments.
type settingsStore struct {
	*lifecycleStore
	settings    map[string]*domain.ClusterSettings
	settingsErr error
	submitted   []*domain.Experiment
	running     []*domain.Experiment
}

func (s *settingsStore) GetClusterSettings(_ context.Context, clusterID string) (*domain.ClusterSettings, error) {
	return s.settings[clusterID], s.settingsErr
}

func (s *settingsStore) ListExperimentsWithStatus(_ context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	if status == domain.StatusSubmitted {
		return s.submitted, nil
	}
	return s.running, nil
}

// unobservableBackend runs the job on a type the metrics store cannot yet name.
type unobservableBackend struct{ *scaleUpBackend }

func (unobservableBackend) GetAdmittedAcceleratorType(context.Context, *domain.Experiment) (domain.AcceleratorType, bool, error) {
	return "", false, nil
}

// idsBackend is scaleUpBackend with a cluster id read that can fail.
type idsBackend struct {
	*scaleUpBackend
	idsErr error
}

func (b idsBackend) GetClusterIDs(context.Context) (map[string]string, error) {
	return b.clusterIDs, b.idsErr
}

var clusterA = map[string]string{"cluster-a": "cid-a"}

func stuckPendingOverride(seconds int) map[string]*domain.ClusterSettings {
	return map[string]*domain.ClusterSettings{"cid-a": {StuckPendingTimeoutSeconds: &seconds}}
}

var errPhase = errors.New("no snapshot")

// A cluster of rented instances boots slowly. Its own stuck-pending timeout replaces the global
// one in both directions, for every way a job can fail to reach RUNNING.
func TestAClustersOwnStuckPendingTimeoutReplacesTheGlobalOne(t *testing.T) {
	const global = 5 * time.Minute
	ways := map[string]JobReports{
		"never placed":                       &scaleUpBackend{phase: domain.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA},
		"placed, then gone":                  &scaleUpBackend{phase: domain.JobPhaseGone, scheduledNodes: 1, clusterIDs: clusterA},
		"placed, phase unreadable":           &scaleUpBackend{phase: domain.JobPhasePending, scheduledNodes: 1, phaseErr: errPhase, clusterIDs: clusterA},
		"running on a type not yet observed": unobservableBackend{&scaleUpBackend{phase: domain.JobPhaseRunning, scheduledNodes: 1, clusterIDs: clusterA}},
	}
	for way, backend := range ways {
		for name, tc := range map[string]struct {
			settings map[string]*domain.ClusterSettings
			waited   time.Duration
			evicted  bool
		}{
			"no row: global applies, past it":          {nil, 6 * time.Minute, true},
			"no row: global applies, within it":        {nil, 4 * time.Minute, false},
			"longer: past the global, within its own":  {stuckPendingOverride(900), 6 * time.Minute, false},
			"longer: past its own":                     {stuckPendingOverride(900), 16 * time.Minute, true},
			"shorter: within the global, past its own": {stuckPendingOverride(60), 2 * time.Minute, true},
			"another cluster's row":                    {map[string]*domain.ClusterSettings{"cid-b": stuckPendingOverride(900)["cid-a"]}, 6 * time.Minute, true},
		} {
			t.Run(way+"/"+name, func(t *testing.T) {
				store := &settingsStore{lifecycleStore: &lifecycleStore{}, settings: tc.settings}
				w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(global)
				exp := scaleUpExperiment(1, tc.waited)
				exp.AcceleratorCount = 1

				err := w.reconcileOne(context.Background(), exp, nil, clusterA)
				if err != nil && !errors.Is(err, errPhase) {
					t.Fatal(err)
				}
				if evicted := len(store.terminalReasons) == 1; evicted != tc.evicted {
					t.Fatalf("terminalReasons = %v, want evicted=%v", store.terminalReasons, tc.evicted)
				}
			})
		}
	}
}

// A runtime that rents machines reports first that the job waits for one and then that the one
// it rented is starting. Each is a wait of its own under the cluster's timeout, counted from the
// runtime's first report of it, so a machine found late is given its time to start. Going back
// to a wait already reported does not start it again.
func TestAStuckPendingDeadlineCountsFromTheWaitTheRuntimeReports(t *testing.T) {
	const timeout = 5 * time.Minute
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	for name, tc := range map[string]struct {
		reason        string
		firstReported map[string]time.Time
		phase         domain.JobPhase
		evicted       bool
	}{
		"machine rented late and starting":          {domain.PhaseReasonInstanceStarting, map[string]time.Time{domain.PhaseReasonWaitingForOffer: ago(7 * time.Minute), domain.PhaseReasonInstanceStarting: ago(2 * time.Minute)}, domain.JobPhasePending, false},
		"its machine is being replaced":             {domain.PhaseReasonInstanceStarting, map[string]time.Time{domain.PhaseReasonInstanceStarting: ago(2 * time.Minute)}, domain.JobPhaseGone, false},
		"starting for longer than the timeout":      {domain.PhaseReasonInstanceStarting, map[string]time.Time{domain.PhaseReasonInstanceStarting: ago(6 * time.Minute)}, domain.JobPhasePending, true},
		"back to a wait first reported long ago":    {domain.PhaseReasonWaitingForOffer, map[string]time.Time{domain.PhaseReasonWaitingForOffer: ago(7 * time.Minute), domain.PhaseReasonInstanceStarting: ago(2 * time.Minute)}, domain.JobPhasePending, true},
		"no reason reported: counts from admission": {"", map[string]time.Time{"": ago(time.Minute)}, domain.JobPhasePending, true},
	} {
		t.Run(name, func(t *testing.T) {
			store := &settingsStore{lifecycleStore: &lifecycleStore{}}
			backend := &scaleUpBackend{phase: tc.phase, scheduledNodes: 0, clusterIDs: clusterA, reason: tc.reason, firstReported: tc.firstReported}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(timeout)

			if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 8*time.Minute), nil, clusterA); err != nil {
				t.Fatal(err)
			}
			if evicted := len(store.terminalReasons) == 1; evicted != tc.evicted {
				t.Fatalf("terminalReasons = %v, want evicted=%v", store.terminalReasons, tc.evicted)
			}
		})
	}
}

// The scale-up deadline of an autoscaler cluster is a separate setting with its own failover. A
// stuck-pending override on the same cluster does not stretch it.
func TestAStuckPendingOverrideLeavesTheScaleUpDeadlineAlone(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{infraCeiling: 1}, settings: stuckPendingOverride(900)}
	backend := &scaleUpBackend{
		phase: domain.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(5 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 2*time.Minute), backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || domain.EvictionReason(store.requeuedReasons[0]).Code() != domain.EvictionScaleUpTimeout {
		t.Fatalf("requeued = %v, terminal = %v, want one scale_up_timeout failover", store.requeuedReasons, store.terminalReasons)
	}
}

// A cluster with no fresh heartbeat reports no id, and its settings are keyed by that id. Its
// jobs are not judged by the global timeout meanwhile: the cluster may give them longer, which is
// the whole point of the setting. The decision waits for a pass in which the cluster reports, and
// a cluster that stays away is the controller's to act on.
func TestAJobOnAClusterReportingNoIDIsNotJudgedThisPass(t *testing.T) {
	for way, backend := range map[string]*scaleUpBackend{
		"never placed":      {phase: domain.JobPhasePending, scheduledNodes: 0},
		"placed, then gone": {phase: domain.JobPhaseGone, scheduledNodes: 1},
	} {
		for _, waited := range []time.Duration{6 * time.Minute, time.Hour} {
			t.Run(way+"/"+waited.String(), func(t *testing.T) {
				store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("must not be read")}
				w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
				// Another cluster reports; the job's own does not.
				ids := map[string]string{"cluster-b": "cid-b"}

				if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, waited), nil, ids); err != nil {
					t.Fatal(err)
				}
				if store.transitioned {
					t.Fatalf("evicted by the global timeout: %v", store.terminalReasons)
				}
			})
		}
	}
}

// The whole pass: a cluster that set 900 seconds and has gone quiet. Its job, waiting past the
// global 300, is left alone, and is evicted by its own timeout once the cluster reports again.
func TestADisconnectedClustersOverrideIsNotReplacedByTheGlobalTimeout(t *testing.T) {
	store := &settingsStore{
		lifecycleStore: &lifecycleStore{}, settings: stuckPendingOverride(900),
		submitted: []*domain.Experiment{scaleUpExperiment(1, 6*time.Minute)},
	}
	backend := &scaleUpBackend{phase: domain.JobPhasePending, scheduledNodes: 0, clusterIDs: map[string]string{}}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
	if err := w.scanAndWatch(context.Background()); err != nil || store.transitioned {
		t.Fatalf("disconnected: err %v, evicted %v", err, store.terminalReasons)
	}

	backend.clusterIDs = clusterA
	if err := w.scanAndWatch(context.Background()); err != nil || store.transitioned {
		t.Fatalf("reconnected, inside its own 900 s: err %v, evicted %v", err, store.terminalReasons)
	}
	store.submitted = []*domain.Experiment{scaleUpExperiment(1, 16*time.Minute)}
	if err := w.scanAndWatch(context.Background()); err != nil || len(store.terminalReasons) != 1 {
		t.Fatalf("reconnected, past its own 900 s: err %v, evicted %v", err, store.terminalReasons)
	}
}

// A deadline that cannot be read is not replaced by the global one: a job's cluster settings that
// cannot be read skip that job's decision this pass, and cluster ids that cannot be read end the
// pass, instead of evicting a job on a timeout its cluster may not have.
func TestAnUnreadableTimeoutEvictsNothing(t *testing.T) {
	unreadable := errors.New("unavailable")
	for way, phase := range map[string]*scaleUpBackend{
		"never placed":      {phase: domain.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA},
		"placed, then gone": {phase: domain.JobPhaseGone, scheduledNodes: 1, clusterIDs: clusterA},
	} {
		t.Run(way+"/cluster settings", func(t *testing.T) {
			store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: unreadable}
			w := NewJobWatcher(store, phase, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

			err := w.reconcileOne(context.Background(), scaleUpExperiment(1, time.Hour), nil, clusterA)
			if store.transitioned {
				t.Fatalf("evicted on an unreadable timeout (err = %v)", err)
			}
			// A placed job has nothing left to check, so the failed read is its result. An
			// unplaced one goes on to its phase, which here has nothing to add.
			if placed := phase.scheduledNodes > 0; placed != (err != nil) {
				t.Fatalf("err = %v for a job with scheduledNodes %d", err, phase.scheduledNodes)
			}
		})
		t.Run(way+"/cluster ids", func(t *testing.T) {
			store := &settingsStore{lifecycleStore: &lifecycleStore{}, submitted: []*domain.Experiment{scaleUpExperiment(1, time.Hour)}}
			w := NewJobWatcher(store, idsBackend{scaleUpBackend: phase, idsErr: unreadable}, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

			if err := w.scanAndWatch(context.Background()); err == nil || store.transitioned {
				t.Fatalf("err = %v, evicted %v, want the pass ended and nothing evicted", err, store.terminalReasons)
			}
		})
	}
}

// A job that has finished is read as finished whatever the state of the settings: no
// stuck-pending decision is pending for it, so nothing is read on its behalf.
func TestAFinishedJobIsTerminalisedWithUnreadableSettings(t *testing.T) {
	unreadable := errors.New("unavailable")
	for name, tc := range map[string]struct {
		scheduledNodes int32
		status         domain.ExperimentStatus
	}{
		"running":                 {1, domain.StatusRunning},
		"submitted, placed":       {1, domain.StatusSubmitted},
		"submitted, never placed": {0, domain.StatusSubmitted},
	} {
		t.Run(name, func(t *testing.T) {
			store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: unreadable}
			backend := idsBackend{scaleUpBackend: &scaleUpBackend{phase: domain.JobPhaseSucceeded, scheduledNodes: tc.scheduledNodes, clusterIDs: clusterA}, idsErr: unreadable}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
			exp := scaleUpExperiment(1, time.Minute)
			exp.Status = tc.status

			if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil || !store.transitioned {
				t.Fatalf("err = %v, transitioned = %v", err, store.transitioned)
			}
		})
	}
}

// A RUNNING job is past every stuck-pending check, so its cluster's settings are not read for it.
func TestARunningJobDoesNotReadItsClustersSettings(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("must not be read")}
	backend := idsBackend{scaleUpBackend: &scaleUpBackend{phase: domain.JobPhaseRunning, scheduledNodes: 1}, idsErr: errors.New("must not be read")}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
	exp := scaleUpExperiment(1, time.Hour)
	exp.Status = domain.StatusRunning

	if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil || store.transitioned {
		t.Fatalf("err = %v, transitioned = %v", err, store.transitioned)
	}
}

// The whole pass, on a deployment that never opted into the scale-up deadline: the override is
// still applied.
func TestAPassAppliesTheOverrideWithoutTheScaleUpOptIn(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]*domain.ClusterSettings
		evicted  bool
	}{
		"its own longer timeout": {stuckPendingOverride(900), false},
		"no row":                 {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			store := &settingsStore{
				lifecycleStore: &lifecycleStore{}, settings: tc.settings,
				submitted: []*domain.Experiment{scaleUpExperiment(1, 6*time.Minute)},
			}
			backend := &scaleUpBackend{phase: domain.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

			if err := w.scanAndWatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if evicted := len(store.terminalReasons) == 1; evicted != tc.evicted {
				t.Fatalf("terminalReasons = %v, want evicted=%v", store.terminalReasons, tc.evicted)
			}
		})
	}
}

type phaseByJobBackend struct {
	idsBackend
	phases map[string]domain.JobPhase
}

func (b *phaseByJobBackend) LatestJobPhase(_ context.Context, exp *domain.Experiment) (domain.JobPhase, bool, error) {
	return b.phases[exp.ID], true, nil
}
