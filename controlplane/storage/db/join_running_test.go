package db

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// An executor that joins a running platform experiment takes the place of one that ended, or one
// max_agents gained, and its share first from what the ones that ended left unspent, then from the
// executors at work, and max_agents counts the executors at work.
func TestAnExecutorJoinsARunningPlatformExperimentInThePlaceOfOneThatEnded(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = 2 WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	spawn := func(name string) string {
		id, _ := domain.NewID(domain.AgentIDPrefix)
		agent := &domain.Agent{ID: id, Name: name + "-" + id, CreatedAt: time.Now().UTC()}
		if err := pes.SpawnAgent(ctx, pe, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleExecutor}); err != nil {
			t.Fatalf("spawn %s: got = %v, want = nil", name, err)
		}
		return id
	}
	atWork, ended := spawn("at-work"), spawn("ended")
	for _, id := range []string{atWork, ended} {
		qid, _ := domain.NewID(domain.AgentQuotaIDPrefix)
		if err := pool.write(ctx, func(w *Write) error {
			return upsertAgentQuota(ctx, w, &domain.AgentQuota{
				ID: qid, AgentID: id, PlatformExperimentID: pe, GuaranteedAcceleratorHours: 5, BurstAcceleratorHours: 5, CreatedAt: time.Now().UTC(),
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	late := LateJoin{Used: map[string]float64{atWork: 2, ended: 1}, BurstFraction: 1}
	join := func(role domain.SignupRole) (string, error) {
		id, _ := domain.NewID(domain.AgentIDPrefix)
		agent := &domain.Agent{ID: id, CreatedAt: time.Now().UTC()}
		return id, pes.JoinRunning(ctx, pe, agent, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role}, late)
	}
	if _, err := join(domain.SignupRoleExecutor); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("a third executor at work: got = %v, want = ErrMaxAgentsReached", err)
	}
	if err := pes.FinishAgent(ctx, pe, ended, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := join(domain.SignupRoleReviewer); !errors.Is(err, ErrSignupClosed) {
		t.Fatalf("a reviewer: got = %v, want = ErrSignupClosed", err)
	}
	newcomer, err := join(domain.SignupRoleExecutor)
	if err != nil {
		t.Fatalf("an executor in the place of one that finished: got = %v, want = nil", err)
	}
	// 3 + 4 unspent over the one at work and the newcomer, at most the 4 the ended one left.
	for id, want := range map[string]float64{newcomer: 3.5, ended: 1.5, atWork: 5} {
		q, err := pes.GetAgentQuota(ctx, id, pe)
		if err != nil || q == nil || math.Abs(q.GuaranteedAcceleratorHours-want) > 1e-6 {
			t.Errorf("quota of %s: got = %+v %v, want = %g guaranteed", id, q, err, want)
		}
	}
	late.Used[newcomer] = 0
	if _, err := join(domain.SignupRoleExecutor); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("an executor past max_agents at work: got = %v, want = ErrMaxAgentsReached", err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = 5 WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	late.Used[ended] = 1.5
	late.MinShare = 2.5
	if _, err := join(domain.SignupRoleExecutor); !errors.Is(err, ErrNoShareLeft) {
		t.Fatalf("an even part below one trial: got = %v, want = ErrNoShareLeft", err)
	}
	late.MinShare = 2
	added, err := join(domain.SignupRoleExecutor)
	if err != nil {
		t.Fatalf("an executor in a place max_agents gained: got = %v, want = nil", err)
	}
	// Nothing left by the one that ended: 3 + 3.5 unspent over three, given by the two at work
	// above it, each keeping what it used.
	for id, want := range map[string]float64{added: 6.5 / 3, atWork: 2 + 6.5/3, newcomer: 6.5 / 3, ended: 1.5} {
		q, err := pes.GetAgentQuota(ctx, id, pe)
		if err != nil || q == nil || math.Abs(q.GuaranteedAcceleratorHours-want) > 1e-5 {
			t.Errorf("quota of %s: got = %+v %v, want = %g guaranteed", id, q, err, want)
		}
	}
}
