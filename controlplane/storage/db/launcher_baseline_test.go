package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A launcher's baseline agent has nothing left to do once its platform experiment's baseline is
// declared, and an agent of another role is not affected.
func TestALaunchersBaselineAgentIsDoneOnceTheBaselineIsDeclared(t *testing.T) {
	pool := eventsTestDB(t)
	pe, pes := poolRun(t, pool, 4, domain.AgentPool{{AgentType: "generalist", Model: "model-1", Count: 4}})
	executor := spawnExecutor(t, pes, pe, "generalist")
	baseline, _ := domain.NewID(domain.AgentIDPrefix)
	if err := pes.SpawnAgent(context.Background(), pe, &domain.Agent{ID: baseline, Name: "b-" + baseline, CreatedAt: time.Now().UTC()},
		domain.AgentSignup{Actor: testActor, AgentID: baseline, Role: domain.SignupRoleBaseline, InAgentSandbox: true, AgentType: "baseline", Model: "model-1"}); err != nil {
		t.Fatalf("spawn baseline: got = %v, want = nil", err)
	}
	done := func() map[string]bool {
		out := map[string]bool{}
		for _, agent := range launcherAgentsOf(t, pes, time.Minute, pe) {
			out[agent.AgentID] = agent.BaselineDone
		}
		return out
	}
	if got := done(); len(got) != 1 || got[baseline] {
		t.Fatalf("open without a baseline: got = %v, want the baseline agent alone, not done", got)
	}
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', baseline_job_id = 'job-1' WHERE id = $1`, pe)
	if got := done(); len(got) != 2 || !got[baseline] || got[executor] {
		t.Fatalf("running with a baseline: got = %v, want the baseline agent done and the executor not", got)
	}
}
