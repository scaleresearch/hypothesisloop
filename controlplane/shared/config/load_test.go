package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const signingSecretEnv = "HYPOTHESISLOOP_SIGNING_SECRET"

// productionConfig is the committed settings file, with the deployment's secret in the
// environment as every deployment supplies it.
func productionConfig(t *testing.T) string {
	t.Helper()
	t.Setenv(signingSecretEnv, "a-signing-secret-only-this-test-knows")
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

// The committed file runs nothing by itself: the secret comes from the deployment's environment,
// and a control plane started without one, or with a guessable one, does not start.
func TestTheSigningSecretComesFromTheEnvironmentAndIsRequired(t *testing.T) {
	text := productionConfig(t)
	if !strings.Contains(text, "  signing_secret: ${"+signingSecretEnv+"}\n") {
		t.Fatal("the committed settings hold a signing secret; they must name the environment variable instead")
	}
	if err := loadText(t, text); err != nil {
		t.Fatalf("Load with the secret in the environment: %v", err)
	}
	t.Setenv(signingSecretEnv, "too-short")
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted a signing secret shorter than its MAC")
	}
	os.Unsetenv(signingSecretEnv)
	if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), signingSecretEnv) {
		t.Fatalf("Load without the variable: got = %v, want an error naming it", err)
	}
}

func TestLoadRequiresTheUIOrigin(t *testing.T) {
	text := strings.Replace(productionConfig(t), "  ui_origin: http://localhost:3000", `  ui_origin: ""`, 1)
	if err := loadText(t, text); err == nil {
		t.Fatal("Load accepted an empty ui_origin")
	}
}

func TestLoadRequiresAnIssuerAndAnAudienceOnceJWTKeysAreListed(t *testing.T) {
	keys := "  jwt_keys:\n    - {kid: k1, kty: EC, alg: ES256, crv: P-256, x: AA, y: AA}"
	withKeys := strings.Replace(productionConfig(t), "  jwt_keys: []", keys, 1)
	withIssuer := strings.Replace(withKeys, `  issuer: ""`, "  issuer: https://issuer.example/auth/v1", 1)
	withAudience := strings.Replace(withKeys, `  audience: ""`, "  audience: authenticated", 1)
	for name, text := range map[string]string{"neither": withKeys, "no audience": withIssuer, "no issuer": withAudience} {
		if err := loadText(t, text); err == nil {
			t.Errorf("Load accepted jwt_keys with %s", name)
		}
	}
	both := strings.Replace(withIssuer, `  audience: ""`, "  audience: authenticated", 1)
	if err := loadText(t, both); err != nil {
		t.Fatalf("Load rejected jwt_keys with an issuer and an audience: %v", err)
	}
}

func TestLoadRejectsAnIncompleteAgentsSection(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no launcher":           {"  launcher: main", `  launcher: ""`},
		"launcher not an id":    {"  launcher: main", "  launcher: Main_1"},
		"no image":              {"  image: registry.example.invalid/hypothesisloop-experimentator-base:latest", `  image: ""`},
		"no api url":            {"  api_url: http://REPLACE-WITH-HOST-ADDRESS:8081", `  api_url: ""`},
		"api url with no host":  {"  api_url: http://REPLACE-WITH-HOST-ADDRESS:8081", "  api_url: /api"},
		"api url not http":      {"  api_url: http://REPLACE-WITH-HOST-ADDRESS:8081", "  api_url: ftp://example.invalid"},
		"misspelled agents key": {"  launcher: main", "  launcher: main\n  launchers: [main]"},
	} {
		text := productionConfig(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the production config has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
