package podexec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestResolveHostMounts(t *testing.T) {
	t.Run("nil/empty input resolves to nil", func(t *testing.T) {
		got, err := resolveHostMounts(nil)
		if err != nil || got != nil {
			t.Fatalf("resolveHostMounts(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("existing directory resolves through unchanged", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveHostMounts(map[string]string{"/data/dataset": dir})
		if err != nil {
			t.Fatalf("resolveHostMounts: unexpected error: %v", err)
		}
		if got["/data/dataset"] != dir {
			t.Fatalf("resolveHostMounts = %v, want /data/dataset -> %s", got, dir)
		}
	})

	t.Run("missing host path fails fast rather than silently mounting nothing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := resolveHostMounts(map[string]string{"/data/dataset": missing})
		if err == nil {
			t.Fatal("resolveHostMounts: expected an error for a nonexistent host path, got nil")
		}
	})

	t.Run("host path that is a file, not a directory, fails", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "not-a-dir")
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		defer f.Close()
		if _, err := resolveHostMounts(map[string]string{"/data/dataset": f.Name()}); err == nil {
			t.Fatal("resolveHostMounts: expected an error for a host path that is a file, got nil")
		}
	})
}

// TestHashContainerSpecDetectsReadOnlyMountsDrift guards against a regression where
// ReadOnlyMounts was omitted from hashContainerSpec's input: reconcile compares this hash
// against the running container's desired-spec label to decide whether to recreate it, so a
// change to a job's desired host mounts (different host path or container target) must change
// the hash, or the running container silently keeps stale/wrong mounts forever.
func TestHashContainerSpecDetectsReadOnlyMountsDrift(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		ReadOnlyMounts: map[string]string{
			"/data/dataset": "/host/dataset-v1",
		},
	}
	changed := base
	changed.ReadOnlyMounts = map[string]string{
		"/data/dataset": "/host/dataset-v2",
	}

	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatalf("hashContainerSpec(base): unexpected error: %v", err)
	}
	changedHash, err := hashContainerSpec(changed)
	if err != nil {
		t.Fatalf("hashContainerSpec(changed): unexpected error: %v", err)
	}
	if baseHash == changedHash {
		t.Fatal("hashContainerSpec: hash unchanged when ReadOnlyMounts changed — reconcile will never detect this drift")
	}
}

func maxRetriesPtr(v int) *int { return &v }

// The same fact the k8s runtime gets wrong when it reads the experiment total: a container is
// handed job.accelerator_count devices, so that is the only number HYPOTHESISLOOP_ACCELERATOR_COUNT
// can truthfully carry. This runtime is single-node today, which makes per-node and total equal
// by accident -- the assertion pins the source, so the next multi-node change cannot silently
// inherit the k8s bug.
func TestBuildContainerSpecInjectsPerNodeAcceleratorCount(t *testing.T) {
	e := &Executor{
		apiURL:                               "http://example.invalid:8083",
		defaultTerminationGracePeriodSeconds: 5,
		maxTerminationGracePeriodSeconds:     30,
	}
	exp := &domain.Experiment{
		ID: "per-node-count", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		// Deliberately disagreeing with the spec: only reading the spec produces "2".
		AcceleratorCount:       8,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: maxRetriesPtr(0), AcceleratorCount: 2,
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	cs, err := e.BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0", "/dev/tenstorrent/1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.Env["HYPOTHESISLOOP_ACCELERATOR_COUNT"]; got != "2" {
		t.Fatalf("HYPOTHESISLOOP_ACCELERATOR_COUNT = %q, want %q (per node, not the 8-device job total)", got, "2")
	}
}
