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

// The summary gate holds a job submitted after an unsummarised job ended, and none that was
// queued before: the query takes the moment the waiting job was submitted.
func TestAnUnsummarisedJobIsOwedOnlyByWhatWasSubmittedAfterItEnded(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	if decision, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err != nil || decision != AdmitInserted {
		t.Fatalf("admit = %v %v, want inserted", decision, err)
	}
	ended := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, `UPDATE experiments SET status = 'COMPLETED', not_admitted_reason = NULL, updated_at = $2 WHERE id = $1`, exp.ID, ended); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		submitted time.Time
		want      int
	}{
		"queued a second before it ended":   {ended.Add(-time.Second), 0},
		"submitted the moment it ended":     {ended, 1},
		"submitted a second after it ended": {ended.Add(time.Second), 1},
	} {
		owed, err := store.UnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID, c.submitted)
		if err != nil || len(owed) != c.want || c.want == 1 && owed[0] != exp.ID {
			t.Errorf("a job %s: owed = %v (%v), want %d", name, owed, err, c.want)
		}
	}
}
