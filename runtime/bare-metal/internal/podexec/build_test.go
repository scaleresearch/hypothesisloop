package podexec

import (
	"os"
	"path/filepath"
	"testing"
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
