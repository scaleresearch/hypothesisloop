package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A researcher joins a running platform experiment whose executors fill max_agents, holds no quota,
// and is at work in the pool's count, outside the executors max_agents counts.
func TestAResearcherTakesNoPlaceOfMaxAgentsAndNoQuota(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET max_agents = 1 WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	spawn := func(role domain.SignupRole) string {
		t.Helper()
		id, _ := domain.NewID(domain.AgentIDPrefix)
		if err := pes.SpawnAgent(ctx, pe, &domain.Agent{ID: id, CreatedAt: time.Now().UTC()}, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role}); err != nil {
			t.Fatalf("spawn a %s: %v", role, err)
		}
		return id
	}
	executor := spawn(domain.SignupRoleExecutor)
	early := spawn(domain.SignupRoleResearcher)
	started, quotas, err := pes.StartPlatformExperimentTx(ctx, pe, time.Hour, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		if len(participants) != 1 || participants[0].AgentID != executor {
			t.Errorf("participants = %+v, want the executor alone", participants)
		}
		return allocateStartQuotas(pe, 4, participants), oneStage, nil
	})
	if err != nil || !started || len(quotas) != 1 {
		t.Fatalf("start: %v %v %d quotas", started, err, len(quotas))
	}
	id, _ := domain.NewID(domain.AgentIDPrefix)
	late := LateJoin{Used: map[string]float64{executor: 1}, BurstFraction: 1}
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: id, CreatedAt: time.Now().UTC()}, domain.AgentSignup{Actor: testActor, AgentID: id, Role: domain.SignupRoleResearcher}, late); err != nil {
		t.Fatalf("a researcher joining a full run: %v, want it added", err)
	}
	for _, researcher := range []string{early, id} {
		if q, err := pes.GetAgentQuota(ctx, researcher, pe); err != nil || q != nil {
			t.Errorf("quota of researcher %s: %+v %v, want none", researcher, q, err)
		}
		if signup := signupOf(t, pes, pe, researcher); !signup.AtWork || signup.AgentType != domain.ResearcherAgentType {
			t.Errorf("researcher %s: %+v, want at work as the built-in researcher", researcher, signup)
		}
	}
	atWork, err := pes.AgentsAtWork(ctx, pe)
	if err != nil || atWork[domain.PoolKey{AgentType: domain.ResearcherAgentType}] != 2 || atWork[domain.PoolKey{AgentType: domain.DefaultAgentType}] != 1 {
		t.Errorf("at work = %v %v, want two researchers and one executor", atWork, err)
	}
	second, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: second, CreatedAt: time.Now().UTC()}, domain.AgentSignup{Actor: testActor, AgentID: second, Role: domain.SignupRoleExecutor}, late); !errors.Is(err, ErrMaxAgentsReached) {
		t.Errorf("a second executor past max_agents 1: got = %v, want ErrMaxAgentsReached", err)
	}
}

// A researcher runs on its own model, else its type's, else its platform experiment's
// coordinator_model, else the platform's coordinator default; an executor never on the
// coordinator's. The model proxy reads the same choice of each.
func TestAResearchersModelIsItsOwnElseItsTypesElseTheCoordinators(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	spawn := func(role domain.SignupRole, model string) string {
		t.Helper()
		id, _ := domain.NewID(domain.AgentIDPrefix)
		if err := pes.SpawnAgent(ctx, pe, &domain.Agent{ID: id, CreatedAt: time.Now().UTC()}, domain.AgentSignup{Actor: testActor, AgentID: id, Role: role, Model: model, InAgentSandbox: true}); err != nil {
			t.Fatalf("spawn a %s: %v", role, err)
		}
		return id
	}
	own, inherits, executor := spawn(domain.SignupRoleResearcher, "anthropic/own"), spawn(domain.SignupRoleResearcher, ""), spawn(domain.SignupRoleExecutor, "")
	defaults := domain.ModelDefaults{Agent: "anthropic/agent-default", Coordinator: "anthropic/coordinator-default"}
	model := func(agentID string) string {
		t.Helper()
		src, err := pes.AgentInstructions(ctx, pe, agentID)
		if err != nil || src == nil {
			t.Fatalf("instructions of %s: %+v %v", agentID, src, err)
		}
		proxied, err := pes.ProxiedModel(ctx, pe, agentID)
		if err != nil || proxied == nil || *proxied != src.ModelChoice() {
			t.Fatalf("the proxy's choice of %s: %+v %v, want its instructions' %+v", agentID, proxied, err, src.ModelChoice())
		}
		return src.ModelChoice().Model(defaults)
	}
	if model(own) != "anthropic/own" || model(inherits) != "anthropic/coordinator-default" || model(executor) != "anthropic/agent-default" {
		t.Errorf("no coordinator_model: models %q %q %q, want its own, then each role's default", model(own), model(inherits), model(executor))
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET coordinator_model = 'anthropic/coordinator' WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if model(own) != "anthropic/own" || model(inherits) != "anthropic/coordinator" || model(executor) != "anthropic/agent-default" {
		t.Errorf("a coordinator_model: models %q %q %q, want its own, the coordinator's, and the default for the executor", model(own), model(inherits), model(executor))
	}
	if src, _ := pes.AgentInstructions(ctx, pe, inherits); src.Role != domain.AgentRoleResearcher {
		t.Errorf("role = %s, want researcher", src.Role)
	}
}
