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
	if _, err := NewPlatformExperimentsStore(pool).Signup(ctx, pe, in, domain.SignupRoleCompetitor, ""); err != nil {
		t.Fatalf("signup: %v", err)
	}

	agents := NewAgentsStore(pool)
	for _, tc := range []struct {
		name   string
		filter AgentsFilter
		want   []string
	}{
		{"signed up", AgentsFilter{PlatformExperimentID: pe}, []string{in}},
		{"search, any case", AgentsFilter{Search: "BETA-" + suffix}, []string{out}},
		{"search across both, newest first", AgentsFilter{Search: suffix}, []string{out, in}},
		{"signed up and search miss", AgentsFilter{PlatformExperimentID: pe, Search: "beta"}, nil},
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
		total, err := agents.CountAgents(ctx, tc.filter)
		if err != nil || total != len(tc.want) {
			t.Fatalf("%s: count = (%d, %v), want %d", tc.name, total, err, len(tc.want))
		}
	}
}
