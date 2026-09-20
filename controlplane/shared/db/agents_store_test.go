package db

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestListAgentsFiltersBySignupAndSearch(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	in, out, pe := "alpha-"+suffix, "beta-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, in, domain.AgentKindAgent)
	createTestAgent(t, pool, out, domain.AgentKindAgent)
	createTestPE(t, pool, pe, 10)
	if _, err := NewPlatformExperimentsStore(pool).Signup(ctx, pe, domain.AgentSignup{AgentID: in, Role: domain.SignupRoleCompetitor}); err != nil {
		t.Fatalf("signup: %v", err)
	}

	agents := NewAgentsStore(pool)
	for _, tc := range []struct {
		name   string
		filter AgentsFilter
		want   []string
	}{
		{"signed up", AgentsFilter{Scope: domain.AllTenants, PlatformExperimentID: pe}, []string{in}},
		{"search, any case", AgentsFilter{Scope: domain.AllTenants, Search: "BETA-" + suffix}, []string{out}},
		{"search across both, newest first", AgentsFilter{Scope: domain.AllTenants, Search: suffix}, []string{out, in}},
		{"signed up and search miss", AgentsFilter{Scope: domain.AllTenants, PlatformExperimentID: pe, Search: "beta"}, nil},
	} {
		got, err := agents.ListAgents(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: list: %v", tc.name, err)
		}
		var ids []string
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		if !slices.Equal(ids, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, ids, tc.want)
		}
		stats, err := agents.AgentStats(ctx, tc.filter)
		if err != nil || stats.Total != len(tc.want) {
			t.Fatalf("%s: stats = (%+v, %v), want total %d", tc.name, stats, err, len(tc.want))
		}
	}
}

func TestAgentStatsCountsTop3AndAveragesScoreOverTheWholeSet(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	placed, unplaced, pe := "placed-"+suffix, "unplaced-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, placed, domain.AgentKindAgent)
	createTestAgent(t, pool, unplaced, domain.AgentKindAgent)
	createTestPE(t, pool, pe, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE agents SET performance_score = CASE id WHEN $1 THEN 1 ELSE 0.5 END WHERE id IN ($1, $2)`, placed, unplaced); err != nil {
		t.Fatalf("set scores: %v", err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO experiment_top3 (platform_experiment_id, agent_id, final_metric) VALUES ($1, $2, 1)`, pe, placed); err != nil {
		t.Fatalf("insert top3: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiment_top3 WHERE platform_experiment_id = $1`, pe)
	})

	got, err := NewAgentsStore(pool).AgentStats(ctx, AgentsFilter{Scope: domain.AllTenants, Search: suffix})
	want := domain.AgentStats{Total: 2, Top3Eligible: 1, AvgPerformanceScore: 0.75}
	if err != nil || *got != want {
		t.Fatalf("stats = (%+v, %v), want %+v", got, err, want)
	}
}
