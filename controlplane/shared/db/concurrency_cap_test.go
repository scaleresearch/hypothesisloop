package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// seedExperimentForCapTest inserts a minimal experiment row via CreateExperiment, then forces its
// status directly (CreateExperiment always inserts QUEUED) — not going through AdmitExperimentTx,
// which is quota-gated on accelerator-hours, not concurrency: the cap this file tests is a
// second, independent gate on the same submit path.
func seedExperimentForCapTest(t *testing.T, pool *Pool, id, agentID, peID string, acceleratorCount int, status domain.ExperimentStatus) {
	t.Helper()
	hyp, _, err := NewHypothesesStore(pool).FindOrCreateHypothesis(context.Background(), domain.HypothesisSourceAgent, agentID, agentID, peID, "cap test hypothesis "+id)
	if err != nil {
		t.Fatalf("seed hypothesis for %s: got = %v, want = nil", id, err)
	}
	exps := NewExperimentsStore(pool)
	exp := &domain.Experiment{
		ID: id, AgentID: agentID, PlatformExperimentID: peID,
		HypothesisID: hyp.ID, Hypothesis: "h", Objective: "o",
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40", AcceleratorCount: acceleratorCount,
		EstimatedDurationHours: 1, EstimatedCostAccH: 1,
		CapacityTier: domain.CapacityGuaranteed, Status: domain.StatusQueued,
		Job: domain.JobSpec{AcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40", AcceleratorCount: acceleratorCount},
	}
	if err := exps.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("seed experiment %s: got = %v, want = nil", id, err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiments WHERE id = $1`, id)
	})
	if status != domain.StatusQueued {
		if _, err := pool.pool.Exec(context.Background(), `UPDATE experiments SET status=$2, not_admitted_reason=NULL WHERE id=$1`, id, status); err != nil {
			t.Fatalf("force status for %s: got = %v, want = nil", id, err)
		}
	}
}

func noopObserve(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{}, nil
}

func setupCapTestPool(t *testing.T, cap int) (*Pool, *PlatformExperimentsFullStore, string, string) {
	t.Helper()
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID, peID := "agent-cap-"+suffix, "pe-cap-"+suffix
	// Human kind, not Agent: AgentKindAgent gets zero guaranteed quota (burst-only) — this test is
	// about the concurrency cap, not the quota-tier split, so give it a guaranteed allocation big
	// enough that only the concurrency cap can reject a submit.
	createTestAgent(t, pool, agentID, domain.AgentKindHuman)
	createTestPE(t, pool, peID, 1000)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_concurrent_accelerators=$2 WHERE id=$1`, peID, cap); err != nil {
		t.Fatalf("set cap: got = %v, want = nil", err)
	}
	pes := NewPlatformExperimentsStore(pool)
	if _, err := pes.Signup(ctx, peID, agentID, domain.SignupRoleCompetitor, ""); err != nil {
		t.Fatalf("signup: got = %v, want = nil", err)
	}
	if _, _, err := pes.StartPlatformExperimentTx(ctx, peID, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		return allocateStartQuotas(peID, 1000, participants), nil
	}); err != nil {
		t.Fatalf("start: got = %v, want = nil", err)
	}
	return pool, NewPlatformExperimentsFullStore(NewStore(pool, "http://metrics.invalid", 3)), agentID, peID
}

// TestReserveAdmittedFlavorTxRejectsOverCap covers autoscaler.md's concurrency cap logic: a submit
// that would push this pool's SUBMITTED+RUNNING accelerator count over
// max_concurrent_accelerators is rejected with domain.NotAdmittedConcurrencyCap, and one that
// fits is admitted (the flavor/estimate columns are updated).
func TestReserveAdmittedFlavorTxRejectsOverCap(t *testing.T) {
	pool, store, agentID, peID := setupCapTestPool(t, 4)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	seedExperimentForCapTest(t, pool, "exp-running-"+suffix, agentID, peID, 2, domain.StatusRunning)
	seedExperimentForCapTest(t, pool, "exp-fits-"+suffix, agentID, peID, 2, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, "exp-over-"+suffix, agentID, peID, 4, domain.StatusQueued)

	// Fits: 2 running + 2 requested = 4 = cap.
	reason, err := store.ReserveAdmittedFlavorTx(ctx, "exp-fits-"+suffix, "nvidia.com/gpu.product=NVIDIA-L40", 1, 2, 0, noopObserve)
	if err != nil || reason != "" {
		t.Fatalf("fits-at-cap: got = (%q, %v), want = (\"\", nil)", reason, err)
	}

	// Over: 2 running + 4 requested = 6 > cap 4.
	reason, err = store.ReserveAdmittedFlavorTx(ctx, "exp-over-"+suffix, "nvidia.com/gpu.product=NVIDIA-L40", 1, 4, 0, noopObserve)
	if err != nil {
		t.Fatalf("over-cap: got err = %v, want nil", err)
	}
	if reason != domain.NotAdmittedConcurrencyCap {
		t.Fatalf("over-cap: reason = %q, want %q", reason, domain.NotAdmittedConcurrencyCap)
	}
}

// TestReserveAdmittedFlavorTxCapSerializesAgainstAnAlreadyCommittedBaseline demonstrates the part
// of the cap that is genuinely race-free: once a reservation is reflected in a SUBMITTED/RUNNING
// row (the state ClaimSubmitted writes right after a successful reservation in production), every
// later concurrent caller against the same pool sees it, because each call re-reads the SUM inside
// its own transaction under the pool's advisory lock. Two calls racing to reserve against rows
// that are STILL QUEUED (neither yet reflected as SUBMITTED/RUNNING) are NOT mutually visible to
// each other by this design — the doc's SUM is explicitly SUBMITTED+RUNNING only, and
// ReserveAdmittedFlavorTx does not itself flip status (ClaimSubmitted does, moments later, under a
// separate per-cluster lock). That is a real, narrow, accepted race bounded by how many control-
// plane replicas tick concurrently for the same pool — noted here rather than silently assumed.
func TestReserveAdmittedFlavorTxCapSerializesAgainstAnAlreadyCommittedBaseline(t *testing.T) {
	pool, store, agentID, peID := setupCapTestPool(t, 3)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	seedExperimentForCapTest(t, pool, "exp-a-"+suffix, agentID, peID, 2, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, "exp-b-"+suffix, agentID, peID, 2, domain.StatusQueued)

	// exp-a reserves and is claimed (status flips to SUBMITTED, as ClaimSubmitted would do),
	// bringing the pool to 2 in flight.
	if reason, err := store.ReserveAdmittedFlavorTx(ctx, "exp-a-"+suffix, "nvidia.com/gpu.product=NVIDIA-L40", 1, 2, 0, noopObserve); err != nil || reason != "" {
		t.Fatalf("exp-a reserve: got = (%q, %v), want = (\"\", nil)", reason, err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET status='SUBMITTED', not_admitted_reason=NULL WHERE id=$1`, "exp-a-"+suffix); err != nil {
		t.Fatalf("mark exp-a submitted: got = %v, want = nil", err)
	}

	// exp-b now sees the committed 2-in-flight baseline (exp-a) and requests 2 more: 2+2=4 > cap 3.
	reason, err := store.ReserveAdmittedFlavorTx(ctx, "exp-b-"+suffix, "nvidia.com/gpu.product=NVIDIA-L40", 1, 2, 0, noopObserve)
	if err != nil {
		t.Fatalf("exp-b reserve: got err = %v, want nil", err)
	}
	if reason != domain.NotAdmittedConcurrencyCap {
		t.Fatalf("exp-b reserve against a committed 2-in-flight baseline: reason = %q, want %q", reason, domain.NotAdmittedConcurrencyCap)
	}
}
