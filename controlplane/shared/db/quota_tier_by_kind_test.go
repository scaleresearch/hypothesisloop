package db

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func quotaTierTestDB(t *testing.T) *Pool {
	t.Helper()
	return eventsTestDB(t)
}

func createTestAgent(t *testing.T, pool *Pool, id string) {
	t.Helper()
	agents := NewAgentsStore(pool)
	if err := agents.CreateAgent(context.Background(), &domain.Agent{
		ID: id, Name: id, OrgID: "default", PerformanceScore: 0.5, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create agent %s: got = %v, want = nil", id, err)
	}
}

func createTestPE(t *testing.T, pool *Pool, id string, budget float64) {
	t.Helper()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{
		ID: id, ProjectID: "default", Name: id, Description: "quota tier test", BudgetAcceleratorHours: budget,
		MaxAgents: 10, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics:               []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}},
		ReportIntervalSeconds: 30,
		Stages:                []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		CurrentStage:          1,
		// The store writes these through to CHECK-constrained columns. The service resolves the
		// empty default (domain.ParseSubmitterPolicy) before it ever gets here, so a caller
		// building the row itself has to resolve it too.,
		// Left empty on purpose in most of this file: an empty DefaultQuotaTier is the legacy/
		// unset-policy case, which ResolveQuotaTier now resolves to guaranteed for everyone (no
		// AgentKind involved at all) — the exact case these tests exist to pin down.
	}
	pe.MaxHosted = 50
	if err := pes.CreatePlatformExperiment(context.Background(), pe, nil, nil); err != nil {
		t.Fatalf("create platform experiment: got = %v, want = nil", err)
	}
}

// oneStage is the ladder these starts store: none of them is about the ladder.
var oneStage = []domain.Stage{{LengthPct: 100}}

// allocateStartQuotas mirrors services/quota.PlatformExperimentsService.Start's per-dimension
// allocation, without pulling in the whole service (no metrics dependency needed for this path)
// — the same domain.AllocateQuota + domain.ResolveQuotaTier/ApplyQuotaTier sequence production runs.
func allocateStartQuotas(peID string, budget float64, participants []StartParticipant) []*domain.AgentQuota {
	cfg := domain.QuotaConfig{BurstFraction: 0.5}
	out := make([]*domain.AgentQuota, 0, len(participants))
	for _, p := range participants {
		g, b := domain.AllocateQuota(budget, len(participants), 0, 0, cfg)
		g, b = domain.ApplyQuotaTier(domain.ResolveQuotaTier(p.QuotaTierOverride, ""), g, b)
		out = append(out, &domain.AgentQuota{
			ID: uuid.New().String(), AgentID: p.AgentID, PlatformExperimentID: peID,
			GuaranteedAcceleratorHours: g, BurstAcceleratorHours: b, CreatedAt: time.Now().UTC(),
		})
	}
	return out
}

// A signup-time override still lets one experiment mix tiers however it wants — an agent
// explicitly restricted to burst-only, and a human explicitly restricted to burst-only, in the
// same run — and it wins regardless of kind, exactly as it did before AgentKind was removed from
// the resolution order.
func TestSignupQuotaTierOverrideWinsRegardlessOfKind(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentBurstOnly, humanBurstOnly, pe := "agent-burst-"+suffix, "human-burst-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, agentBurstOnly)
	createTestAgent(t, pool, humanBurstOnly)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	if _, err := pes.Signup(ctx, pe, domain.AgentSignup{AgentID: agentBurstOnly, Role: domain.SignupRoleCompetitor, QuotaTier: domain.QuotaTierBurstOnly}); err != nil {
		t.Fatalf("signup agent with burst_only override: got = %v, want = nil", err)
	}
	if _, err := pes.Signup(ctx, pe, domain.AgentSignup{AgentID: humanBurstOnly, Role: domain.SignupRoleCompetitor, QuotaTier: domain.QuotaTierBurstOnly}); err != nil {
		t.Fatalf("signup human with burst_only override: got = %v, want = nil", err)
	}

	started, quotas, err := pes.StartPlatformExperimentTx(ctx, pe, time.Hour, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		return allocateStartQuotas(pe, 4, participants), oneStage, nil
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}

	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}
	for _, id := range []string{agentBurstOnly, humanBurstOnly} {
		if g := byAgent[id].GuaranteedAcceleratorHours; g != 0 {
			t.Errorf("%s explicitly restricted to burst_only: guaranteed = %v, want 0", id, g)
		}
		if b := byAgent[id].BurstAcceleratorHours; b <= 0 {
			t.Errorf("%s explicitly restricted to burst_only: burst = %v, want > 0", id, b)
		}
	}
}

// The stage-boundary credit routes through the same tier resolution as Start, so it must give
// both kinds of survivor the same (guaranteed) treatment; a cut agent's quota is zeroed
// regardless of kind or tier.
func TestAdvanceStageCreditsEverySurvivorIntoTheSameTierRegardlessOfKind(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	humanSurvivor, agentSurvivor, cutAgent, pe := "human-"+suffix, "agent-"+suffix, "cut-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, humanSurvivor)
	createTestAgent(t, pool, agentSurvivor)
	createTestAgent(t, pool, cutAgent)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	for _, id := range []string{humanSurvivor, agentSurvivor, cutAgent} {
		if _, err := pes.Signup(ctx, pe, domain.AgentSignup{AgentID: id, Role: domain.SignupRoleCompetitor}); err != nil {
			t.Fatalf("signup %s: got = %v, want = nil", id, err)
		}
	}
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe, time.Hour, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		return allocateStartQuotas(pe, 4, participants), oneStage, nil
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}

	dims := []StageRedistribution{{
		ResourceType: domain.ResourceAcceleratorHours, Budget: 10, ReleaseFrac: 0.6,
		UsedByAgent: map[string]float64{cutAgent: 0},
	}}
	advanced, err := pes.AdvanceStage(ctx, pe, 1, []string{cutAgent}, []string{humanSurvivor, agentSurvivor}, dims)
	if err != nil || !advanced {
		t.Fatalf("advance stage: got = (%v, %v), want = (true, nil)", advanced, err)
	}

	quotas, err := (&AgentsQuotaLookup{pool: pool}).byAgent(ctx, pe, []string{humanSurvivor, agentSurvivor, cutAgent})
	if err != nil {
		t.Fatalf("lookup quotas: got = %v, want = nil", err)
	}
	if g := quotas[humanSurvivor].guaranteed; g <= 4.0/2 {
		t.Errorf("human survivor guaranteed after credit = %v, want > its pre-credit share (credit landed in the wrong column, or not at all)", g)
	}
	if g := quotas[agentSurvivor].guaranteed; g <= 4.0/2 {
		t.Errorf("agent survivor guaranteed after credit = %v, want > its pre-credit share — quota tier no longer depends on AgentKind", g)
	}
	if quotas[humanSurvivor].guaranteed != quotas[agentSurvivor].guaranteed {
		t.Errorf("guaranteed after credit = human %v, agent %v; want equal", quotas[humanSurvivor].guaranteed, quotas[agentSurvivor].guaranteed)
	}
	if g, b := quotas[cutAgent].guaranteed, quotas[cutAgent].burst; g != 0 || b != 0 {
		t.Errorf("cut agent quota = (%v, %v), want (0, 0) — cut zeroes both columns", g, b)
	}
}

// A burst-only agent's allocation and spend both live in the burst column, so that is where its
// unspent hours are read from when it is cut.
func TestAdvanceStageReleasesACutBurstOnlyAgentsUnspentHours(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	survivorA, survivorB, cutAgent, pe := "a-"+suffix, "b-"+suffix, "cut-"+suffix, "pe-"+suffix
	ids := []string{survivorA, survivorB, cutAgent}
	createTestPE(t, pool, pe, 10)
	pes := NewPlatformExperimentsStore(pool)
	for _, id := range ids {
		createTestAgent(t, pool, id)
		if _, err := pes.Signup(ctx, pe, domain.AgentSignup{AgentID: id, Role: domain.SignupRoleCompetitor, QuotaTier: domain.QuotaTierBurstOnly}); err != nil {
			t.Fatalf("signup %s: got = %v, want = nil", id, err)
		}
	}
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe, time.Hour, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		return allocateStartQuotas(pe, 4, participants), oneStage, nil
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}
	lookup := &AgentsQuotaLookup{pool: pool}
	before, err := lookup.byAgent(ctx, pe, ids)
	if err != nil {
		t.Fatalf("lookup quotas: got = %v, want = nil", err)
	}

	const usedBurst = 0.5
	dims := []StageRedistribution{{
		ResourceType: domain.ResourceAcceleratorHours, Budget: 10, ReleaseFrac: 0.6,
		UsedByAgent:      map[string]float64{cutAgent: 0},
		UsedBurstByAgent: map[string]float64{cutAgent: usedBurst},
	}}
	advanced, err := pes.AdvanceStage(ctx, pe, 1, []string{cutAgent}, []string{survivorA, survivorB}, dims)
	if err != nil || !advanced {
		t.Fatalf("advance stage: got = (%v, %v), want = (true, nil)", advanced, err)
	}
	after, err := lookup.byAgent(ctx, pe, ids)
	if err != nil {
		t.Fatalf("lookup quotas: got = %v, want = nil", err)
	}

	wantCredit := (10*0.6 + before[cutAgent].burst - usedBurst) / 2
	for _, id := range []string{survivorA, survivorB} {
		if got := after[id].burst - before[id].burst; math.Abs(got-wantCredit) > 1e-6 {
			t.Errorf("%s burst credit = %v, want %v (stage share plus the cut agent's unspent burst)", id, got, wantCredit)
		}
		if g := after[id].guaranteed; g != 0 {
			t.Errorf("%s guaranteed = %v, want 0 for a burst-only survivor", id, g)
		}
	}
}

// AgentsQuotaLookup is a tiny test-only helper reading both quota columns back for assertions.
type AgentsQuotaLookup struct{ pool *Pool }

type quotaRow struct{ guaranteed, burst float64 }

func (l *AgentsQuotaLookup) byAgent(ctx context.Context, peID string, agentIDs []string) (map[string]quotaRow, error) {
	rows, err := l.pool.pool.Query(ctx,
		`SELECT agent_id, guaranteed_accelerator_hours, burst_accelerator_hours FROM agent_quotas
		 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, peID, agentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]quotaRow, len(agentIDs))
	for rows.Next() {
		var id string
		var r quotaRow
		if err := rows.Scan(&id, &r.guaranteed, &r.burst); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}
