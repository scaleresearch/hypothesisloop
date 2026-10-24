package agentsandbox

import (
	"testing"

	sandboxapi "github.com/scaleresearch/hypothesisloop/contract/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// Once the baseline is declared an agent of role baseline has nothing left to do. A machine of it
// that runs is told to stop and not replaced, and one that stopped is not asked for again when
// the image or the launcher changes: it would build and record the experiment's image again,
// and the executors' jobs would run in another image than the baseline's.
func TestABaselineAgentGetsNoMachineOnceTheBaselineIsDeclared(t *testing.T) {
	baseline := func(signup domain.AgentSignup) db.LauncherAgent {
		signup.Role = domain.SignupRoleBaseline
		return db.LauncherAgent{AgentSignup: signup}
	}
	store := &launcherStore{ByLauncher: map[string][]db.LauncherAgent{"main": {baseline(sandboxed("pe-1", "ag-running")), baseline(sandboxed("pe-1", "ag-stopped"))}}}
	first := mustReconcile(t, store, observed())
	if len(first.Instances) != 2 {
		t.Fatalf("before the baseline: %+v, want a machine for each baseline agent", first.Instances)
	}
	running := store.Find("pe-1", "ag-running").Name
	answer := first
	for !store.Find("pe-1", "ag-stopped").Stopped {
		var report []sandboxapi.ObservedInstance
		for _, instance := range answer.Instances {
			if instance.Name == running {
				report = append(report, sandboxapi.ObservedInstance{Name: instance.Name, State: "running"})
			} else {
				report = append(report, sandboxapi.ObservedInstance{Name: instance.Name, State: "exited", Log: "crashed"})
			}
		}
		answer = mustReconcile(t, store, observed(report...))
	}

	for i := range store.ByLauncher["main"] {
		store.ByLauncher["main"][i].BaselineDone = true
	}
	rotated := observed(sandboxapi.ObservedInstance{Name: running, State: "running"})
	rotated.LaunchDigest = "fedcba9876543210fedcba9876543210"
	for range 2 {
		answer = mustReconcile(t, store, rotated)
		if len(answer.Instances) != 0 || len(answer.Ending) != 1 || answer.Ending[0] != running {
			t.Fatalf("after the baseline was declared and the launch changed: %+v, want only %s ending", answer, running)
		}
	}
	if recorded := store.Find("pe-1", "ag-stopped"); !recorded.Stopped || recorded.Name != "" {
		t.Fatalf("stopped baseline agent recorded %+v, want it left stopped", recorded)
	}
}
