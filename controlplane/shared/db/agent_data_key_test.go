package db

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAgentDataKeyIsCreatedOnceEvenByConcurrentFirstCalls(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agent, pe := "agentkey-agent-"+suffix, "agentkey-pe-"+suffix
	createTestAgent(t, pool, agent)
	createTestPE(t, pool, pe, 10)
	store := NewPlatformExperimentsStore(pool)

	var mu sync.Mutex
	created := 0
	create := func(context.Context) (domain.DataKey, error) {
		mu.Lock()
		defer mu.Unlock()
		created++
		return domain.DataKey{AccessKeyID: "AK-" + uuid.New().String()[:8], SecretAccessKey: "SK"}, nil
	}

	if _, err := store.AgentDataKey(ctx, pe, agent, create); !errors.Is(err, ErrNotSignedUp) {
		t.Fatalf("before signup: err = %v, want ErrNotSignedUp", err)
	}
	if ok, err := store.Signup(ctx, pe, domain.AgentSignup{AgentID: agent, Role: domain.SignupRoleCompetitor}); err != nil || !ok {
		t.Fatalf("signup = %v %v", ok, err)
	}
	if _, err := store.AgentDataKey(ctx, pe, agent, func(context.Context) (domain.DataKey, error) {
		return domain.DataKey{}, errors.New("store refused")
	}); err == nil {
		t.Fatal("a failed create returned a key")
	}

	keys := make([]domain.DataKey, 4)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := store.AgentDataKey(ctx, pe, agent, create)
			if err != nil {
				t.Errorf("AgentDataKey: %v", err)
			}
			keys[i] = key
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created %d keys, want 1", created)
	}
	for _, key := range keys {
		if key.AccessKeyID == "" || key != keys[0] {
			t.Fatalf("calls returned different keys: %+v", keys)
		}
	}

	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AgentDataKey(ctx, pe, agent, create); !errors.Is(err, ErrPlatformExperimentClosed) {
		t.Fatalf("after close: err = %v, want ErrPlatformExperimentClosed", err)
	}
}

func TestRemovingAnAgentDeletesItsDataKeyBeforeItsSignup(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agent, pe := "agentkey-agent-"+suffix, "agentkey-pe-"+suffix
	createTestAgent(t, pool, agent)
	createTestPE(t, pool, pe, 10)
	store := NewPlatformExperimentsStore(pool)
	if ok, err := store.Signup(ctx, pe, domain.AgentSignup{AgentID: agent, Role: domain.SignupRoleCompetitor}); err != nil || !ok {
		t.Fatalf("signup = %v %v", ok, err)
	}
	key, err := store.AgentDataKey(ctx, pe, agent, func(context.Context) (domain.DataKey, error) {
		return domain.DataKey{AccessKeyID: "AK-" + suffix, SecretAccessKey: "SK"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	refused := errors.New("store refused")
	if err := store.RemoveAgent(ctx, pe, agent, func(context.Context, string) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("remove with a key the store kept: err = %v, want the store's refusal", err)
	}
	if in, err := store.IsSignedUp(ctx, pe, agent); err != nil || !in {
		t.Fatalf("signed up after a failed removal = %v %v, want true", in, err)
	}

	var deleted []string
	if err := store.RemoveAgent(ctx, pe, agent, func(_ context.Context, accessKeyID string) error {
		deleted = append(deleted, accessKeyID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != key.AccessKeyID {
		t.Fatalf("deleted keys %v, want %s", deleted, key.AccessKeyID)
	}
	if in, err := store.IsSignedUp(ctx, pe, agent); err != nil || in {
		t.Fatalf("signed up after removal = %v %v, want false", in, err)
	}
}
