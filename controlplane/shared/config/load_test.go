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
