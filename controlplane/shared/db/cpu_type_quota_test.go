package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A CPU class is a priced type like any other to the store: a job on cores draws on the same
// quota at its own estimate and stands in the same concurrency cap with its count. These tests
// hold that beside jobs on an accelerator type in one platform experiment.

const (
	quotaCPUClass      = "cpu.hypothesisloop.dev/class=EPYC-9354"
	quotaOtherCPUClass = "cpu.hypothesisloop.dev/class=Xeon-8480"
	quotaL40           = "nvidia.com/gpu.product=NVIDIA-L40"
)

func seedPricedExperiment(t *testing.T, pool *Pool, agentID, peID string, flavor domain.AcceleratorType, count int, cost float64, status domain.ExperimentStatus) string {
	t.Helper()
	id := "exp-priced-" + uuid.New().String()[:8]
	exp := &domain.Experiment{
		ID: id, AgentID: agentID, Actor: domain.Actor{Kind: domain.ActorAgent, ID: agentID}, PlatformExperimentID: peID,
		HypothesisID: "priced-hypothesis-" + id, Hypothesis: "h", Objective: "o",
		AcceleratorType: flavor, AcceleratorCount: count, EstimatedDurationHours: 3, EstimatedCostAccH: cost,
		CapacityTier: domain.CapacityGuaranteed, Status: domain.StatusQueued,
		Job:     domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: flavor, AcceleratorCount: count},
		DataKey: domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"},
	}
	if err := createExperiment(context.Background(), pool.pool, exp); err != nil {
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
		name   string
		flavor domain.AcceleratorType
		count  int
		want   string
	}{
		{"2 more cores reach the cap", quotaCPUClass, 2, ""},
		{"3 more cores pass it", quotaCPUClass, 3, domain.NotAdmittedConcurrencyCap},
		{"2 more devices reach the cap", quotaL40, 2, ""},
		{"3 more devices pass it", quotaL40, 3, domain.NotAdmittedConcurrencyCap},
		{"16 cores alone pass it", quotaCPUClass, 16, domain.NotAdmittedConcurrencyCap},
	} {
		id := seedPricedExperiment(t, pool, agentID, peID, tc.flavor, tc.count, 0.01, domain.StatusQueued)
		reason, err := store.ReserveAdmittedFlavorTx(ctx, id, tc.flavor, 0.01, tc.count, 0, noopObserve)
		if err != nil || reason != tc.want {
			t.Errorf("%s: got (%q, %v), want (%q, nil) with 2 devices and 6 cores in flight under a cap of 10", tc.name, reason, err, tc.want)
		}
	}
}

func TestACoreJobDrawsOnTheSameQuotaAtItsOwnCost(t *testing.T) {
	pool, store, agentID, peID := setupCapTestPool(t, 1000)
	ctx := context.Background()
	var limit float64
	if err := pool.pool.QueryRow(ctx, `SELECT guaranteed_accelerator_hours FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2`, agentID, peID).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	// Half an accelerator-hour is left of the agent's guaranteed quota.
	observe := func(context.Context, string, string, time.Time) (*domain.AgentQuota, error) {
		return &domain.AgentQuota{UsedGuaranteedAccH: limit - 0.5}, nil
	}
	reserve := func(flavor domain.AcceleratorType, count int, cost float64) (string, string) {
		id := seedPricedExperiment(t, pool, agentID, peID, flavor, count, cost, domain.StatusQueued)
		reason, err := store.ReserveAdmittedFlavorTx(ctx, id, flavor, cost, count, 0, observe)
		if err != nil {
			t.Fatalf("reserve %d of %s: %v", count, flavor, err)
		}
		return id, reason
	}

	// 8 cores for 3 hours at 0.01 a core-hour are 0.24. Two such jobs fit in 0.5 and a third does
	// not; nor does one l40 for 3 hours at 0.25, which is 0.75.
	first, reason := reserve(quotaCPUClass, 8, 0.24)
	if reason != "" {
		t.Fatalf("the first 8-core job: %q, want it within the 0.5 left", reason)
	}
	if _, reason := reserve(quotaCPUClass, 8, 0.24); reason != "" {
		t.Fatalf("the second 8-core job: %q, want it within the 0.5 left", reason)
	}
	third, reason := reserve(quotaCPUClass, 8, 0.24)
	if !strings.HasPrefix(reason, "insufficient_guaranteed_quota") || !strings.Contains(reason, quotaCPUClass) {
		t.Fatalf("the third 8-core job: %q, want the guaranteed quota short for the class", reason)
	}
	if _, err := pool.pool.Exec(ctx, `DELETE FROM experiments WHERE id=$1`, third); err != nil {
		t.Fatal(err)
	}
	if _, reason := reserve(quotaL40, 1, 0.75); !strings.HasPrefix(reason, "insufficient_guaranteed_quota") {
		t.Fatalf("an l40 for 3 hours beside the two core jobs: %q, want the quota short", reason)
	}

	// Moving a job to a dearer class is charged the difference: 0.24 becomes 0.48, and with the
	// other job's 0.24 that is 0.72 against the 0.5 left.
	reason, err := store.ReserveAdmittedFlavorTx(ctx, first, quotaOtherCPUClass, 0.48, 8, 0, observe)
	if err != nil || !strings.HasPrefix(reason, "insufficient_guaranteed_quota") {
		t.Fatalf("moving a job to the dearer class: (%q, %v), want the quota short", reason, err)
	}
	var flavor string
	var cost float64
	if err := pool.pool.QueryRow(ctx, `SELECT accelerator_type, estimated_cost_acch FROM experiments WHERE id=$1`, first).Scan(&flavor, &cost); err != nil {
		t.Fatal(err)
	}
	if flavor != quotaCPUClass || cost != 0.24 {
		t.Errorf("a refused move left the job as %s at %v, want it as it was", flavor, cost)
	}
}

// The same accelerator jobs against the same quota and cap, in a platform experiment that holds
// only them: the answers are those of the tests that predate the CPU class.
func TestAcceleratorJobsAreReservedAsBeforeInAnExperimentWithNoCoreJobs(t *testing.T) {
	pool, store, agentID, peID := setupCapTestPool(t, 4)
	ctx := context.Background()
	var limit float64
	if err := pool.pool.QueryRow(ctx, `SELECT guaranteed_accelerator_hours FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2`, agentID, peID).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	observe := func(context.Context, string, string, time.Time) (*domain.AgentQuota, error) {
		return &domain.AgentQuota{UsedGuaranteedAccH: limit - 1}, nil
	}
	seedPricedExperiment(t, pool, agentID, peID, quotaL40, 2, 0, domain.StatusRunning)

	fits := seedPricedExperiment(t, pool, agentID, peID, quotaL40, 2, 0.5, domain.StatusQueued)
	if reason, err := store.ReserveAdmittedFlavorTx(ctx, fits, quotaL40, 0.5, 2, 0, observe); err != nil || reason != "" {
		t.Fatalf("2 l40 at the cap and within quota: (%q, %v)", reason, err)
	}
	overCap := seedPricedExperiment(t, pool, agentID, peID, quotaL40, 3, 0.25, domain.StatusQueued)
	if reason, err := store.ReserveAdmittedFlavorTx(ctx, overCap, quotaL40, 0.25, 3, 0, observe); err != nil || reason != domain.NotAdmittedConcurrencyCap {
		t.Fatalf("3 l40 beside 2 running under a cap of 4: (%q, %v)", reason, err)
	}
	overQuota := seedPricedExperiment(t, pool, agentID, peID, quotaL40, 1, 0.5, domain.StatusQueued)
	if reason, err := store.ReserveAdmittedFlavorTx(ctx, overQuota, quotaL40, 0.5, 1, 0, observe); err != nil || !strings.HasPrefix(reason, "insufficient_guaranteed_quota") {
		t.Fatalf("a job that takes the estimates to 1.25 of the 1 left: (%q, %v)", reason, err)
	}
}
