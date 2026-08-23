package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// fakeObserved answers the controller's metrics-store reads from plain fields, one field per
// question the code actually asks.
//
// It replaced an httptest server that hand-wrote GreptimeDB result JSON and routed requests by
// matching substrings of the emitted SQL ("LAG(ts)", "CROSS JOIN present"). Two things were wrong
// with that: a change to a query's text silently re-routed a test to the wrong answer rather than
// failing it, and the stub answered every snapshot-presence query identically — so the cluster
// unreachable, workload gone, and absence-not-yet-confirmed branches of checkSilence, three of its
// most consequential verdicts, could not be reached from a test at all.
//
// Fields are named for the fact, not the query: what has been seen, not what the SQL returns.
type fakeObserved struct {
	// firstObserved / observedOK are the job's first confirmed observation. observedOK=false is a
	// job nothing has ever been seen from, which checkSilence must decline to judge.
	firstObserved time.Time
	observedOK    bool

	alive      bool
	phase      workload.JobPhase
	phaseFound bool

	presence metricsdb.SnapshotPresence

	// declaredReported / declaredChanged answer the in-window progress read. reported=true means
	// at least two samples arrived — one point cannot show movement, so metricsdb folds it into
	// not-reported. declaredEverReported is the lifetime read, where one sample is a complete
	// answer.
	declaredReported     bool
	declaredChanged      bool
	declaredEverReported bool

	// everReportedAny is the "did any metric at all ever arrive" read, used to tell a dead trainer
	// apart from a reporting path that never worked.
	everReportedAny bool

	// best is the per-agent standing BestPerAgentOnMetric reports, and nonRaw the agents that only
	// ever reported the metric on a rescaled basis — flagged rather than ranked.
	best   map[string]metricsdb.AgentBest
	nonRaw []string

	// failOn names one method that returns an error, so a test can prove the controller declines
	// to act on an answer it could not get rather than guessing one.
	failOn string
}

func (f fakeObserved) fail(method string) error {
	if f.failOn == method {
		return fmt.Errorf("metrics store unavailable")
	}
	return nil
}

func (f fakeObserved) IsAlive(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return f.alive, f.fail("IsAlive")
}

func (f fakeObserved) ObservedElapsedHours(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (float64, error) {
	return 0, f.fail("ObservedElapsedHours")
}

func (f fakeObserved) FirstObserved(_ context.Context, _ string, _, _ time.Time, _ time.Duration) (time.Time, bool, error) {
	return f.firstObserved, f.observedOK, f.fail("FirstObserved")
}

func (f fakeObserved) LatestJobPhase(_ context.Context, _, _ string, _ time.Duration) (workload.JobPhase, bool, error) {
	return f.phase, f.phaseFound, f.fail("LatestJobPhase")
}

func (f fakeObserved) ClusterSnapshotPresence(_ context.Context, _, _ string, _, _ time.Time) (metricsdb.SnapshotPresence, error) {
	return f.presence, f.fail("ClusterSnapshotPresence")
}

func (f fakeObserved) AnyDeclaredMetricChanged(_ context.Context, _ string, _ []string, _ time.Duration) (bool, bool, error) {
	return f.declaredReported, f.declaredChanged, f.fail("AnyDeclaredMetricChanged")
}

func (f fakeObserved) AnyDeclaredMetricReported(_ context.Context, _ string, _ []string, _ time.Duration) (bool, error) {
	return f.declaredEverReported, f.fail("AnyDeclaredMetricReported")
}

func (f fakeObserved) HasEverReportedMetric(_ context.Context, _ string, _, _ time.Time) (bool, error) {
	return f.everReportedAny, f.fail("HasEverReportedMetric")
}

func (f fakeObserved) TotalObservedAccH(_ context.Context, _ time.Time, _ string) (float64, error) {
	return 0, f.fail("TotalObservedAccH")
}

func (f fakeObserved) PopulateUsage(_ context.Context, _ time.Time, _ string, _ []*domain.AgentQuota) error {
	return f.fail("PopulateUsage")
}

func (f fakeObserved) BestPerAgentOnMetric(_ context.Context, _ *domain.PlatformExperiment, _ domain.MetricDefinition) (map[string]metricsdb.AgentBest, []string, error) {
	return f.best, f.nonRaw, f.fail("BestPerAgentOnMetric")
}
