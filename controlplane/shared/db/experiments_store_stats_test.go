package db

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestExperimentStatsGroupsByAgentAndHypothesis(t *testing.T) {
	pool := quotaTierTestDB(t)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	peID, agentA, agentB := "pe-stats-"+suffix, "agent-stats-a-"+suffix, "agent-stats-b-"+suffix
	createTestPE(t, pool, peID, 1000)
	createTestAgent(t, pool, agentA)
	createTestAgent(t, pool, agentB)

	for _, job := range []struct {
		id, agent, hypothesis string
		accelerators          int
		status                domain.ExperimentStatus
	}{
		{"a-running", agentA, "h1", 2, domain.StatusRunning},
		{"a-submitted", agentA, "h1", 4, domain.StatusSubmitted},
		{"a-completed", agentA, "h2", 8, domain.StatusCompleted},
		{"b-queued", agentB, "h2", 1, domain.StatusQueued},
		{"b-evicted", agentB, "h3", 1, domain.StatusEvicted},
	} {
		id := job.id + "-" + suffix
		seedExperimentForCapTest(t, pool, id, job.agent, peID, job.accelerators, job.status)
		if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET hypothesis_id=$2 WHERE id=$1`, id, job.hypothesis); err != nil {
			t.Fatalf("set hypothesis for %s: got = %v, want = nil", id, err)
		}
	}
	store := NewExperimentsStore(pool)
	filter := domain.ExperimentFilter{Scope: domain.AllTenants, PlatformExperimentID: peID}

	plain, err := store.ExperimentStats(ctx, filter, "")
	if err != nil {
		t.Fatalf("ExperimentStats: got = %v, want = nil", err)
	}
	if plain.Total != 5 || plain.InFlightAccelerators != 6 || plain.Groups != nil {
		t.Errorf("ungrouped: total = %d, in flight = %d, groups = %v, want 5, 6, nil", plain.Total, plain.InFlightAccelerators, plain.Groups)
	}

	byAgent, err := store.ExperimentStats(ctx, filter, "agent")
	if err != nil {
		t.Fatalf("ExperimentStats by agent: got = %v, want = nil", err)
	}
	if byAgent.Total != 5 || len(byAgent.Groups) != 2 {
		t.Fatalf("by agent: total = %d, groups = %d, want 5, 2", byAgent.Total, len(byAgent.Groups))
	}
	a, b := byAgent.Groups[agentA], byAgent.Groups[agentB]
	wantA := map[string]int{"RUNNING": 1, "SUBMITTED": 1, "COMPLETED": 1}
	if a.Total != 3 || a.InFlightAccelerators != 6 || !reflect.DeepEqual(a.ByStatus, wantA) {
		t.Errorf("agent a: total = %d, in flight = %d, by status = %v, want 3, 6, %v", a.Total, a.InFlightAccelerators, a.ByStatus, wantA)
	}
	if b.Total != 2 || b.InFlightAccelerators != 0 || b.EvictionsByReason["unknown"] != 1 {
		t.Errorf("agent b: total = %d, in flight = %d, evictions = %v, want 2, 0, unknown:1", b.Total, b.InFlightAccelerators, b.EvictionsByReason)
	}

	byHypothesis, err := store.ExperimentStats(ctx, filter, "hypothesis")
	if err != nil {
		t.Fatalf("ExperimentStats by hypothesis: got = %v, want = nil", err)
	}
	want := map[string]map[string]int{
		"h1": {"RUNNING": 1, "SUBMITTED": 1},
		"h2": {"COMPLETED": 1, "QUEUED": 1},
		"h3": {"EVICTED": 1},
	}
	got := map[string]map[string]int{}
	for key, group := range byHypothesis.Groups {
		got[key] = group.ByStatus
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("by hypothesis: got = %v, want = %v", got, want)
	}

	if _, err := store.ExperimentStats(ctx, filter, "status; DROP TABLE experiments"); err == nil {
		t.Error("unknown group field: got = nil, want an error")
	}
}
