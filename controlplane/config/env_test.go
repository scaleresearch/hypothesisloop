package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// madeUp is a value of every key, none of which a test's settings use for anything real.
var madeUp = map[Key]string{
	DatabaseURL: "postgres://test.example.invalid/hypothesisloop", MetricsDBURL: "http://metrics.example.invalid:4000",
	GraphDBURL: "bolt://graph.example.invalid:7687", AuthSigningSecret: "a-signing-secret-only-this-test-knows",
	ModelAnthropicOAuthToken: "a-model-credential", ObjectStoreMinIOSecretAccessKey: "a-store-secret",
	GitHostGitHubAppPrivateKey: "a-pem", GitHostPlainPassword: "a-git-password", AccountKey(SectionSandbox, "fly"): "FlyV1 a-token",
	SandboxE2BAPIKey: "an-e2b-key", AccountKey(SectionCloudCompute, "a"): "a-vast-key",
}

// inEnvironment sets SettingsPath to settings written from text and every key of madeUp.
func inEnvironment(t *testing.T, text string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SettingsPath.Name, path)
	for key, value := range madeUp {
		t.Setenv(key.Name, value)
	}
}

const (
	githubHost   = "\ngit_host:\n  provider: github\n  experiment_template_repository: scaleresearch/hypothesisloop-experiment-template\n  github:\n    app_id: 4242\n    app_slug: hypothesisloop\n"
	plainAccount = "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: https://git.example.invalid\n    username: platform\n"
	rents        = "\nsandbox:\n  provider: fly\n  name: main\n  reconcile_interval_seconds: 5\n  create_pause_milliseconds: 0\n  warm_pool_size: 0\n  fly: {}\n" +
		"  instance:\n    command: [run]\n    cpu: \"1\"\n    memory: 1Gi\n    storage: 1Gi\n" +
		"cloud_compute:\n" + cluster
	cluster = "  - cluster_name: a\n    namespace: main\n    reconcile_interval_seconds: 10\n    status_interval_seconds: 30\n" +
		"    offer_wait_seconds: 60\n    startup_seconds: 60\n    provider: vast\n    vast:\n      account_id: \"1\"\n"
)

func names(keys []Key) []string {
	var out []string
	for _, key := range keys {
		out = append(out, key.Name)
	}
	return out
}

func TestEachProgramNeedsItsOwnKeysAndAProvidersKeyOnlyWithThatProvider(t *testing.T) {
	plain := productionConfig(t)
	for name, c := range map[string]struct {
		text string
		want map[Program][]Key
	}{
		"a plain host that asks for nothing": {plain, map[Program][]Key{
			ProgramControlService: {DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, ModelAnthropicOAuthToken, ObjectStoreMinIOSecretAccessKey},
			ProgramMetricsService: {DatabaseURL, MetricsDBURL, AuthSigningSecret},
			ProgramAdmin:          {DatabaseURL, AuthSigningSecret},
		}},
		"a plain host with an account": {withoutGitHost(t) + plainAccount, map[Program][]Key{
			ProgramControlService: {DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, ModelAnthropicOAuthToken, ObjectStoreMinIOSecretAccessKey, GitHostPlainPassword},
			ProgramMetricsService: {DatabaseURL, MetricsDBURL, AuthSigningSecret},
			ProgramAdmin:          {DatabaseURL, AuthSigningSecret},
		}},
		"the GitHub App and rented machines": {withoutGitHost(t) + githubHost + rents, map[Program][]Key{
			ProgramControlService: {DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, ModelAnthropicOAuthToken, ObjectStoreMinIOSecretAccessKey,
				GitHostGitHubAppPrivateKey, AccountKey(SectionSandbox, "fly"), AccountKey(SectionCloudCompute, "a")},
			ProgramMetricsService: {DatabaseURL, MetricsDBURL, AuthSigningSecret},
			ProgramAdmin:          {DatabaseURL, AuthSigningSecret, GitHostGitHubAppPrivateKey},
		}},
	} {
		inEnvironment(t, c.text)
		s, err := LoadSettings(os.Getenv(SettingsPath.Name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range Programs {
			if got := s.Needs(p); !slices.Equal(got, c.want[p]) {
				t.Errorf("%s: %s needs %v, want %v", name, p, names(got), names(c.want[p]))
			}
		}
	}
}

func TestEachLoaderRefusesEachMissingKeyNamingItAndWhatItIsFor(t *testing.T) {
	text := withoutGitHost(t) + githubHost + rents
	loaders := map[Program]func() error{
		ProgramControlService: func() error { _, err := LoadControlService(); return err },
		ProgramMetricsService: func() error { _, err := LoadMetricsService(); return err },
		ProgramAdmin:          func() error { _, err := LoadAdmin(); return err },
	}
	inEnvironment(t, text)
	s, err := LoadSettings(os.Getenv(SettingsPath.Name))
	if err != nil {
		t.Fatal(err)
	}
	for p, load := range loaders {
		inEnvironment(t, text)
		if err := load(); err != nil {
			t.Fatalf("%s with every key: %v", p, err)
		}
		for _, key := range append(s.Needs(p), SettingsPath) {
			inEnvironment(t, text)
			t.Setenv(key.Name, "")
			want := string(p) + ": " + key.Name + " is required: " + key.For
			if err := load(); err == nil || err.Error() != want {
				t.Errorf("%s without %s: got %v, want %q", p, key.Name, err, want)
			}
		}
	}
	inEnvironment(t, text)
	t.Setenv(AuthSigningSecret.Name, "too-short")
	if _, err := LoadMetricsService(); err == nil || !strings.Contains(err.Error(), AuthSigningSecret.Name) {
		t.Errorf("a short signing secret: got %v, want it refused by name", err)
	}
}

func TestALoaderReadsItsOwnKeysAndIgnoresTheOthers(t *testing.T) {
	inEnvironment(t, withoutGitHost(t)+githubHost+rents)
	c, err := LoadControlService()
	if err != nil {
		t.Fatal(err)
	}
	if c.SigningSecret != madeUp[AuthSigningSecret] || c.GitHubKey != madeUp[GitHostGitHubAppPrivateKey] || c.SandboxSecret != madeUp[AccountKey(SectionSandbox, "fly")] ||
		c.ModelToken != madeUp[ModelAnthropicOAuthToken] || c.ObjectStoreSecret != madeUp[ObjectStoreMinIOSecretAccessKey] || c.GraphDBURL != madeUp[GraphDBURL] ||
		c.CloudComputeSecrets["a"] != madeUp[AccountKey(SectionCloudCompute, "a")] {
		t.Errorf("control-service = %+v, want the environment's keys", c)
	}
	t.Setenv(ModelAnthropicOAuthToken.Name, "")
	t.Setenv(GraphDBURL.Name, "")
	m, err := LoadMetricsService()
	if err != nil || m.DatabaseURL != madeUp[DatabaseURL] || m.MetricsDBURL != madeUp[MetricsDBURL] {
		t.Errorf("metrics-service = %+v, %v, want its keys and no others", m, err)
	}
}

// A provider's secret is <SECTION>_<NAME>_<FIELD>, NAME the section's provider or a cloud_compute
// entry's cluster_name, and IsKey knows every key some program reads by that rule and the
// platform's own few, so a key a test or a chart sets is never mistaken for anything else.
func TestAProvidersKeyIsItsSectionItsNameAndItsField(t *testing.T) {
	for key, want := range map[Key]string{
		AccountKey(SectionSandbox, "fly"):                    "SANDBOX_FLY_API_KEY",
		AccountKey(SectionCloudCompute, "vast"):              "CLOUD_COMPUTE_VAST_API_KEY",
		AccountKey(SectionCloudCompute, "gcp-europe-west4"):  "CLOUD_COMPUTE_GCP_EUROPE_WEST4_API_KEY",
		ProviderKey(SectionGitHost, "plain", "PASSWORD", ""): "GIT_HOST_PLAIN_PASSWORD",
		ObjectStoreMinIOSecretAccessKey:                      "OBJECT_STORE_MINIO_SECRET_ACCESS_KEY",
		GitHostGitHubAppPrivateKey:                           "GIT_HOST_GITHUB_APP_PRIVATE_KEY",
		SandboxE2BAPIKey:                                     "SANDBOX_E2B_API_KEY",
	} {
		if key.Name != want {
			t.Errorf("key = %s, want %s", key.Name, want)
		}
		if !IsKey(key.Name) {
			t.Errorf("IsKey(%s) = false", key.Name)
		}
	}
	for _, name := range []string{SettingsPath.Name, DatabaseURL.Name, MetricsDBURL.Name, GraphDBURL.Name, AuthSigningSecret.Name, ModelAnthropicOAuthToken.Name, "CLOUD_COMPUTE_RUNPOD_API_KEY"} {
		if !IsKey(name) {
			t.Errorf("IsKey(%s) = false, want a key some program reads", name)
		}
	}
	for _, name := range []string{"", "PATH", "HOME", "DATA_STORE_MINIO_SECRET_ACCESS_KEY", "ENVIRONMENTS_GCP_CREDENTIALS", "GITHUB_APP_PRIVATE_KEY", "SANDBOX", "SANDBOX_", "SANDBOX_FLY", "SANDBOX_FLY_", "OBJECT_STORE__SECRET", "sandbox_fly_api_key", "DATABASE_URL_RO", "HL_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if IsKey(name) {
			t.Errorf("IsKey(%q) = true, want no key of the platform", name)
		}
	}
}

// Two cloud_compute entries may rent on one provider, each with its own account key.
func TestEachCloudComputeEntryTakesItsOwnAccountKey(t *testing.T) {
	second := strings.Replace(cluster, "cluster_name: a", "cluster_name: vast-b", 1)
	inEnvironment(t, withoutGitHost(t)+githubHost+rents+second)
	t.Setenv(AccountKey(SectionCloudCompute, "vast-b").Name, "another-vast-key")
	c, err := LoadControlService()
	if err != nil {
		t.Fatal(err)
	}
	if c.CloudComputeSecrets["a"] != madeUp[AccountKey(SectionCloudCompute, "a")] || c.CloudComputeSecrets["vast-b"] != "another-vast-key" {
		t.Errorf("secrets = %v, want each entry's own", c.CloudComputeSecrets)
	}
	t.Setenv("CLOUD_COMPUTE_VAST_B_API_KEY", "")
	if _, err := LoadControlService(); err == nil || !strings.Contains(err.Error(), "CLOUD_COMPUTE_VAST_B_API_KEY is required") {
		t.Errorf("without the second entry's key: %v, want it named", err)
	}
}
