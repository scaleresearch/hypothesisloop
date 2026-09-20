package scheduler

import (
	"context"
	"errors"
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
}

func (s *settingsStore) GetClusterSettings(_ context.Context, clusterID string) (*domain.ClusterSettings, error) {
	return s.settings[clusterID], s.settingsErr
}

func (s *settingsStore) ListExperimentsWithStatus(_ context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error) {
	if status == domain.StatusSubmitted {
		return s.submitted, nil
	}
	return nil, nil
}

// unobservableBackend runs the job on a type the metrics store cannot yet name.
type unobservableBackend struct{ *scaleUpBackend }

func (unobservableBackend) GetAdmittedAcceleratorType(context.Context, *domain.Experiment) (domain.AcceleratorType, error) {
	return "", errors.New("no observation yet")
}

func stuckPendingOverride(seconds int) map[string]*domain.ClusterSettings {
	return map[string]*domain.ClusterSettings{"cid-a": {ClusterID: "cid-a", StuckPendingTimeoutSeconds: &seconds}}
}

var errPhase = errors.New("no snapshot")

// A cluster of rented instances boots slowly. Its own stuck-pending timeout replaces the global
// one in both directions, for every way a job can fail to reach RUNNING.
func TestAClustersOwnStuckPendingTimeoutReplacesTheGlobalOne(t *testing.T) {
	const global = 5 * time.Minute
	ways := map[string]JobBackendClient{
		"never placed":                       &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0},
		"placed, then gone":                  &scaleUpBackend{phase: workload.JobPhaseGone, scheduledNodes: 1},
		"placed, phase unreadable":           &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 1, phaseErr: errPhase},
		"running on a type not yet observed": unobservableBackend{&scaleUpBackend{phase: workload.JobPhaseRunning, scheduledNodes: 1}},
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

				err := w.reconcileOne(context.Background(), exp, nil, map[string]string{"cluster-a": "cid-a"})
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
	backend := &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

	if err := w.reconcileOne(context.Background(), scaleUpExperiment(1, 6*time.Minute), nil, map[string]string{"cluster-b": "cid-b"}); err != nil {
		t.Fatal(err)
	}
	if len(store.terminalReasons) != 1 || store.terminalReasons[0] != string(domain.EvictionStuckPending) {
		t.Fatalf("terminalReasons = %v", store.terminalReasons)
	}
}

// A deadline that cannot be read is not replaced by the global one: the job is left for the next
// pass instead of being evicted on a timeout its cluster may not have.
func TestUnreadableClusterSettingsEvictNothing(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("database unavailable")}
	backend := &scaleUpBackend{phase: workload.JobPhasePending, scheduledNodes: 0}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)

	err := w.reconcileOne(context.Background(), scaleUpExperiment(1, time.Hour), nil, map[string]string{"cluster-a": "cid-a"})
	if err == nil || store.transitioned {
		t.Fatalf("err = %v, transitioned = %v, want an error and no eviction", err, store.transitioned)
	}
}

// A RUNNING job is past every stuck-pending check, so its cluster's settings are not read for it.
func TestARunningJobDoesNotReadItsClustersSettings(t *testing.T) {
	store := &settingsStore{lifecycleStore: &lifecycleStore{}, settingsErr: errors.New("must not be read")}
	backend := &scaleUpBackend{phase: workload.JobPhaseRunning, scheduledNodes: 1}
	w := NewJobWatcher(store, backend, noopSettler{}, zap.NewNop()).WithStuckPendingTimeout(5 * time.Minute)
	exp := scaleUpExperiment(1, time.Hour)
	exp.Status = domain.StatusRunning

	if err := w.reconcileOne(context.Background(), exp, nil, map[string]string{"cluster-a": "cid-a"}); err != nil || store.transitioned {
		t.Fatalf("err = %v, transitioned = %v", err, store.transitioned)
	}
}

// The whole pass, on a deployment that never opted into the scale-up deadline: cluster ids are
// still read, since the stuck-pending override is keyed on them.
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
			backend := &scaleUpBackend{
				phase: workload.JobPhasePending, scheduledNodes: 0,
				clusterIDs: map[string]string{"cluster-a": "cid-a"},
			}
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
