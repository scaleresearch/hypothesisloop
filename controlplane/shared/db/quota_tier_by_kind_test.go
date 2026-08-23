package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// quotaTierTestDB is eventsTestDB: the same dev database, and crucially the same installation of
// every ADD COLUMN IF NOT EXISTS in schema.sql. These tests read columns (agents.kind,
// experiment_signups.quota_tier) that a development database a few migrations behind will not
// have, and without that step they fail as a missing column rather than as a wrong allocation.
func quotaTierTestDB(t *testing.T) *Pool {
	t.Helper()
	return eventsTestDB(t)
}

func createTestAgent(t *testing.T, pool *Pool, id string, kind domain.AgentKind) {
	t.Helper()
	agents := NewAgentsStore(pool)
	if err := agents.CreateAgent(context.Background(), &domain.Agent{
		ID: id, Name: id, Kind: kind, PerformanceScore: 0.5, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create agent %s: got = %v, want = nil", id, err)
	}
}

func createTestPE(t *testing.T, pool *Pool, id string, budget float64) {
	t.Helper()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{
		ID: id, Name: id, Description: "quota tier test", BudgetAcceleratorHours: budget,
		MaxAgents: 10, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics:               []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}},
		ReportIntervalSeconds: 30,
		Stages:                []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		CurrentStage:          1,
		// The store writes these through to CHECK-constrained columns. The service resolves the
		// empty default (domain.ParseSubmitterPolicy) before it ever gets here, so a caller
		// building the row itself has to resolve it too.
		HypothesisSubmitPolicy: domain.SubmitterPolicyMixed,
		JobSubmitPolicy:        domain.SubmitterPolicyMixed,
	}
	if err := pes.CreatePlatformExperiment(context.Background(), pe); err != nil {
		t.Fatalf("create platform experiment: got = %v, want = nil", err)
	}
}

// allocateStartQuotas mirrors services/quota.PlatformExperimentsService.Start's per-dimension
// allocation, without pulling in the whole service (no metrics dependency needed for this path)
// — the same domain.AllocateQuota + domain.ResolveQuotaTier/ApplyQuotaTier sequence production runs.
func allocateStartQuotas(peID string, budget float64, participants []StartParticipant) []*domain.AgentQuota {
	cfg := domain.QuotaConfig{BurstFraction: 0.5}
	out := make([]*domain.AgentQuota, 0, len(participants))
	for _, p := range participants {
		g, b := domain.AllocateQuota(budget, len(participants), 0, 0, cfg)
		g, b = domain.ApplyQuotaTier(domain.ResolveQuotaTier(p.Kind, p.QuotaTierOverride), g, b)
		out = append(out, &domain.AgentQuota{
			ID: uuid.New().String(), AgentID: p.AgentID, PlatformExperimentID: peID,
			GuaranteedAcceleratorHours: g, BurstAcceleratorHours: b, CreatedAt: time.Now().UTC(),
		})
	}
	return out
}

func TestStartGivesHumansGuaranteedQuotaAndAgentsBurstOnly(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	human, agent, pe := "human-"+suffix, "agent-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, human, domain.AgentKindHuman)
	createTestAgent(t, pool, agent, domain.AgentKindAgent)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	if _, err := pes.Signup(ctx, pe, human, domain.SignupRoleCompetitor, ""); err != nil {
		t.Fatalf("signup human: got = %v, want = nil", err)
	}
	if _, err := pes.Signup(ctx, pe, agent, domain.SignupRoleCompetitor, ""); err != nil {
		t.Fatalf("signup agent: got = %v, want = nil", err)
	}

	var seenKinds map[string]domain.AgentKind
	started, quotas, err := pes.StartPlatformExperimentTx(ctx, pe, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		seenKinds = make(map[string]domain.AgentKind, len(participants))
		for _, p := range participants {
			seenKinds[p.AgentID] = p.Kind
		}
		return allocateStartQuotas(pe, 4, participants), nil // 4 = budget * stage-1 explore fraction, arbitrary here
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}
	if seenKinds[human] != domain.AgentKindHuman {
		t.Errorf("StartParticipant.Kind for %s = %q, want %q — the join in StartPlatformExperimentTx must resolve real kinds", human, seenKinds[human], domain.AgentKindHuman)
	}
	if seenKinds[agent] != domain.AgentKindAgent {
		t.Errorf("StartParticipant.Kind for %s = %q, want %q", agent, seenKinds[agent], domain.AgentKindAgent)
	}

	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}
	if g := byAgent[human].GuaranteedAcceleratorHours; g <= 0 {
		t.Errorf("human guaranteed = %v, want > 0", g)
	}
	if g := byAgent[agent].GuaranteedAcceleratorHours; g != 0 {
		t.Errorf("agent guaranteed = %v, want 0 — agents are burst-only", g)
	}
	if b := byAgent[agent].BurstAcceleratorHours; b <= 0 {
		t.Errorf("agent burst = %v, want > 0 — the agent's would-be guaranteed share must land here instead", b)
	}
}

// The feature this file is really about: a signup-time override lets one experiment mix tiers
// however it wants, independent of AgentKind — an agent explicitly granted guaranteed quota, and
// a human explicitly restricted to burst-only, in the same run.
func TestSignupQuotaTierOverrideWinsOverAgentKind(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentGuaranteed, humanBurstOnly, pe := "agent-guar-"+suffix, "human-burst-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, agentGuaranteed, domain.AgentKindAgent)
	createTestAgent(t, pool, humanBurstOnly, domain.AgentKindHuman)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	if _, err := pes.Signup(ctx, pe, agentGuaranteed, domain.SignupRoleCompetitor, domain.QuotaTierGuaranteed); err != nil {
		t.Fatalf("signup agent with guaranteed override: got = %v, want = nil", err)
	}
	if _, err := pes.Signup(ctx, pe, humanBurstOnly, domain.SignupRoleCompetitor, domain.QuotaTierBurstOnly); err != nil {
		t.Fatalf("signup human with burst_only override: got = %v, want = nil", err)
	}

	started, quotas, err := pes.StartPlatformExperimentTx(ctx, pe, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		return allocateStartQuotas(pe, 4, participants), nil
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}

	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}
	if g := byAgent[agentGuaranteed].GuaranteedAcceleratorHours; g <= 0 {
		t.Errorf("agent explicitly granted guaranteed: guaranteed = %v, want > 0 despite AgentKindAgent", g)
	}
	if g := byAgent[humanBurstOnly].GuaranteedAcceleratorHours; g != 0 {
		t.Errorf("human explicitly restricted to burst_only: guaranteed = %v, want 0 despite AgentKindHuman", g)
	}
	if b := byAgent[humanBurstOnly].BurstAcceleratorHours; b <= 0 {
		t.Errorf("human explicitly restricted to burst_only: burst = %v, want > 0", b)
	}
}

func TestAdvanceStageCreditsSurvivorsIntoTheTierTheirKindAllows(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	humanSurvivor, agentSurvivor, cutAgent, pe := "human-"+suffix, "agent-"+suffix, "cut-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, humanSurvivor, domain.AgentKindHuman)
	createTestAgent(t, pool, agentSurvivor, domain.AgentKindAgent)
	createTestAgent(t, pool, cutAgent, domain.AgentKindAgent)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	for _, id := range []string{humanSurvivor, agentSurvivor, cutAgent} {
		if _, err := pes.Signup(ctx, pe, id, domain.SignupRoleCompetitor, ""); err != nil {
			t.Fatalf("signup %s: got = %v, want = nil", id, err)
		}
	}
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		return allocateStartQuotas(pe, 4, participants), nil
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
	if g := quotas[agentSurvivor].guaranteed; g != 0 {
		t.Errorf("agent survivor guaranteed after credit = %v, want 0 — the stage-boundary credit must never regrant guaranteed quota to an agent", g)
	}
	if b := quotas[agentSurvivor].burst; b <= 2.0 {
		t.Errorf("agent survivor burst after credit = %v, want > its pre-credit burst (the release must land here instead)", b)
	}
	if g, b := quotas[cutAgent].guaranteed, quotas[cutAgent].burst; g != 0 || b != 0 {
		t.Errorf("cut agent quota = (%v, %v), want (0, 0) — cut zeroes both columns", g, b)
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

func TestDonationToAnAgentRecipientLandsEntirelyInBurst(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	donorHuman, recipientAgent, pe := "donor-"+suffix, "recipient-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, donorHuman, domain.AgentKindHuman)
	createTestAgent(t, pool, recipientAgent, domain.AgentKindAgent)
	createTestPE(t, pool, pe, 10)

	pes := NewPlatformExperimentsStore(pool)
	for _, id := range []string{donorHuman, recipientAgent} {
		if _, err := pes.Signup(ctx, pe, id, domain.SignupRoleCompetitor, ""); err != nil {
			t.Fatalf("signup %s: got = %v, want = nil", id, err)
		}
	}
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		return allocateStartQuotas(pe, 8, participants), nil
	})
	if err != nil || !started {
		t.Fatalf("start: got = (%v, %v), want = (true, nil)", started, err)
	}

	donationID := uuid.New().String()
	if _, err := pool.pool.Exec(ctx,
		`INSERT INTO donation_requests (id, agent_id, platform_experiment_id, credits_want, reason, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'test', 'open', now(), now())`,
		donationID, recipientAgent, pe, 1.0,
	); err != nil {
		t.Fatalf("insert donation request: got = %v, want = nil", err)
	}

	fulfilled, err := pes.FulfillDonationTx(ctx, donationID, donorHuman, recipientAgent, pe,
		domain.ResourceAcceleratorHours, 1.0, 0.5,
		func(context.Context) (*domain.AgentQuota, error) {
			return &domain.AgentQuota{AgentID: donorHuman, PlatformExperimentID: pe}, nil // zero observed usage
		})
	if err != nil || !fulfilled {
		t.Fatalf("fulfill donation: got = (%v, %v), want = (true, nil)", fulfilled, err)
	}

	quotas, err := (&AgentsQuotaLookup{pool: pool}).byAgent(ctx, pe, []string{recipientAgent})
	if err != nil {
		t.Fatalf("lookup quota: got = %v, want = nil", err)
	}
	recipient := quotas[recipientAgent]
	if recipient.guaranteed != 0 {
		t.Errorf("agent recipient guaranteed after donation = %v, want 0 — a human's donation must not grant an agent priority capacity", recipient.guaranteed)
	}
	if recipient.burst <= 4.0 { // pre-donation burst share was ~4 (8 AccH / 2 participants), donation adds 1.5 more
		t.Errorf("agent recipient burst after donation = %v, want it to have grown by the full donated amount", recipient.burst)
	}
}
