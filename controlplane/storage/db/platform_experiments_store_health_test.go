package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// An agent and the coordinator relaunched under another image keep counting their restarts: the
// budget is whole again under the new launch, and the run's health counts every one.
func TestRunHealthCountsEveryRestartOfAnAgentAndTheCoordinatorAcrossLaunches(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe := createTestPEWithCoordinator(t, pool)
	agent := spawnTestAgents(t, pes, pe, domain.AgentSignup{Actor: testActor, InAgentSandbox: true, AgentType: "generalist"})[0]
	for i, launch := range []string{"launch-1", "launch-1", "launch-2"} {
		for _, id := range []string{agent, ""} {
			name := uuid.New().String()[:8]
			if err := pes.AskInstance(ctx, pe, id, name, launch); err != nil {
				t.Fatal(err)
			}
			if _, recorded, err := pes.EndInstance(ctx, pe, id, name, InstanceExited, "exit", time.Minute, domain.MaxAgentInstanceRestarts); err != nil || !recorded {
				t.Fatalf("exit %d of %q: recorded %v, %v", i, id, recorded, err)
			}
		}
	}
	health, err := pes.RunHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restarts := map[string]int{}
	for _, a := range health {
		if a.PlatformExperimentID == pe {
			restarts[a.Role+"/"+a.AgentID] = a.Restarts
		}
	}
	want := map[string]int{string(domain.SignupRoleExecutor) + "/" + agent: 3, domain.CoordinatorHealthRole + "/": 3}
	if !reflect.DeepEqual(restarts, want) {
		t.Errorf("restarts = %v, want %v", restarts, want)
	}
}
