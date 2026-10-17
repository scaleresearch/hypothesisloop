package db

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// max_agents is decided inside the signup's own transaction. Counted before it, concurrent
// competitors each saw room and all of them committed.
func TestConcurrentCompetitorSignupsNeverExceedMaxAgents(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	pe := "pe-" + suffix
	createTestPE(t, pool, pe, 10)
	const maxAgents, contenders = 2, 8
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = $2 WHERE id = $1`, pe, maxAgents); err != nil {
		t.Fatalf("set max_agents: got = %v, want = nil", err)
	}
	agents := make([]string, contenders)
	for i := range agents {
		agents[i] = "agent-" + suffix + "-" + string(rune('a'+i))
		createTestAgent(t, pool, agents[i])
	}
	baseline := "baseline-" + suffix
	createTestAgent(t, pool, baseline)

	pes := NewStore(pool, 3)
	results := make([]error, contenders)
	var wg sync.WaitGroup
	for i, agentID := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = pes.Signup(ctx, pe, domain.AgentSignup{Actor: testActor, AgentID: agentID, Role: domain.SignupRoleCompetitor})
		}()
	}
	wg.Wait()

	refused := 0
	for _, err := range results {
		switch {
		case errors.Is(err, ErrMaxAgentsReached):
			refused++
		case err != nil:
			t.Fatalf("signup: got = %v, want = nil or ErrMaxAgentsReached", err)
		}
	}
	if refused != contenders-maxAgents {
		t.Errorf("refused signups: got = %v, want = %v", refused, contenders-maxAgents)
	}
	competitors, err := pes.ListSignupsByRole(ctx, pe, domain.SignupRoleCompetitor)
	if err != nil {
		t.Fatalf("list competitors: got = %v, want = nil", err)
	}
	if len(competitors) != maxAgents {
		t.Errorf("competitors signed up: got = %v, want = %v", len(competitors), maxAgents)
	}
	if inserted, err := pes.Signup(ctx, pe, domain.AgentSignup{Actor: testActor, AgentID: baseline, Role: domain.SignupRoleBaseline}); err != nil || !inserted {
		t.Errorf("baseline signup into a full field: got = %v, %v, want = true, nil", inserted, err)
	}
}
