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

// settingsStore is lifecycleStore with cluster_settings rows and a list of SUBMITTED experiments.
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

func (unobservableBackend) GetAdmittedAcceleratorType(context.Context, *domain.Experiment) (domain.AcceleratorType, error) {
	return "", errors.New("no observation yet")
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
	return map[string]*domain.ClusterSettings{"cid-a": {ClusterID: "cid-a", StuckPendingTimeoutSeconds: &seconds}}
}

var errPhase = errors.New("no snapshot")

// A cluster of rented instances boots slowly. Its own stuck-pending timeout replaces the global
// one in both directions, for every way a job can fail to reach RUNNING.
func TestAClustersOwnStuckPendingTimeoutReplacesTheGlobalOne(t *testing.T) {
	const global = 5 * time.Minute
	ways := map[string]JobBackendClient{
		"never placed":                       &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA},
		"placed, then gone":                  &scaleUpBackend{phase: workload.JobPhaseGone, scheduledNodes: 1, clusterIDs: clusterA},
		"placed, phase unreadable":           &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 1, phaseErr: errPhase, clusterIDs: clusterA},
		"running on a type not yet observed": unobservableBackend{&scaleUpBackend{phase: workload.JobPhaseRunning, scheduledNodes: 1, clusterIDs: clusterA}},
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

				err := w.reconcileOne(context.Background(), exp, nil, nil)
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

// The scale-up deadline of an autoscaler cluster is a separate setting with its own failover. A
// stuck-pending override on the same cluster does not stretch it.
func TestAStuckPendingOverrideLeavesTheScaleUpDeadlineAlone(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settings: stuckPendingOverride(900)}
	backend := &scaleUpBackend{
		phase: workload.JobPhasePending, scheduledNodes: 0,
		autoscalerEnabled: map[string]bool{"cluster-a": true},
		clusterIDs:        map[string]string{"cluster-a": "cid-a"},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).
		WithStuckPendingTimeout(5 * time.Minute).
		WithScaleUpTimeout(1 * time.Minute)

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 2*time.Minute), backend.autoscalerEnabled, backend.clusterIDs); err != nil {
		t.Fatal(err)
	}
	if len(store.requeuedReasons) != 1 || store.requeuedReasons[0] != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("requeued = %v, terminal = %v, want one scale_up_timeout failover", store.requeuedReasons, store.terminalReasons)
	}
}

// Settings are keyed by cluster id. A cluster that has reported none has no row to read, so its
// jobs are judged by the global timeout and the store is not asked.
func TestAClusterWithNoReportedIDIsJudgedByTheGlobalTimeout(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("must not be read")}
	backend := &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0, clusterIDs: map[string]string{"cluster-b": "cid-b"}}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 6*time.Minute), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != string(domain.EvictionStuckPending) {
		t.Fatalf("terminalReasons = %v", store.terminalReasons)
	}
}

// A deadline that cannot be read is not replaced by the global one: that decision is skipped for
// that job this pass, whichever of the two reads failed, instead of evicting it on a timeout its
// cluster may not have.
func TestAnUnreadableTimeoutEvictsNothing(t *testing.T) {
	unreadable := errors.New("unavailable")
	for way, phase := range map[string]*scaleUpBackend{
		"never placed":      {phase: workload.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA},
		"placed, then gone": {phase: workload.JobPhaseGone, scheduledNodes: 1, clusterIDs: clusterA},
	} {
		for name, tc := range map[string]struct {
			settingsErr, idsErr error
		}{
			"cluster settings": {settingsErr: unreadable},
			"cluster ids":      {idsErr: unreadable},
		} {
			t.Run(way+"/"+name, func(t *testing.T) {
				store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: tc.settingsErr}
				w := NewJobWatcher(store, idsBackend{phase, tc.idsErr}, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

				err := w.reconcileOne(context.Background(), scaleUpExperiment(1, time.Hour), nil, nil)
				if store.transitioned {
					t.Fatalf("evicted on an unreadable timeout (err = %v)", err)
				}
				// A placed job has nothing left to check, so the failed read is its result. An
				// unplaced one goes on to its phase, which here has nothing to add.
				if placed := phase.scheduledNodes > 0; placed != (err != nil) {
					t.Fatalf("err = %v for a job with scheduledNodes %d", err, phase.scheduledNodes)
				}
			})
		}
	}
}

// A job that has finished is read as finished whatever the state of the settings: no
// stuck-pending decision is pending for it, so nothing is read on its behalf. The terminal
// transition of a job never observed RUNNING needs the metrics store, which this test does not
// have, so reaching that requirement is the proof the settings did not stand in the way.
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
			backend := idsBackend{&scaleUpBackend{phase: workload.JobPhaseSucceeded, scheduledNodes: tc.scheduledNodes, clusterIDs: clusterA}, unreadable}
			w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
			exp := scaleUpExperiment(1, time.Minute)
			exp.Status = tc.status

			err := w.reconcileOne(context.Background(), exp, nil, nil)
			if tc.status == domain.StatusRunning {
				if err != nil || !store.transitioned {
					t.Fatalf("err = %v, transitioned = %v", err, store.transitioned)
				}
				return
			}
			if err == nil || errors.Is(err, unreadable) || !strings.Contains(err.Error(), "metrics DB URL") {
				t.Fatalf("err = %v, want the job to have reached its terminal transition", err)
			}
		})
	}
}

// A RUNNING job is past every stuck-pending check, so its cluster's settings are not read for it.
func TestARunningJobDoesNotReadItsClustersSettings(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("must not be read")}
	backend := idsBackend{&scaleUpBackend{phase: workload.JobPhaseRunning, scheduledNodes: 1}, errors.New("must not be read")}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
	exp := scaleUpExperiment(1, time.Hour)
	exp.Status = domain.StatusRunning

	if err := w.reconcileOne(context.Background(), exp, nil, nil); err != nil || store.transitioned {
		t.Fatalf("err = %v, transitioned = %v", err, store.transitioned)
	}
}

// The whole pass, on a deployment that never opted into the scale-up deadline: the override is
// still applied, with the cluster ids read where the decision is made.
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
			backend := &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0, clusterIDs: clusterA}
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

// Cluster ids that cannot be read, on a deployment without the scale-up deadline, cost the jobs
// waiting on a stuck-pending decision that one decision. The pass itself goes on and still
// finishes the jobs that have ended, on every runtime.
func TestUnreadableClusterIDsDoNotEndAPassWithoutTheScaleUpOptIn(t *testing.T) {
	finished := scaleUpExperiment(1, time.Hour)
	finished.ID, finished.Status = "exp-finished", domain.StatusRunning
	store := &settingsStore{
		lifecycleStore: &lifecycleStore{},
		submitted:      []*domain.Experiment{scaleUpExperiment(1, time.Hour)},
		running:        []*domain.Experiment{finished},
	}
	backend := &phaseByJobBackend{
		idsBackend: idsBackend{&scaleUpBackend{scheduledNodes: 1}, errors.New("metrics store unavailable")},
		phases:     map[string]workload.JobPhase{"exp-su": workload.JobPhaseGone, "exp-finished": workload.JobPhaseSucceeded},
	}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

	if err := w.scanAndWatch(context.Background()); err != nil {
		t.Fatalf("the pass ended: %v", err)
	}
	if !store.transitioned || len(store.terminalReasons) != 0 {
		t.Fatalf("transitioned = %v, terminalReasons = %v, want the finished job completed and nothing evicted", store.transitioned, store.terminalReasons)
	}
}

type phaseByJobBackend struct {
	idsBackend
	phases map[string]workload.JobPhase
}

func (b *phaseByJobBackend) PollJobPhase(_ context.Context, exp *domain.Experiment) (workload.JobPhase, error) {
	return b.phases[exp.ID], nil
}
