package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func productionConfig(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "settings", "hypothesisloop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func loadText(t *testing.T, text string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

func TestProductionConfigIsComplete(t *testing.T) {
	if err := loadText(t, productionConfig(t)); err != nil {
		t.Fatalf("production config: %v", err)
	}
}

func TestLoadRejectsMissingRequiredSchedulerSetting(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  loop_heartbeat_seconds: 10", "  loop_heartbeat_seconds: 0", 1)
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted missing loop heartbeat")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	if err := loadText(t, productionConfig(t)+"\nunknown_setting: true\n"); err == nil {
		t.Fatal("Load accepted an unknown configuration field")
	}
}

// A scale_up_timeout at or past PyTorch's default 30-minute rendezvous store timeout would let a
// partial gang's landed ranks time out and fail the job for real before job_watcher's own
// scale_up_timeout eviction ever fires — turning a scheduling failure into a charged retry.
func TestLoadRejectsScaleUpTimeoutAtOrAboveRendezvousLimit(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  scale_up_timeout_seconds: 600", "  scale_up_timeout_seconds: 1800", 1)
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted scale_up_timeout_seconds >= 1800")
	}
}

func TestLoadAcceptsScaleUpTimeoutBelowRendezvousLimit(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  scale_up_timeout_seconds: 600", "  scale_up_timeout_seconds: 1799", 1)
	if err := loadText(t, text); err != nil {
		t.Fatalf("Load rejected a valid scale_up_timeout_seconds: %v", err)
	}
}

func TestLoadRejectsZeroDefaultMaxConcurrentAccelerators(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  default_max_concurrent_accelerators: 64", "  default_max_concurrent_accelerators: 0", 1)
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted default_max_concurrent_accelerators: 0")
	}
}
