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
var committedConfig = filepath.Join("..", "..", "settings", "hypothesisloop.yaml")

// productionConfig is the committed settings file as a deployment renders it.
func productionConfig(t *testing.T) string {
	t.Helper()
	t.Setenv(signingSecretEnv, "a-signing-secret-only-this-test-knows")
	b, err := os.ReadFile(committedConfig)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), Placeholder+"HOST-ADDRESS", "host.example.invalid")
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

func TestLoadRefusesTheCommittedFileUntilItsPlaceholdersAreFilledIn(t *testing.T) {
	t.Setenv(signingSecretEnv, "a-signing-secret-only-this-test-knows")
	if _, err := Load(committedConfig); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("Load of the committed settings: got = %v, want it refused for its placeholders", err)
	}
	rendered := productionConfig(t)
	for _, line := range []string{
		"  api_url: http://host.example.invalid:8081\n  default_model",
		"  api_url: http://host.example.invalid:8081\n  metrics_url",
		"  metrics_url: http://host.example.invalid:8084",
		"  endpoint: http://host.example.invalid:9000",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("the settings file has no %q", line)
		}
		unfilled := strings.Replace(rendered, line, strings.Replace(line, "host.example.invalid", Placeholder+"HOST-ADDRESS", 1), 1)
		if err := loadText(t, unfilled); err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("one value left unfilled (%q): Load = %v, want it refused as a placeholder", line, err)
		}
	}
	if err := loadText(t, strings.Replace(rendered, "  endpoint: http://host.example.invalid:9000", "  endpoint: http://0.0.0.0:9000", 1)); err == nil {
		t.Error("a data store at the unspecified address: Load = nil, want it refused")
	}
}

func TestLoadRejectsRuntimeAddressesARuntimeCannotReach(t *testing.T) {
	const api, metrics = "  api_url: http://host.example.invalid:8081\n  metrics_url: http://host.example.invalid:8084", "  metrics_url: http://host.example.invalid:8084"
	for name, edit := range map[string][2]string{
		"no section":            {"runtimes:\n" + api, ""},
		"no api url":            {api, "  metrics_url: http://metrics.example.invalid"},
		"no metrics url":        {metrics, `  metrics_url: ""`},
		"metrics url no host":   {metrics, "  metrics_url: /metrics"},
		"metrics url not http":  {metrics, "  metrics_url: grpc://metrics.example.invalid"},
		"metrics url localhost": {metrics, "  metrics_url: http://localhost:8084"},
		"metrics url loopback":  {metrics, "  metrics_url: http://127.0.0.1:8084"},
		"metrics url any":       {metrics, "  metrics_url: http://0.0.0.0:8084"},
		"metrics url any v6":    {metrics, "  metrics_url: http://[::]:8084"},
		"api url loopback":      {api, "  api_url: http://[::1]:8081\n  metrics_url: http://metrics.example.invalid"},
		"misspelled key":        {metrics, "  metrics_url: http://metrics.example.invalid\n  metric_url: http://metrics.example.invalid"},
	} {
		text := productionConfig(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the production config has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil || !strings.Contains(err.Error(), "runtimes") && !strings.Contains(err.Error(), "metric_url") {
			t.Errorf("%s: Load = %v, want it refused naming the runtimes section", name, err)
		}
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtimes.APIURL != "http://host.example.invalid:8081" || cfg.Runtimes.MetricsURL != "http://host.example.invalid:8084" {
		t.Errorf("runtimes = %+v, want both addresses of the settings file", cfg.Runtimes)
	}
}

func TestLoadRejectsAnIncompleteAgentsSection(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no launcher":           {"  launcher: main", `  launcher: ""`},
		"launcher not an id":    {"  launcher: main", "  launcher: Main_1"},
		"no image":              {"  image: registry.example.invalid/hypothesisloop-experimentator-base:latest", `  image: ""`},
		"no api url":            {"  api_url: http://host.example.invalid:8081", `  api_url: ""`},
		"api url with no host":  {"  api_url: http://host.example.invalid:8081", "  api_url: /api"},
		"api url not http":      {"  api_url: http://host.example.invalid:8081", "  api_url: ftp://example.invalid"},
		"misspelled agents key": {"  launcher: main", "  launcher: main\n  launchers: [main]"},
		"no default model":      {"  default_model: claude-sonnet-5\n", ""},
		"no boot limit":         {"  boot_limit_seconds: 300\n", ""},
		"negative boot limit":   {"  boot_limit_seconds: 300", "  boot_limit_seconds: -1"},
		"no answer size":        {"  max_new_instances_per_answer: 10\n", ""},
		"no hosted bound":       {"  max_hosted_per_platform_experiment: 25\n", ""},
		"zero hosted bound":     {"  max_hosted_per_platform_experiment: 25", "  max_hosted_per_platform_experiment: 0"},
		"launcher with a dash":  {"  launcher: main", "  launcher: main-2"},
		"default model unsafe":  {"  default_model: claude-sonnet-5", "  default_model: $(id)"},
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

func TestLoadRejectsAnIncompleteAccountsSection(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no section":        {"accounts:\n  max_orgs_per_user: 3\n  max_projects_per_org: 10\n", ""},
		"no org bound":      {"  max_orgs_per_user: 3\n", ""},
		"zero org bound":    {"  max_orgs_per_user: 3", "  max_orgs_per_user: 0"},
		"no project bound":  {"  max_projects_per_org: 10\n", ""},
		"negative projects": {"  max_projects_per_org: 10", "  max_projects_per_org: -1"},
		"misspelled bound":  {"  max_orgs_per_user: 3", "  max_orgs_per_person: 3"},
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

const githubSection = `
github:
  app_id: 4242
  app_slug: hypothesisloop
  client_id: Iv23test
  client_secret: ${HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET}
  private_key: ${HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY}
`

func TestLoadTakesTheGitHubAppSecretsFromTheEnvironment(t *testing.T) {
	text := productionConfig(t) + githubSection
	if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), "HYPOTHESISLOOP_GITHUB_APP_") {
		t.Fatalf("Load with the App's secrets unset: got = %v, want it to name the missing variable", err)
	}
	t.Setenv("HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nabc: def # not a comment\n-----END RSA PRIVATE KEY-----\n")
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.GitHub.Configured() || cfg.GitHub.AppID != 4242 || cfg.GitHub.ClientSecret != "client-secret" ||
		cfg.GitHub.PrivateKey != os.Getenv("HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY") {
		t.Fatalf("github = %+v, want the section with both secrets as the environment holds them", cfg.GitHub.AppID)
	}
}

func TestTheCommittedConfigHasNoGitHubApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHub.Configured() {
		t.Fatal("the committed settings file configures a GitHub App, which no repo file may hold the secrets of")
	}
}

func TestLoadRejectsAHalfConfiguredGitHubApp(t *testing.T) {
	t.Setenv("HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY", "pem")
	for _, line := range []string{"  app_id: 4242\n", "  app_slug: hypothesisloop\n", "  client_id: Iv23test\n",
		"  client_secret: ${HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET}\n", "  private_key: ${HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY}\n"} {
		section := strings.Replace(githubSection, line, "", 1)
		if section == githubSection {
			t.Fatalf("the test section has no line %q", line)
		}
		if err := loadText(t, productionConfig(t)+section); err == nil || !strings.Contains(err.Error(), "github") {
			t.Errorf("Load without %q: got = %v, want a refusal naming github", strings.TrimSpace(line), err)
		}
	}
}
