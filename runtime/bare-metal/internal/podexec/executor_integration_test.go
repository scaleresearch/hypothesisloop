//go:build integration

// Run with: go test -tags=integration ./runtime/bare-metal/internal/podexec/... -run Integration -v
//
// Requires a real podman or docker binary and network access to pull "busybox" once. Not run by
// default `go test ./...` since it exercises the host's container engine directly.
package podexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	dir := t.TempDir()
	e, err := New(Config{
		APIURL:                               "http://example.invalid:8083",
		DefaultTerminationGracePeriodSeconds: 5,
		MaxTerminationGracePeriodSeconds:     30,
		MaxCheckpointGraceSeconds:            600,
		ScratchDir:                           dir,
		NodeName:                             "integration-test-node",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func maxRetries(n int) *int { return &n }

func TestIntegrationLifecycle(t *testing.T) {
	e := newTestExecutor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	exp := &domain.Experiment{
		ID:           "it-" + filepath.Base(t.TempDir()),
		AgentID:      "agent-1",
		ProjectID:    "proj-1",
		CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image:      "busybox",
			Command:    []string{"sh", "-c"},
			Args:       []string{"echo hello && sleep 1"},
			CPU:        "0.5",
			Memory:     "64Mi",
			Storage:    "128Mi",
			MaxRetries: maxRetries(0),
		},
	}

	if err := e.CreateWorkload(ctx, exp); err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}
	t.Cleanup(func() {
		_ = e.DeleteWorkload(context.Background(), exp.ID, false)
		_ = e.removeContainer(context.Background(), containerName(exp.ID))
	})

	ids, err := e.ListManagedJobs(ctx)
	if err != nil {
		t.Fatalf("ListManagedJobs: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == exp.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s in ListManagedJobs, got %v", exp.ID, ids)
	}

	matches, err := e.WorkloadMatchesDesired(ctx, exp)
	if err != nil {
		t.Fatalf("WorkloadMatchesDesired: %v", err)
	}
	if !matches {
		t.Fatalf("expected freshly created workload to match desired spec")
	}

	deadline := time.Now().Add(30 * time.Second)
	var phase workload.JobPhase
	for time.Now().Before(deadline) {
		phase, _, err = e.PollJobPhaseAndUID(ctx, exp.ID)
		if err != nil {
			t.Fatalf("PollJobPhaseAndUID: %v", err)
		}
		if phase == workload.JobPhaseSucceeded || phase == workload.JobPhaseFailed {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if phase != workload.JobPhaseSucceeded {
		t.Fatalf("expected phase succeeded, got %v", phase)
	}

	if err := e.DeleteWorkload(ctx, exp.ID, false); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	if err := e.ReapTerminal(ctx, map[string]*domain.Experiment{}); err != nil {
		t.Fatalf("ReapTerminal: %v", err)
	}
	phase, _, err = e.PollJobPhaseAndUID(ctx, exp.ID)
	if err != nil {
		t.Fatalf("PollJobPhaseAndUID after delete: %v", err)
	}
	if phase != workload.JobPhaseGone {
		t.Fatalf("expected phase gone after reap, got %v", phase)
	}
}

func TestIntegrationCapacityReporting(t *testing.T) {
	e := newTestExecutor(t)
	ctx := context.Background()

	if _, err := os.Stat(e.scratchDir); err != nil {
		t.Fatalf("scratch dir missing: %v", err)
	}

	cpuAvail, cpuTotal, err := e.GetLiveCPUCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveCPUCapacity: %v", err)
	}
	if cpuTotal <= 0 || cpuAvail < 0 || cpuAvail > cpuTotal {
		t.Fatalf("implausible cpu capacity available=%v total=%v", cpuAvail, cpuTotal)
	}

	ramAvail, ramTotal, err := e.GetLiveRAMCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveRAMCapacity: %v", err)
	}
	if ramTotal <= 0 || ramAvail < 0 || ramAvail > ramTotal {
		t.Fatalf("implausible ram capacity available=%v total=%v", ramAvail, ramTotal)
	}

	storageAvail, storageTotal, err := e.GetLiveStorageCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveStorageCapacity: %v", err)
	}
	if storageTotal <= 0 || storageAvail < 0 || storageAvail > storageTotal {
		t.Fatalf("implausible storage capacity available=%v total=%v", storageAvail, storageTotal)
	}
}

// TestIntegrationRejectsMultiNode confirms num_nodes>1 is still a hard error: there is no way
// to partially honor distributed placement across multiple bare nodes on a single-node runtime.
func TestIntegrationRejectsMultiNode(t *testing.T) {
	e := newTestExecutor(t)
	spec := domain.JobSpec{
		Image: "busybox", CPU: "0.5", Memory: "64Mi", Storage: "128Mi",
		MaxRetries: maxRetries(0), NumNodes: 2,
	}
	exp := &domain.Experiment{ID: "reject-num-nodes", Job: spec}
	if _, err := e.BuildContainerSpec(exp, Placement{}); err == nil {
		t.Fatal("expected rejection for num_nodes>1")
	}
}

// TestIntegrationIgnoresK8sOnlyFields confirms the three fields that only have meaning under a
// k8s device-plugin/taint model (accelerator_tolerations/extra_resources/
// accelerator_pod_resources) are accepted rather than rejected: the same job spec submitted
// against either runtime must build successfully, since the control plane — not the job spec —
// decides which runtime a job lands on.
func TestIntegrationIgnoresK8sOnlyFields(t *testing.T) {
	e := newTestExecutor(t)
	base := domain.JobSpec{
		Image:      "busybox",
		CPU:        "0.5",
		Memory:     "64Mi",
		Storage:    "128Mi",
		MaxRetries: maxRetries(0),
	}

	cases := []struct {
		name string
		spec domain.JobSpec
	}{
		{"tolerations", func() domain.JobSpec { s := base; s.AcceleratorTolerations = []string{"x"}; return s }()},
		{"extra_resources", func() domain.JobSpec {
			s := base
			s.ExtraResources = map[string]string{"google.com/tpu": "1"}
			return s
		}()},
		{"accelerator_pod_resources", func() domain.JobSpec {
			s := base
			s.AcceleratorPodResources = map[string]string{"hugepages-1Gi": "1Gi"}
			return s
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := &domain.Experiment{ID: "ignore-" + tc.name, Job: tc.spec}
			if _, err := e.BuildContainerSpec(exp, Placement{}); err != nil {
				t.Fatalf("expected %s to be ignored, not rejected: %v", tc.name, err)
			}
		})
	}
}
