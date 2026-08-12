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
