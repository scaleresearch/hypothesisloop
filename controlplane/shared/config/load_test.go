package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

func TestLoadRequiresStartTimeout(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  start_timeout_seconds: 1800\n", "", 1)
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted a config with no start_timeout_seconds")
	}
}

func TestLoadExpandsEnvReferences(t *testing.T) {
	t.Setenv("TEST_STORE_SECRET", "p@ss: #word\"")
	text := strings.Replace(productionConfig(t), "  secret_access_key: hypothesisloop", "  secret_access_key: ${TEST_STORE_SECRET}", 1)
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataStore.SecretAccessKey != "p@ss: #word\"" {
		t.Fatalf("secret_access_key = %q", cfg.DataStore.SecretAccessKey)
	}
}

func TestLoadRejectsUndefinedEnvReference(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  secret_access_key: hypothesisloop", "  secret_access_key: ${TEST_UNDEFINED_SECRET}", 1)
	err := loadText(t, text)
	if err == nil || !strings.Contains(err.Error(), "TEST_UNDEFINED_SECRET") {
		t.Fatalf("Load error = %v, want one naming TEST_UNDEFINED_SECRET", err)
	}
}

func TestExpandEnvLeavesCommentsAndKeysAlone(t *testing.T) {
	t.Setenv("TEST_HOST", "store.example")
	out, err := ExpandEnv([]byte("# ${TEST_UNDEFINED_SECRET} in a comment\n${TEST_HOST}: 1\nurl: http://${TEST_HOST}:9000/x\n"))
	if err != nil {
		t.Fatalf("ExpandEnv: %v", err)
	}
	var got map[string]string
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "http://store.example:9000/x" || got["${TEST_HOST}"] != "1" {
		t.Fatalf("got %v", got)
	}
}
