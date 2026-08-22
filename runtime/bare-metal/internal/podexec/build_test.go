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
		Data: testDataAccess(),
		ID:   "per-node-count", AgentID: "agent", ProjectID: "project",
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

// Every job in every test gets the same durable-data access the control plane really sends,
// because BuildJob refuses to build a job without one: a job with nowhere to save is a job whose
// result cannot outlive it, and that must fail at build time rather than at the end of a run.
func testDataAccess() *domain.DataAccess {
	return &domain.DataAccess{
		URI: "s3://hl-data/pe-1/agent/exp-1/", Shared: "s3://hl-data/pe-1/",
		Endpoint: "http://store.invalid:9000", Region: "us-east-1",
		AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "session-token",
	}
}

// The same two variables the k8s runtime injects, from the same place. A checkpoint written by a
// job on a bare node has to be readable by a job the scheduler later places in a k8s cluster, so
// the address a job is handed can depend on desired state and on nothing about its runtime.
func TestBuildContainerSpecInjectsDurableDataAddressFromDesiredState(t *testing.T) {
	e := &Executor{
		apiURL:                               "http://example.invalid:8083",
		defaultTerminationGracePeriodSeconds: 5,
		maxTerminationGracePeriodSeconds:     30,
	}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "data-env", AgentID: "agent", ProjectID: "project",
		AcceleratorType:        "tenstorrent.com/chipArch=blackhole",
		AcceleratorCount:       1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: maxRetriesPtr(0), AcceleratorCount: 1,
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	cs, err := e.BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.Env["HYPOTHESISLOOP_DATA_URI"]; got != exp.Data.URI {
		t.Fatalf("HYPOTHESISLOOP_DATA_URI = %q, want %q (the job's own writable prefix, straight from desired state)", got, exp.Data.URI)
	}
	if got := cs.Env["HYPOTHESISLOOP_DATA_SHARED"]; got != exp.Data.Shared {
		t.Fatalf("HYPOTHESISLOOP_DATA_SHARED = %q, want %q (the platform experiment's readable prefix, straight from desired state)", got, exp.Data.Shared)
	}
	if got := cs.Env["AWS_SECRET_ACCESS_KEY"]; got != exp.Data.SecretAccessKey {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want %q — an address the job has no credentials for is not an address", got, exp.Data.SecretAccessKey)
	}
	if got := cs.Env["AWS_SESSION_TOKEN"]; got != exp.Data.SessionToken {
		t.Fatalf("AWS_SESSION_TOKEN = %q, want %q — without the session token the scoped credentials are unusable", got, exp.Data.SessionToken)
	}
}

// Same reason as the k8s runtime: a fresh session arrives on every reconcile pass, and reconcile
// compares this hash against the running container's label to decide whether to recreate it. A
// hashed session means every running container is destroyed and rebuilt every few seconds.
func TestHashContainerSpecIgnoresRotatingDurableDataCredentials(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env: map[string]string{
			"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-1/",
			"AWS_ACCESS_KEY_ID":       "key",
			"AWS_SECRET_ACCESS_KEY":   "secret",
			"AWS_SESSION_TOKEN":       "session-token",
		},
	}
	rotated := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env: map[string]string{
			"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-1/",
			"AWS_ACCESS_KEY_ID":       "next-key",
			"AWS_SECRET_ACCESS_KEY":   "next-secret",
			"AWS_SESSION_TOKEN":       "next-session-token",
		},
	}

	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatalf("hashContainerSpec(base): unexpected error: %v", err)
	}
	rotatedHash, err := hashContainerSpec(rotated)
	if err != nil {
		t.Fatalf("hashContainerSpec(rotated): unexpected error: %v", err)
	}
	if baseHash != rotatedHash {
		t.Fatal("hashContainerSpec: a rotated durable-data session changed the hash — reconcile will recreate every running container every pass")
	}
}

// The prefix itself is desired state, and a container left writing to a prefix the control plane
// has moved is exactly the drift this hash exists to catch.
func TestHashContainerSpecDetectsDurableDataPrefixDrift(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-1/"},
	}
	moved := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-2/"},
	}

	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatalf("hashContainerSpec(base): unexpected error: %v", err)
	}
	movedHash, err := hashContainerSpec(moved)
	if err != nil {
		t.Fatalf("hashContainerSpec(moved): unexpected error: %v", err)
	}
	if baseHash == movedHash {
		t.Fatal("hashContainerSpec: hash unchanged when the job's data prefix changed — reconcile will never repoint it")
	}
}
