package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func admitDataKeyFixture(t *testing.T) (*PlatformExperimentsFullStore, *domain.Experiment) {
	t.Helper()
	pool := eventsTestDB(t)
	suffix := uuid.New().String()[:8]
	agent, pe := "datakey-agent-"+suffix, "datakey-pe-"+suffix
	createTestAgent(t, pool, agent)
	createTestPE(t, pool, pe, 10)
	store := NewPlatformExperimentsFullStore(NewStore(pool, "http://metrics.invalid", 1))
	if _, err := pool.pool.Exec(context.Background(), upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: uuid.New().String(), AgentID: agent, PlatformExperimentID: pe,
		GuaranteedAcceleratorHours: 10, BurstAcceleratorHours: 5, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		t.Fatalf("upsert quota: %v", err)
	}
	retries := 0
	now := time.Now().UTC()
	exp := &domain.Experiment{
		ID: "datakey-exp-" + suffix, AgentID: agent, Actor: domain.Actor{Kind: domain.ActorAgent, ID: agent}, PlatformExperimentID: pe,
		CodeRef: "repo@" + fortyHexChars(), Branch: "hl/pe/researcher-1/idea-abc123", ConfigHash: "cfg", DataRef: "none",
		HypothesisID: "h", Hypothesis: "h", AcceleratorType: "cpu",
		EstimatedDurationHours: 1, Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now,
		Job:     domain.JobSpec{Image: "img", CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries},
		DataKey: domain.DataKey{AccessKeyID: "AK-" + suffix, SecretAccessKey: "SK-" + suffix},
	}
	return store, exp
}

func noUsage(context.Context) (*domain.AgentQuota, error) { return &domain.AgentQuota{}, nil }

func fortyHexChars() string {
	return "0123456789abcdef0123456789abcdef01234567"
}

// The key is written with the row and read back with every experiment, so the reconcile answer
// can hand it out without asking the store.
func TestAdmitExperimentTxStoresTheDataKeyWithTheRow(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	created := 0
	decision, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{},
		func(context.Context) error { created++; return nil })
	if err != nil || decision != AdmitInserted {
		t.Fatalf("admit = %v %v, want inserted", decision, err)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataKey != exp.DataKey {
		t.Fatalf("stored key %+v, want %+v", got.DataKey, exp.DataKey)
	}
	if got.Branch != exp.Branch {
		t.Fatalf("stored branch %q, want %q", got.Branch, exp.Branch)
	}

	// A re-submission of the queued job keeps the first key and creates none in the store.
	again := *exp
	again.DataKey = domain.DataKey{AccessKeyID: "other", SecretAccessKey: "other"}
	decision, err = store.AdmitExperimentTx(ctx, &again, noUsage, SubmissionRateLimit{},
		func(context.Context) error { created++; return nil })
	if err != nil || decision != AdmitAlreadyQueued {
		t.Fatalf("re-submit = %v %v, want already queued", decision, err)
	}
	if created != 1 {
		t.Fatalf("store asked to create %d keys, want 1", created)
	}
	if got, _ := store.GetExperiment(ctx, exp.ID); got.DataKey != exp.DataKey {
		t.Fatalf("re-submission replaced the key with %+v", got.DataKey)
	}

	// Burst hours are inside the agent's share of the budget: with 8 of its 10 hours spent, a
	// burst job of 3 is refused though 5 burst hours are unused.
	burst := *exp
	burst.ID, burst.CapacityTier, burst.EstimatedCostAccH = exp.ID+"-burst", domain.CapacityBurst, 3
	spent := func(context.Context) (*domain.AgentQuota, error) {
		return &domain.AgentQuota{UsedGuaranteedAccH: 8}, nil
	}
	var refusal *AdmitRefusal
	decision, err = store.AdmitExperimentTx(ctx, &burst, spent, SubmissionRateLimit{}, func(context.Context) error { return nil })
	if !errors.As(err, &refusal) || decision != AdmitRejected || refusal.Reason != RefusedInsufficientQuota || !strings.Contains(refusal.Message, "need 3 accelerator_hours (") || !strings.Contains(refusal.Message, "has 2 remaining") || !strings.Contains(refusal.Message, "it is spent") {
		t.Fatalf("burst job past the share = %v %v, want it refused for the share, saying none of it is a reservation", decision, err)
	}

	// Both researchers of a run were refused their second trial with nothing spent: the first
	// held its whole estimate. The refusal says how many jobs hold how much.
	held := *exp
	held.ID, held.EstimatedCostAccH = exp.ID+"-held", 9
	if decision, err = store.AdmitExperimentTx(ctx, &held, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err != nil || decision != AdmitInserted {
		t.Fatalf("a job inside the share = %v %v, want inserted", decision, err)
	}
	_, err = store.AdmitExperimentTx(ctx, &burst, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil })
	if want := "has 1 remaining"; !errors.As(err, &refusal) || !strings.Contains(refusal.Message, want) || !strings.Contains(refusal.Message, "1 of your jobs that have not ended hold 9 by their estimates") {
		t.Fatalf("a job past what the queued one leaves = %v, want %q and the one job holding 9", err, want)
	}

	// Each refusal carries its own reason: a cut agent and one with no quota were answered as
	// short of credits, with a stage outlook that was not theirs.
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, `DELETE FROM agent_quotas WHERE agent_id = $1 AND platform_experiment_id = $2`, exp.AgentID, exp.PlatformExperimentID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitExperimentTx(ctx, &burst, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); !errors.As(err, &refusal) || refusal.Reason != RefusedNoQuota {
		t.Fatalf("an agent with no quota = %v, want %s", err, RefusedNoQuota)
	}
	if _, err = store.AdmitExperimentTx(ctx, &burst, noUsage, SubmissionRateLimit{MaxPerHour: 1, Window: time.Hour}, func(context.Context) error { return nil }); !errors.As(err, &refusal) || refusal.Reason != RefusedRateLimited {
		t.Fatalf("an agent at its hourly limit = %v, want %s", err, RefusedRateLimited)
	}
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, exp.PlatformExperimentID, exp.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdmitExperimentTx(ctx, &burst, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); !errors.As(err, &refusal) || refusal.Reason != RefusedAgentCut {
		t.Fatalf("a cut agent = %v, want %s", err, RefusedAgentCut)
	}
}

// A submit stored after its client gave up waiting was sent again, and two jobs held quota. One
// agent's submits with one submission key are one job: the second creates nothing, asks the store
// for no key and names the first. Another key, and another agent's same key, are other jobs.
func TestASubmissionKeySeenFromTheAgentAnswersItsJobAndCreatesNothing(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	exp.SubmissionKey, exp.EstimatedCostAccH, exp.CapacityTier = "key-1", 6, domain.CapacityGuaranteed
	created := 0
	createKey := func(context.Context) error { created++; return nil }
	if decision, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, createKey); err != nil || decision != AdmitInserted {
		t.Fatalf("first submit = %v %v, want inserted", decision, err)
	}

	// 6 of the agent's 10 hours are held, so a second job of 6 would be refused: the repeat is not.
	again := *exp
	again.ID = exp.ID + "-again"
	decision, err := store.AdmitExperimentTx(ctx, &again, noUsage, SubmissionRateLimit{MaxPerHour: 1, Window: time.Hour}, createKey)
	if err != nil || decision != AdmitRepeated || again.ID != exp.ID || created != 1 {
		t.Fatalf("repeat = %v %v naming %s with %d keys created, want repeated, naming %s, with 1", decision, err, again.ID, created, exp.ID)
	}
	var jobs int
	if err := store.ExperimentsStore.pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM experiments WHERE agent_id = $1`, exp.AgentID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("the agent has %d jobs (%v), want 1", jobs, err)
	}

	other := *exp
	other.ID, other.SubmissionKey, other.EstimatedCostAccH = exp.ID+"-other", "key-2", 1
	if decision, err := store.AdmitExperimentTx(ctx, &other, noUsage, SubmissionRateLimit{}, createKey); err != nil || decision != AdmitInserted {
		t.Fatalf("another key = %v %v, want inserted", decision, err)
	}

	second := exp.AgentID + "-b"
	createTestAgent(t, store.ExperimentsStore.pool, second)
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: uuid.New().String(), AgentID: second, PlatformExperimentID: exp.PlatformExperimentID, GuaranteedAcceleratorHours: 10, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		t.Fatal(err)
	}
	theirs := *exp
	theirs.ID, theirs.AgentID, theirs.Actor.ID = exp.ID+"-theirs", second, second
	if decision, err := store.AdmitExperimentTx(ctx, &theirs, noUsage, SubmissionRateLimit{}, createKey); err != nil || decision != AdmitInserted || theirs.ID != exp.ID+"-theirs" {
		t.Fatalf("another agent's same key = %v %v naming %s, want a job of its own", decision, err, theirs.ID)
	}
}

// A baseline job that states no estimate was sized outside the transaction from a figure that
// counts a running job at what it spent, and refused 402 inside it, where the job counts at its
// estimate. The transaction sizes it from the figure it admits on.
func TestAJobWithNoEstimateTakesTheLargestTheAdmissionsOwnFigureHolds(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"cpu": 0.5})
	unsized := func(exp *domain.Experiment, id string, limit float64) *domain.Experiment {
		job := *exp
		job.ID, job.AcceleratorCount, job.EstimatedDurationHours, job.EstimatedCostAccH, job.MaxEstimateHours = exp.ID+id, 2, 0, 0, limit
		return &job
	}
	ctx := context.Background()
	noKey := func(context.Context) error { return nil }

	// 2 cores at 0.5 hold 1 an hour. With 4 spent and a running job holding 3.33337 of the 10,
	// 2.66663 remain: 2.6666 hours, rounded down to a step.
	store, exp := admitDataKeyFixture(t)
	exp.CapacityTier = domain.CapacityGuaranteed
	running := *exp
	running.ID, running.EstimatedCostAccH = exp.ID+"-running", 3.33337
	if _, err := store.AdmitExperimentTx(ctx, &running, noUsage, SubmissionRateLimit{}, noKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, `UPDATE experiments SET status = 'RUNNING', not_admitted_reason = NULL WHERE id = $1`, running.ID); err != nil {
		t.Fatal(err)
	}
	spent := func(context.Context) (*domain.AgentQuota, error) {
		return &domain.AgentQuota{UsedGuaranteedAccH: 4}, nil
	}
	job := unsized(exp, "-sized", 0)
	if decision, err := store.AdmitExperimentTx(ctx, job, spent, SubmissionRateLimit{}, noKey); err != nil || decision != AdmitInserted || job.EstimatedDurationHours != 2.6666 || job.EstimatedCostAccH != 2.6666 {
		t.Fatalf("got = %v %v with %g h holding %g, want inserted with 2.6666 h holding 2.6666", decision, err, job.EstimatedDurationHours, job.EstimatedCostAccH)
	}
	stored, err := store.GetExperiment(ctx, job.ID)
	if err != nil || stored.EstimatedDurationHours != 2.6666 || stored.EstimatedCostAccH != 2.6666 {
		t.Fatalf("stored %+v (%v), want the estimate it was admitted with", stored, err)
	}

	// Nothing is left now but a sliver under one step.
	var refusal *AdmitRefusal
	if _, err := store.AdmitExperimentTx(ctx, unsized(exp, "-none", 0), spent, SubmissionRateLimit{}, noKey); !errors.As(err, &refusal) || refusal.Reason != RefusedInsufficientQuota || !strings.Contains(refusal.Message, "no estimated_duration_hours fits it now") || !strings.Contains(refusal.Message, "2 of your jobs that have not ended hold 6") {
		t.Fatalf("with nothing remaining = %v, want %s saying nothing fits and what the two jobs hold", err, RefusedInsufficientQuota)
	}

	store, exp = admitDataKeyFixture(t)
	exp.CapacityTier = domain.CapacityGuaranteed
	limited := unsized(exp, "-limited", 0.125)
	if _, err := store.AdmitExperimentTx(ctx, limited, noUsage, SubmissionRateLimit{}, noKey); err != nil || limited.EstimatedDurationHours != 0.125 || limited.EstimatedCostAccH != 0.125 {
		t.Fatalf("within a limit of 0.125 h: %v with %g h holding %g, want 0.125 of each", err, limited.EstimatedDurationHours, limited.EstimatedCostAccH)
	}
}

// A store that cannot create the key refuses this submission alone: nothing is queued that would
// run with a key the store does not know.
func TestAdmitExperimentTxRollsBackWhenTheStoreCannotCreateTheKey(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	storeDown := errors.New("store down")
	_, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{},
		func(context.Context) error { return storeDown })
	if !errors.Is(err, storeDown) {
		t.Fatalf("admit = %v, want the store's error", err)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("experiment %s was inserted although its key was never created", exp.ID)
	}
}

// secret_env is stored with the row and apart from job_spec, which every read of a job returns.
func TestSecretEnvIsStoredApartFromTheJobSpec(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	exp.SecretEnv = map[string]string{"HF_TOKEN": "the-secret-value"}
	if decision, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err != nil || decision != AdmitInserted {
		t.Fatalf("admit = %v %v, want inserted", decision, err)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretEnv["HF_TOKEN"] != "the-secret-value" || got.Job.SecretEnv != nil {
		t.Fatalf("read back secret_env = %v, on the spec = %v, want it on the experiment only", got.SecretEnv, got.Job.SecretEnv)
	}
	var inSpec bool
	if err := store.ExperimentsStore.pool.pool.QueryRow(ctx, `SELECT job_spec::text LIKE '%the-secret-value%' OR job_spec ? 'secret_env' FROM experiments WHERE id = $1`, exp.ID).Scan(&inSpec); err != nil {
		t.Fatal(err)
	}
	if inSpec {
		t.Fatal("job_spec holds the secret")
	}

	_, onSpec := admitDataKeyFixture(t)
	onSpec.Job.SecretEnv = map[string]string{"HF_TOKEN": "the-secret-value"}
	if _, err := store.AdmitExperimentTx(ctx, onSpec, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("a job whose spec still carries secret_env was stored, want an error")
	}
}
