package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// rosterStore lists the running platform experiments, or fails to.
type rosterStore struct {
	everyPEStagesStore
	err error
}

func (f *rosterStore) ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	if status == domain.PlatformExpRunning && f.err != nil {
		return nil, f.err
	}
	return f.everyPEStagesStore.ListPlatformExperimentsByStatus(ctx, status)
}

// oneRunningJob is the controller's Store for a pass over one running job.
type oneRunningJob struct {
	exhaustionStore
}

func (s *oneRunningJob) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}

// A platform experiment whose ranking metric is posted once, when a job completes, and whose
// jobs report an attribute metric as they go. Only ranking metrics and a metric named step
// counted as progress, so under any other name the job was evicted as never_reported_metrics
// while it reported.
func TestReconcileTakesAnyDeclaredMetricForProgress(t *testing.T) {
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, Metrics: []domain.MetricDefinition{
		{Key: "flops_to_target", Direction: "minimize"},
		{Key: "iterations_done", Direction: "maximize", Role: domain.MetricRoleAttribute},
	}}
	obs := liveJob()
	obs.movingKey = "iterations_done"
	job := runningExperiment()
	job.AgentID = "agent-1"
	store := &oneRunningJob{exhaustionStore{running: []*domain.Experiment{job}}}
	c := silenceController(obs)
	c.store, c.settler = store, settledNothing{}
	c.quota = observedQuota{quota: &domain.AgentQuota{GuaranteedAcceleratorHours: 10}}
	c.stagesStore = &rosterStore{everyPEStagesStore: everyPEStagesStore{running: []*domain.PlatformExperiment{pe}}}

	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: got = %v, want nil", err)
	}
	if len(store.terminated) != 0 {
		t.Errorf("terminated %v, want the job left running", store.terminated)
	}

	// The same job reporting nothing its platform experiment declared is still ended.
	obs.movingKey = "undeclared"
	c.observed = obs
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: got = %v, want nil", err)
	}
	if len(store.terminated) != 1 {
		t.Errorf("terminated %v, want the job that reports no declared metric ended", store.terminated)
	}
}

// A job of a platform experiment that reports every ten minutes, last seen two minutes ago. When
// the list of running platform experiments could not be read the pass went on with empty maps
// and judged the job on the default interval, by which it had been silent four windows long.
func TestReconcileJudgesNoJobWhoseListOfPlatformExperimentsItCouldNotRead(t *testing.T) {
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, ReportIntervalSeconds: 600,
		Metrics: []domain.MetricDefinition{{Key: declared, Direction: "maximize"}}}
	for _, tc := range []struct {
		name    string
		listErr error
	}{
		{"read", nil},
		{"unreadable", errors.New("query failed")},
	} {
		obs := liveJob()
		obs.firstObserved = time.Now().UTC().Add(-2 * time.Hour)
		obs.lastSeenAgo = 2 * time.Minute
		obs.declaredReported, obs.declaredChanged = true, true
		job := runningExperiment()
		job.AgentID = "agent-1"
		store := &oneRunningJob{exhaustionStore{running: []*domain.Experiment{job}}}
		c := silenceController(obs)
		c.store, c.settler = store, settledNothing{}
		c.quota = observedQuota{quota: &domain.AgentQuota{GuaranteedAcceleratorHours: 10}}
		c.stagesStore = &rosterStore{everyPEStagesStore: everyPEStagesStore{running: []*domain.PlatformExperiment{pe}}, err: tc.listErr}

		err := c.Reconcile(context.Background())
		if !errors.Is(err, tc.listErr) {
			t.Errorf("%s: Reconcile: got = %v, want %v", tc.name, err, tc.listErr)
		}
		if len(store.terminated) != 0 {
			t.Errorf("%s: terminated %v, want no job ended", tc.name, store.terminated)
		}
	}
}
