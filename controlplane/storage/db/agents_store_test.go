package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestListAgentsFiltersBySignupAndSearch(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	in, out, pe := "alpha-"+suffix, "beta-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, in)
	createTestAgent(t, pool, out)
	createTestPE(t, pool, pe, 10)
	if _, err := NewStore(pool, 3).Signup(ctx, pe, domain.AgentSignup{Actor: testActor, AgentID: in, Role: domain.SignupRoleCompetitor}); err != nil {
		t.Fatalf("signup: %v", err)
	}

	agents := NewStore(pool, 3)
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

// Both lists cut a limit over 200 down to 200 without saying so. The handlers refuse one now, and
// the store reads the page it is asked for.
func TestTheAgentAndPlatformExperimentListsReadThePageAsked(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	const rows = MaxAgentListLimit + 1
	for i := range rows {
		createTestAgent(t, pool, fmt.Sprintf("paged-%s-%d", suffix, i))
		createTestPE(t, pool, fmt.Sprintf("pe-paged-%s-%d", suffix, i), 10)
	}

	agents, err := NewStore(pool, 3).ListAgents(ctx, AgentsFilter{Scope: domain.AllTenants, Search: "paged-" + suffix, Limit: rows})
	if err != nil || len(agents) != rows {
		t.Errorf("agents with limit %d: got = %d (%v), want = %d", rows, len(agents), err, rows)
	}
	pes, err := NewStore(pool, 3).ListPlatformExperiments(ctx, PlatformExperimentsFilter{Scope: domain.AllTenants, Search: "pe-paged-" + suffix, Limit: rows})
	if err != nil || len(pes) != rows {
		t.Errorf("platform experiments with limit %d: got = %d (%v), want = %d", rows, len(pes), err, rows)
	}
	agents, err = NewStore(pool, 3).ListAgents(ctx, AgentsFilter{Scope: domain.AllTenants, Search: "paged-" + suffix})
	if err != nil || len(agents) != defaultAgentListLimit {
		t.Errorf("agents with no limit: got = %d (%v), want = %d", len(agents), err, defaultAgentListLimit)
	}
	pes, err = NewStore(pool, 3).ListPlatformExperiments(ctx, PlatformExperimentsFilter{Scope: domain.AllTenants, Search: "pe-paged-" + suffix})
	if err != nil || len(pes) != defaultPlatformExperimentListLimit {
		t.Errorf("platform experiments with no limit: got = %d (%v), want = %d", len(pes), err, defaultPlatformExperimentListLimit)
	}
}

// A taken id was told from the text of the database's error.
func TestAnAgentIDThatIsTakenIsRefusedAsExisting(t *testing.T) {
	pool := eventsTestDB(t)
	id := "veteran-" + uuid.New().String()[:8]
	createTestAgent(t, pool, id)
	err := NewStore(pool, 3).CreateAgent(context.Background(), &domain.Agent{ID: id, Name: "again", OrgID: "default", CreatedAt: time.Now().UTC()})
	if !errors.Is(err, ErrAgentExists) {
		t.Fatalf("a second agent of id %s: %v, want ErrAgentExists", id, err)
	}
}

func TestAgentStatsCountsTop3OverTheWholeSet(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	placed, unplaced, pe := "placed-"+suffix, "unplaced-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, placed)
	createTestAgent(t, pool, unplaced)
	createTestPE(t, pool, pe, 10)
	if _, err := pool.pool.Exec(ctx, `INSERT INTO experiment_top3 (platform_experiment_id, agent_id, final_metric) VALUES ($1, $2, 1)`, pe, placed); err != nil {
		t.Fatalf("insert top3: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiment_top3 WHERE platform_experiment_id = $1`, pe)
	})

	got, err := NewStore(pool, 3).AgentStats(ctx, AgentsFilter{Scope: domain.AllTenants, Search: suffix})
	want := domain.AgentStats{Total: 2, Top3Eligible: 1}
	if err != nil || *got != want {
		t.Fatalf("stats = (%+v, %v), want %+v", got, err, want)
	}
}
