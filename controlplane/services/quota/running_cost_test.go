package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// aliveServer answers every metrics query with a sample every step from `ago` ago up to now, on
// both the heartbeat and training-metric series — enough to give a job a definite, nonzero
// observed-elapsed time without needing to model gap caps or the alive-grid mechanics exactly
// (those are already pinned by metricsdb's own tests). `ago` must exceed step so at least one
// full interval elapses.
func aliveServer(t *testing.T, now time.Time, step, ago time.Duration) *httptest.Server {
	t.Helper()
	var points []string
	for d := ago; d >= 0; d -= step {
		points = append(points, fmt.Sprintf(`[%d,"1"]`, now.Add(-d).Unix()))
	}
	matrix := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`,
		strings.Join(points, ","))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(matrix))
	}))
}

// runningExpStore fakes only the one PlatformExperimentsStore method running_cost.go calls;
// every other method panics if ever reached, so a test that exercises an unexpected call fails
// loudly instead of silently returning a zero value.
type runningExpStore struct {
	PlatformExperimentsStore
	running []*domain.Experiment
}

func (f *runningExpStore) GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	return f.running, nil
}

func newTestService(t *testing.T, metricsURL string, running []*domain.Experiment) *PlatformExperimentsService {
	t.Helper()
	return NewPlatformExperimentsService(
		&runningExpStore{running: running},
		domain.QuotaConfig{},
		zap.NewNop(),
		metricsURL,
		3*time.Minute, time.Minute,
	)
}

// newObservedTestService is newTestService but with a logger a test can inspect, for assertions
// that a warning did or did not fire — zap.NewNop() discards everything, which makes "no warning
// fired" untestable.
func newObservedTestService(t *testing.T, metricsURL string, running []*domain.Experiment) (*PlatformExperimentsService, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	svc := NewPlatformExperimentsService(
		&runningExpStore{running: running},
		domain.QuotaConfig{},
		zap.New(core),
		metricsURL,
		3*time.Minute, time.Minute,
	)
	return svc, logs
}

// A retired accelerator type (no entry in the live rate catalog — see
// domain.AcceleratorType.LookupCost) must not affect billing at all any more: accH is now derived
// from the job's own admission-time estimate (domain.Experiment.RatedCost), which needs no catalog
// lookup, so it prices identically whether or not the flavor is still registered. This also
// exercises that CPU-core-hours (which never needed an accelerator rate) keep billing normally.
func TestAddRunningActualCostsBillsNormallyWhenAcceleratorRateIsRetired(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	server := aliveServer(t, now, time.Minute, 10*time.Minute)
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-retired",
		CreatedAt:              now.Add(-30 * time.Minute),
		AcceleratorType:        "retired-flavor",
		AcceleratorCount:       2,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		EstimatedCPUCoreHours:  4,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1"}
	if err := svc.addRunningActualCosts(context.Background(), "pe-1", q); err != nil {
		t.Fatalf("addRunningActualCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s), want 0 — accH no longer depends on the live rate catalog: %v", n, logs.All())
	}

	if q.UsedGuaranteedAccH <= 0 {
		t.Fatalf("used_guaranteed_acch = %v, want > 0 — billed from the admission-time estimate, which needs no catalog rate",
			q.UsedGuaranteedAccH)
	}
	if q.UsedGuaranteedCPUCoreH <= 0 {
		t.Fatalf("used_guaranteed_cpu_core_h = %v, want > 0 — CPU cost needs no accelerator rate and must still be billed",
			q.UsedGuaranteedCPUCoreH)
	}
}

// correctRunningCosts's delta swap works the same whether or not the flavor is still registered
// in the live rate catalog, since accH no longer depends on it.
func TestCorrectRunningCostsBillsNormallyOnRetiredRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	server := aliveServer(t, now, time.Minute, 10*time.Minute)
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-retired",
		CreatedAt:              now.Add(-30 * time.Minute),
		AcceleratorType:        "retired-flavor",
		AcceleratorCount:       2,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      5,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: 5}
	if err := svc.correctRunningCosts(context.Background(), "pe-1", []*domain.AgentQuota{q}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s), want 0 — accH no longer depends on the live rate catalog: %v", n, logs.All())
	}
	// exp is alive ~30min out of a 1h estimated duration, so RatedCost(5, ~0.5h) ~= 2.5,
	// applied as a delta on top of the pre-seeded 5.
	if q.UsedGuaranteedAccH >= 5 {
		t.Fatalf("used_guaranteed_acch = %v, want < 5 — the observed-so-far cost is below the full estimate",
			q.UsedGuaranteedAccH)
	}
}

// A job that has never posted an observation must not warn or otherwise short-circuit ahead of
// the !found check.
func TestCorrectRunningCostsNeverObservedIsNotTreatedAsRetiredRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	// No samples at all: the job has never reported.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-fresh",
		CreatedAt:              now.Add(-time.Minute),
		AcceleratorType:        "h100", // a real, registered rate
		AcceleratorCount:       1,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      1,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: 1}
	if err := svc.correctRunningCosts(context.Background(), "pe-1", []*domain.AgentQuota{q}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s) for a job that has simply never reported yet, want 0: %v", n, logs.All())
	}
	if q.UsedGuaranteedAccH != 1 {
		t.Fatalf("used_guaranteed_acch = %v, want unchanged at 1 — an unobserved job must keep its estimate, not be treated as a rate failure",
			q.UsedGuaranteedAccH)
	}
}
