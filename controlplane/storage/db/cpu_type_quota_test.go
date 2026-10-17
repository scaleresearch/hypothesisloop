package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Cores are units of a priced type like any other to the store: a job on cores stands in the
// same concurrency cap with its count, and in-flight sums keep cores apart from devices.

const (
	quotaCPUClass = domain.CPUType
	quotaL40      = "nvidia.com/gpu.product=NVIDIA-L40"
)

func seedPricedExperiment(t *testing.T, pool *Pool, agentID, peID string, acceleratorType domain.AcceleratorType, count int, cost float64, status domain.ExperimentStatus) string {
	t.Helper()
	id := "exp-priced-" + uuid.New().String()[:8]
	exp := &domain.Experiment{
		ID: id, AgentID: agentID, Actor: domain.Actor{Kind: domain.ActorAgent, ID: agentID}, PlatformExperimentID: peID,
		HypothesisID: "priced-hypothesis-" + id, Hypothesis: "h",
		AcceleratorType: acceleratorType, AcceleratorCount: count, EstimatedDurationHours: 3, EstimatedCostAccH: cost,
		CapacityTier: domain.CapacityGuaranteed, Status: domain.StatusQueued,
		Job:     domain.JobSpec{AcceleratorType: acceleratorType, AcceleratorCount: count},
		DataKey: domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"},
	}
	if err := insertExperiment(context.Background(), pool, exp); err != nil {
		t.Fatalf("seed experiment %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiments WHERE id = $1`, id) })
	if status != domain.StatusQueued {
		if _, err := pool.pool.Exec(context.Background(), `UPDATE experiments SET status=$2, not_admitted_reason=NULL WHERE id=$1`, id, status); err != nil {
			t.Fatalf("force status of %s: %v", id, err)
		}
	}
	return id
}

func TestTheConcurrencyCapCountsCoresAndDevicesAsUnitsOfOnePool(t *testing.T) {
	pool, store, agentID, peID := setupCapTestPool(t, 10)
	ctx := context.Background()
	seedPricedExperiment(t, pool, agentID, peID, quotaL40, 2, 1, domain.StatusRunning)
	seedPricedExperiment(t, pool, agentID, peID, quotaCPUClass, 6, 0.18, domain.StatusSubmitted)

	for _, tc := range []struct {
		name            string
		acceleratorType domain.AcceleratorType
		count           int
		want            string
	}{
		{"2 more cores reach the cap", quotaCPUClass, 2, ""},
		{"3 more cores pass it", quotaCPUClass, 3, domain.NotAdmittedConcurrencyCap},
		{"2 more devices reach the cap", quotaL40, 2, ""},
		{"3 more devices pass it", quotaL40, 3, domain.NotAdmittedConcurrencyCap},
		{"16 cores alone pass it", quotaCPUClass, 16, domain.NotAdmittedConcurrencyCap},
	} {
		id := seedPricedExperiment(t, pool, agentID, peID, tc.acceleratorType, tc.count, 0.01, domain.StatusQueued)
		reason, err := store.ReserveAdmittedAcceleratorTypeTx(ctx, id, tc.acceleratorType, 0.01, tc.count, 0, noopObserve)
		if err != nil || reason != tc.want {
			t.Errorf("%s: got (%q, %v), want (%q, nil) with 2 devices and 6 cores in flight under a cap of 10", tc.name, reason, err, tc.want)
		}
	}
}

// Sixteen cores and four devices in flight are not twenty of anything.
func TestInFlightUnitsAreCountedPerKind(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 1000)
	seedPricedExperiment(t, pool, agentID, peID, quotaL40, 3, 1, domain.StatusRunning)
	seedPricedExperiment(t, pool, agentID, peID, quotaL40, 1, 1, domain.StatusSubmitted)
	seedPricedExperiment(t, pool, agentID, peID, quotaL40, 8, 1, domain.StatusQueued)
	seedPricedExperiment(t, pool, agentID, peID, quotaCPUClass, 8, 0.24, domain.StatusRunning)
	seedPricedExperiment(t, pool, agentID, peID, quotaCPUClass, 8, 0.48, domain.StatusSubmitted)
	seedPricedExperiment(t, pool, agentID, peID, quotaCPUClass, 32, 0.96, domain.StatusCompleted)

	store := NewStore(pool, 3)
	filter := domain.ExperimentFilter{Scope: domain.AllTenants, PlatformExperimentID: peID}
	stats, err := store.ExperimentStats(context.Background(), filter, "agent")
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]*domain.ExperimentStats{"all": stats, "the agent": stats.Groups[agentID]} {
		if got == nil || got.InFlightAccelerators != 4 || got.InFlightCores != 16 || got.Total != 6 || got.ByStatus["RUNNING"] != 2 {
			t.Errorf("%s: %+v, want 4 devices and 16 cores in flight among 6 jobs, 2 of them running", name, got)
		}
	}
}
