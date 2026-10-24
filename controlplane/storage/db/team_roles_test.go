package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A database stored before executors and researchers were apart has each researcher type made an
// executor at the next start, the built-in researcher left as it is, and a proposal that stated
// its pool as researchers stating it as agents. A second start changes nothing more.
func TestAStartMovesAnOldDatabaseOntoExecutorsAndResearchers(t *testing.T) {
	pool := builtDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	tuner, err := NewStore(pool, 3).Create(ctx, "default", "Tuner", "", domain.AgentRoleExecutor, testPrompt, testTypeSettings(), *testActor)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE agent_types SET role = 'researcher' WHERE id IN ($1, 'generalist')`, tuner.ID)
	proposal := `{"goal_version":1,"experiment":{"name":"n","researchers":[{"agent_type":"generalist","count":2}]},"agents":[{"agent_type":"generalist"}]}`
	exec(`INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, default_quota_tier, created_by_kind, proposal)
VALUES ('pe-old', 'default', 'pe-old', 1, now(), now() + interval '1 hour', 'guaranteed', 'staff', $1)`, proposal)

	for range 2 {
		if built, err := EnsureSchema(ctx, pool); err != nil || built {
			t.Fatalf("start: %v, %v", built, err)
		}
	}

	roles := map[string]string{}
	rows, err := pool.pool.Query(ctx, `SELECT id, role FROM agent_types WHERE id IN ($1, 'generalist', 'researcher', 'baseline')`, tuner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			t.Fatal(err)
		}
		roles[id] = role
	}
	if roles[tuner.ID] != "executor" || roles["generalist"] != "executor" || roles["researcher"] != "researcher" || roles["baseline"] != "baseline" {
		t.Errorf("roles = %v, want the researcher types executors and the built-in researcher and baseline kept", roles)
	}
	var stored []byte
	if err := pool.pool.QueryRow(ctx, `SELECT proposal FROM platform_experiments WHERE id = 'pe-old'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var got struct {
		GoalVersion int                        `json:"goal_version"`
		Experiment  map[string]json.RawMessage `json:"experiment"`
		Agents      []map[string]any           `json:"agents"`
	}
	if err := json.Unmarshal(stored, &got); err != nil {
		t.Fatal(err)
	}
	if _, old := got.Experiment["researchers"]; old || string(got.Experiment["agents"]) != `[{"count": 2, "agent_type": "generalist"}]` || got.GoalVersion != 1 || len(got.Agents) != 1 {
		t.Errorf("proposal = %s, want its experiment's pool under agents and the rest kept", stored)
	}
}

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
	executor := spawn(domain.SignupRoleCompetitor)
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
	if err := pes.JoinRunning(ctx, pe, &domain.Agent{ID: second, CreatedAt: time.Now().UTC()}, domain.AgentSignup{Actor: testActor, AgentID: second, Role: domain.SignupRoleCompetitor}, late); !errors.Is(err, ErrMaxAgentsReached) {
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
	own, inherits, executor := spawn(domain.SignupRoleResearcher, "anthropic/own"), spawn(domain.SignupRoleResearcher, ""), spawn(domain.SignupRoleCompetitor, "")
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
