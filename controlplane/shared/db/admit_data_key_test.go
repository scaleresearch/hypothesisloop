package db

import (
	"context"
	"errors"
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
	createTestAgent(t, pool, agent, domain.AgentKindAgent)
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
		ID: "datakey-exp-" + suffix, AgentID: agent, PlatformExperimentID: pe,
		CodeRef: "repo@" + fortyHexChars(), ConfigHash: "cfg", DataRef: "none",
		HypothesisID: "h", Hypothesis: "h", Objective: "o", AcceleratorType: "cpu",
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
	decision, reason, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{},
		func(context.Context) error { created++; return nil })
	if err != nil || reason != "" || decision != AdmitInserted {
		t.Fatalf("admit = %v %q %v, want inserted", decision, reason, err)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataKey != exp.DataKey {
		t.Fatalf("stored key %+v, want %+v", got.DataKey, exp.DataKey)
	}

	// A re-submission of the queued job keeps the first key and creates none in the store.
	again := *exp
	again.DataKey = domain.DataKey{AccessKeyID: "other", SecretAccessKey: "other"}
	decision, _, err = store.AdmitExperimentTx(ctx, &again, noUsage, SubmissionRateLimit{},
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
}

// A store that cannot create the key refuses this submission alone: nothing is queued that would
// run with a key the store does not know.
func TestAdmitExperimentTxRollsBackWhenTheStoreCannotCreateTheKey(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	storeDown := errors.New("store down")
	_, _, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{},
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
