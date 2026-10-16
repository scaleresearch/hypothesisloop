package config

import (
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const signingSecretEnv = "HYPOTHESISLOOP_SIGNING_SECRET"

// productionConfig is the committed settings file, with the deployment's secret in the
// environment as every deployment supplies it.
var committedConfig = filepath.Join("..", "..", "settings", "hypothesisloop.yaml")

// productionConfig is the committed settings file as a deployment renders it: with its git host.
func productionConfig(t *testing.T) string {
	t.Helper()
	return withoutGitHost(t) + "\ngit_host:\n  provider: plain\n  plain:\n    base_url: https://git.example.invalid\n"
}

// withoutGitHost is the rendered settings file before the deployment's git host is added.
func withoutGitHost(t *testing.T) string {
	t.Helper()
	t.Setenv(signingSecretEnv, "a-signing-secret-only-this-test-knows")
	b, err := os.ReadFile(committedConfig)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.ReplaceAll(string(b), Placeholder+"HOST-ADDRESS", "host.example.invalid")
	rendered = strings.ReplaceAll(rendered, Placeholder+"UI-ORIGIN", "http://localhost:3000")
	return strings.ReplaceAll(rendered, Placeholder+"IMAGE-TAG", "abc1234")
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

func TestLoadRequiresEveryIdentityProviderToBeCompleteAndListedOnce(t *testing.T) {
	const key = "{kid: k1, kty: EC, alg: ES256, crv: P-256, x: AA, y: AA}"
	with := func(providers string) string {
		return strings.Replace(productionConfig(t), "  identity_providers: []", "  identity_providers: ["+providers+"]", 1)
	}
	supabase := "{issuer: https://issuer.example/auth/v1, audience: authenticated, jwt_keys: [" + key + "]}"
	local := "{issuer: http://localhost/dev, audience: dev, jwt_keys: [" + key + "]}"
	for name, providers := range map[string]string{
		"no issuer":         "{audience: authenticated, jwt_keys: [" + key + "]}",
		"no audience":       "{issuer: https://issuer.example/auth/v1, jwt_keys: [" + key + "]}",
		"no keys":           "{issuer: https://issuer.example/auth/v1, audience: authenticated}",
		"one issuer twice":  supabase + ", " + supabase,
		"a field it has no": "{issuer: https://issuer.example/auth/v1, audience: authenticated, jwt_keys: [" + key + "], jwks_url: https://issuer.example}",
	} {
		if err := loadText(t, with(providers)); err == nil {
			t.Errorf("Load accepted an identity provider with %s", name)
		}
	}
	for name, providers := range map[string]string{"one provider": supabase, "two providers": supabase + ", " + local} {
		if err := loadText(t, with(providers)); err != nil {
			t.Errorf("Load rejected %s: %v", name, err)
		}
	}
	// The keys the file had before identity providers were a list are refused by name.
	if err := loadText(t, strings.Replace(productionConfig(t), "  identity_providers: []", "  issuer: https://issuer.example/auth/v1", 1)); err == nil {
		t.Error("Load accepted auth.issuer outside an identity provider")
	}
}

func TestLoadRefusesTheCommittedFileUntilItsPlaceholdersAreFilledIn(t *testing.T) {
	t.Setenv(signingSecretEnv, "a-signing-secret-only-this-test-knows")
	if _, err := Load(committedConfig); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("Load of the committed settings: got = %v, want it refused for its placeholders", err)
	}
	rendered := productionConfig(t)
	for _, line := range []string{
		"  public_api_url: http://host.example.invalid:8081",
		"  metrics_url: http://host.example.invalid:8084",
		"  endpoint: http://host.example.invalid:9000",
		"  registry: host.example.invalid:5000",
		"  image_tag: abc1234",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("the settings file has no %q", line)
		}
		unfilled := strings.Replace(rendered, line, strings.NewReplacer("host.example.invalid", Placeholder+"HOST-ADDRESS", "abc1234", Placeholder+"IMAGE-TAG").Replace(line), 1)
		if err := loadText(t, unfilled); err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("one value left unfilled (%q): Load = %v, want it refused as a placeholder", line, err)
		}
	}
	if err := loadText(t, strings.Replace(rendered, "  endpoint: http://host.example.invalid:9000", "  endpoint: http://0.0.0.0:9000", 1)); err == nil {
		t.Error("a data store at the unspecified address: Load = nil, want it refused")
	}
}

func TestLoadRequiresTheControlPlanesOwnAddressOfTheDataStore(t *testing.T) {
	const own = "  control_plane_endpoint: http://minio:9000"
	rendered := productionConfig(t)
	if !strings.Contains(rendered, own) {
		t.Fatalf("the settings file has no %q", own)
	}
	if err := loadText(t, strings.Replace(rendered, own, "  control_plane_endpoint: http://localhost:9000", 1)); err != nil {
		t.Errorf("a store on the control plane's own machine: Load = %v, want it accepted", err)
	}
	for name, line := range map[string]string{"no key": "", "empty": `  control_plane_endpoint: ""`, "no host": "  control_plane_endpoint: /store", "not http": "  control_plane_endpoint: s3://bucket"} {
		if err := loadText(t, strings.Replace(rendered, own, line, 1)); err == nil || !strings.Contains(err.Error(), "control_plane_endpoint") {
			t.Errorf("%s: Load = %v, want it refused by the key's name with no value taken from endpoint", name, err)
		}
	}
}

func TestLoadRejectsRuntimeAddressesARuntimeCannotReach(t *testing.T) {
	const api, metrics = "  public_api_url: http://host.example.invalid:8081", "  metrics_url: http://host.example.invalid:8084"
	for name, edit := range map[string][2]string{
		"no section":                            {"runtimes:\n" + metrics + "\n  registry: host.example.invalid:5000\n  image_tag: abc1234", ""},
		"no api url":                            {api + "\n", ""},
		"api url with no host":                  {api, "  public_api_url: /api"},
		"api url not http":                      {api, "  public_api_url: ftp://example.invalid"},
		"api url under agents":                  {"  launcher: main", "  launcher: main\n  api_url: http://host.example.invalid:8081"},
		"api url under runtimes":                {metrics, metrics + "\n  api_url: http://host.example.invalid:8081"},
		"no metrics url":                        {metrics, `  metrics_url: ""`},
		"metrics url no host":                   {metrics, "  metrics_url: /metrics"},
		"metrics url not http":                  {metrics, "  metrics_url: grpc://metrics.example.invalid"},
		"metrics url localhost":                 {metrics, "  metrics_url: http://localhost:8084"},
		"metrics url loopback":                  {metrics, "  metrics_url: http://127.0.0.1:8084"},
		"metrics url any":                       {metrics, "  metrics_url: http://0.0.0.0:8084"},
		"metrics url any v6":                    {metrics, "  metrics_url: http://[::]:8084"},
		"api url loopback":                      {api, "  public_api_url: http://[::1]:8081"},
		"misspelled key":                        {metrics, "  metrics_url: http://metrics.example.invalid\n  metric_url: http://metrics.example.invalid"},
		"job image named twice":                 {"  - name: tenstorrent", "  - name: python"},
		"job image with no ref":                 {"    repository: hypothesisloop-job-tenstorrent\n", ""},
		"environment base of a whole reference": {"    repository: hypothesisloop-job-tenstorrent\n", "    image: registry.example.invalid/tt:1\n"},
		"environment base with a tag":           {"    environment_base: hypothesisloop-job-base\n", "    environment_base: hypothesisloop-job-base:1\n"},
	} {
		text := productionConfig(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the production config has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil || !strings.Contains(err.Error(), "runtimes") && !strings.Contains(err.Error(), "job_images") && !strings.Contains(err.Error(), "metric_url") && !strings.Contains(err.Error(), "api_url") {
			t.Errorf("%s: Load = %v, want it refused naming the address", name, err)
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
	if want := (RuntimesConfig{MetricsURL: "http://host.example.invalid:8084", Registry: "host.example.invalid:5000", ImageTag: "abc1234"}); cfg.Runtimes != want || cfg.Services.PublicAPIURL != "http://host.example.invalid:8081" {
		t.Errorf("runtimes = %+v at %q, want %+v", cfg.Runtimes, cfg.Services.PublicAPIURL, want)
	}
}

// The install command a registered cluster is given names these two, so a value no node can
// pull with is refused where the deployment is configured.
func TestLoadRejectsRuntimeImagesANodeCannotPull(t *testing.T) {
	const registry, tag = "  registry: host.example.invalid:5000", "  image_tag: abc1234"
	for name, edit := range map[string][2]string{
		"no registry":          {registry + "\n", ""},
		"empty registry":       {registry, `  registry: ""`},
		"registry with scheme": {registry, "  registry: https://registry.example.invalid"},
		"registry localhost":   {registry, "  registry: localhost:5000"},
		"registry loopback":    {registry, "  registry: 127.0.0.1:5000/images"},
		"no tag":               {tag, ""},
		"tag with a name":      {tag, "  image_tag: hypothesisloop-cluster-agent:abc1234"},
	} {
		text := productionConfig(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the production config has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil || !strings.Contains(err.Error(), "runtimes") {
			t.Errorf("%s: Load = %v, want it refused naming the runtimes section", name, err)
		}
	}
	if err := loadText(t, strings.Replace(productionConfig(t), registry, "  registry: europe-west1-docker.pkg.dev/project/images", 1)); err != nil {
		t.Errorf("a registry with a path: Load = %v, want it accepted", err)
	}
}

func TestLoadRejectsAnIncompleteAgentsSection(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no launcher":              {"  launcher: main", `  launcher: ""`},
		"launcher not an id":       {"  launcher: main", "  launcher: Main_1"},
		"no image":                 {"  image: registry.example.invalid/hypothesisloop-experimentator-base:latest", `  image: ""`},
		"misspelled agents key":    {"  launcher: main", "  launcher: main\n  launchers: [main]"},
		"no default model":         {"  default_model: claude-sonnet-5\n", ""},
		"no coordinator model":     {"  coordinator_model: claude-opus-5-5\n", ""},
		"coordinator model unsafe": {"  coordinator_model: claude-opus-5-5", "  coordinator_model: $(id)"},
		"no boot limit":            {"  boot_limit_seconds: 300\n", ""},
		"no progress limit":        {"  progress_limit_seconds: 300\n", ""},
		"progress limit too short": {"  progress_limit_seconds: 300", "  progress_limit_seconds: 60"},
		"negative boot limit":      {"  boot_limit_seconds: 300", "  boot_limit_seconds: -1"},
		"no answer size":           {"  max_new_instances_per_answer: 10\n", ""},
		"no hosted bound":          {"  max_hosted_per_platform_experiment: 25\n", ""},
		"zero hosted bound":        {"  max_hosted_per_platform_experiment: 25", "  max_hosted_per_platform_experiment: 0"},
		"no idle notice":           {"  idle_notice_minutes: 10\n", ""},
		"zero idle notice":         {"  idle_notice_minutes: 10", "  idle_notice_minutes: 0"},
		"launcher with a dash":     {"  launcher: main", "  launcher: main-2"},
		"default model unsafe":     {"  default_model: claude-sonnet-5", "  default_model: $(id)"},
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

func TestLoadRejectsAnIncompletePlanningSection(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no limit":             {"  limit_minutes: 30\n", ""},
		"no baseline limit":    {"  baseline_limit_minutes: 90\n", ""},
		"no baseline floor":    {"  min_baseline_minutes: 45\n", ""},
		"floor over limit":     {"  min_baseline_minutes: 45", "  min_baseline_minutes: 91"},
		"no wait":              {"  proposed_for_days: 7\n", ""},
		"misspelled wait":      {"  proposed_for_days: 7", "  proposed_for_day: 7"},
		"no planner kept":      {"  planner_kept_minutes: 10\n", ""},
		"zero planner kept":    {"  planner_kept_minutes: 10", "  planner_kept_minutes: 0"},
		"negative auto accept": {"  auto_accept_seconds: 30", "  auto_accept_seconds: -1"},
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
git_host:
  provider: github
  github:
    app_id: 4242
    app_slug: hypothesisloop
    client_id: Iv23test
    client_secret: ${HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_CLIENT_SECRET}
    private_key: ${HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY}
`

func TestLoadTakesTheGitHubAppSecretsFromTheEnvironment(t *testing.T) {
	text := withoutGitHost(t) + githubSection
	if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), "HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_") {
		t.Fatalf("Load with the App's secrets unset: got = %v, want it to name the missing variable", err)
	}
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nabc: def # not a comment\n-----END RSA PRIVATE KEY-----\n")
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	app := cfg.GitHost.GitHub
	if cfg.GitHost.Provider != GitHostProviderGitHub || app == nil || app.AppID != 4242 || app.ClientSecret != "client-secret" ||
		app.PrivateKey != os.Getenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY") {
		t.Fatalf("git_host = %+v, want the github section with both secrets as the environment holds them", cfg.GitHost.Provider)
	}
}

func TestLoadTakesOneGitHostAndRefusesAHalfStatedOne(t *testing.T) {
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY", "pem")
	t.Setenv("HL_TEST_GIT_USERNAME", "platform")
	t.Setenv("HL_TEST_GIT_PASSWORD", "secret")
	const plain = "\ngit_host:\n  provider: plain\n  plain:\n    base_url: https://git.example/experiments\n"
	const credential = "    username: ${HL_TEST_GIT_USERNAME}\n    password: ${HL_TEST_GIT_PASSWORD}\n"
	const app = "  github:\n    app_id: 4242\n    app_slug: hypothesisloop\n    client_id: Iv23test\n    client_secret: x\n    private_key: y\n"
	for name, section := range map[string]string{
		"a plain host that asks for no credential": plain,
		"a plain host with the credential":         plain + credential,
		"a plain host over http that says so":      "\ngit_host:\n  provider: plain\n  plain:\n    base_url: http://git.example\n    allow_http: true\n",
		"github with its App":                      githubSection,
	} {
		if err := loadText(t, withoutGitHost(t)+section); err != nil {
			t.Errorf("%s: got = %v, want it loaded", name, err)
		}
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(withoutGitHost(t)+plain+credential), 0o600); err != nil {
		t.Fatal(err)
	}
	want := PlainGitConfig{BaseURL: "https://git.example/experiments", Username: "platform", Password: "secret"}
	if cfg, err := Load(path); err != nil || cfg.GitHost.Provider != GitHostProviderPlain || cfg.GitHost.Plain == nil || *cfg.GitHost.Plain != want || cfg.GitHost.GitHub != nil {
		t.Fatalf("Load: %v, want the plain host with the credential as the environment holds it", err)
	}
	for name, section := range map[string]string{
		"no git_host section":              "",
		"no provider":                      "\ngit_host:\n  plain:\n    base_url: https://git.example\n",
		"a provider nobody wrote":          "\ngit_host:\n  provider: gitlab\n",
		"github with no App":               "\ngit_host:\n  provider: github\n",
		"github with a plain section":      githubSection + "  plain:\n    base_url: https://git.example\n",
		"plain beside a GitHub App":        plain + app,
		"plain with no section":            "\ngit_host:\n  provider: plain\n",
		"plain with no base_url":           "\ngit_host:\n  provider: plain\n  plain:\n    allow_http: false\n",
		"plain over http":                  "\ngit_host:\n  provider: plain\n  plain:\n    base_url: http://git.example\n",
		"plain over another scheme":        "\ngit_host:\n  provider: plain\n  plain:\n    base_url: git://git.example\n    allow_http: true\n",
		"plain with a username alone":      plain + "    username: ${HL_TEST_GIT_USERNAME}\n",
		"plain with a variable nobody set": plain + "    username: platform\n    password: ${HL_TEST_GIT_UNSET}\n",
		"the old github section":           "\ngithub:\n  app_id: 4242\ngit:\n  provider: github\n",
	} {
		if err := loadText(t, withoutGitHost(t)+section); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRejectsAHalfConfiguredGitHubApp(t *testing.T) {
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_CLIENT_SECRET", "client-secret")
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY", "pem")
	for _, line := range []string{"    app_id: 4242\n", "    app_slug: hypothesisloop\n", "    client_id: Iv23test\n",
		"    client_secret: ${HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_CLIENT_SECRET}\n", "    private_key: ${HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY}\n"} {
		section := strings.Replace(githubSection, line, "", 1)
		if section == githubSection {
			t.Fatalf("the test section has no line %q", line)
		}
		if err := loadText(t, withoutGitHost(t)+section); err == nil || !strings.Contains(err.Error(), "github") {
			t.Errorf("Load without %q: got = %v, want a refusal naming github", strings.TrimSpace(line), err)
		}
	}
}

// A CPU experiment's budget reads as core-hours, which holds only while a core-hour costs 1: the
// type is built in at that rate, and a file that prices it is refused.
func TestACoreHourIsBuiltInAtOneAndPricedNowhereElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if rate := cfg.RateByName[string(domain.CPUType)]; rate != 1.0 || !slices.Contains(cfg.AcceleratorTypeNames(), string(domain.CPUType)) {
		t.Errorf("rate of %s = %v in %v, want 1 and the type in the catalog", domain.CPUType, rate, cfg.AcceleratorTypeNames())
	}
	listed := strings.Replace(productionConfig(t), "accelerator_types:\n", "accelerator_types:\n  - name: "+string(domain.CPUType)+"\n    acch_rate: 0.5\n", 1)
	if err := loadText(t, listed); err == nil || !strings.Contains(err.Error(), "built in") {
		t.Errorf("a file that lists %s: Load = %v, want it refused as built in", domain.CPUType, err)
	}
}

func TestLoadRejectsMissingPlatformExperimentSettings(t *testing.T) {
	for _, line := range []string{"  default_quota_tier: guaranteed\n", "  max_platform_experiment_days: 30\n"} {
		if err := loadText(t, strings.Replace(productionConfig(t), line, "", 1)); err == nil {
			t.Errorf("Load accepted settings without %q", strings.TrimSpace(line))
		}
	}
}

// A limit left out read as zero, and zero as no limit at all.
func TestLoadRequiresEveryLimitToBeStated(t *testing.T) {
	for _, line := range []string{
		"  max_submissions_per_hour: 100\n", "  max_accelerator_count_per_job: 16\n", "  max_cpu_cores_per_job: 64\n",
		"  max_ram_gb_per_job: 512\n", "  max_storage_gb_per_job: 1000\n", "  max_bytes_per_agent: 214748364800\n",
	} {
		key := strings.SplitN(strings.TrimSpace(line), ":", 2)[0]
		if !strings.Contains(productionConfig(t), line) {
			t.Fatalf("the committed settings do not hold %q", strings.TrimSpace(line))
		}
		for _, text := range []string{
			strings.Replace(productionConfig(t), line, "", 1),
			strings.Replace(productionConfig(t), line, "  "+key+": 0\n", 1),
		} {
			if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), key+" is required") {
				t.Errorf("%s left out or 0: Load = %v, want it named as required", key, err)
			}
		}
	}
}

// What a type always needs beside the device is the catalog's to say, so no job spec has to: the
// committed settings give every Blackhole job its 1 GiB hugepages.
func TestAnAcceleratorTypeDeclaresWhatEveryJobOnItNeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PodResourcesByName["tenstorrent.com/chipArch=blackhole"]["hugepages-1Gi"]; got != "4Gi" {
		t.Errorf("pod_resources of a Blackhole chip: hugepages-1Gi = %q, want 4Gi", got)
	}
	if _, declared := cfg.PodResourcesByName["nvidia.com/gpu.product=NVIDIA-L40"]; declared {
		t.Error("a type that declares none is listed with pod resources")
	}
	for _, quantity := range []string{"lots", "0", "-4Gi"} {
		text := strings.Replace(productionConfig(t), "hugepages-1Gi: 4Gi", "hugepages-1Gi: "+quantity, 1)
		if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), "pod_resources") {
			t.Errorf("pod_resources of %q: err = %v, want it refused", quantity, err)
		}
	}
}

const environmentsSection = `
environments:
  repository: europe-west1-docker.pkg.dev/project/hypothesisloop-environments/environment
  provider: gcp
  gcp:
    pusher: env-pusher@project.iam.gserviceaccount.com
    puller: env-puller@project.iam.gserviceaccount.com
`

func TestEnvironmentsAreLeftOutOrStatedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || cfg.Environments != nil {
		t.Fatalf("Load without the section: %v, environments %+v, want it loaded with none", err, cfg.Environments)
	}
	if err := os.WriteFile(path, []byte(productionConfig(t)+environmentsSection), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with the section: %v", err)
	}
	if want := (EnvironmentsConfig{Repository: "europe-west1-docker.pkg.dev/project/hypothesisloop-environments/environment", Provider: RegistryProviderGCP,
		GCP: GCPRegistryConfig{Pusher: "env-pusher@project.iam.gserviceaccount.com", Puller: "env-puller@project.iam.gserviceaccount.com"}}); *cfg.Environments != want {
		t.Fatalf("environments = %+v, want %+v", *cfg.Environments, want)
	}
	for name, replace := range map[string][2]string{
		"no pusher":                   {"    pusher: env-pusher@project.iam.gserviceaccount.com\n", ""},
		"no provider":                 {"  provider: gcp\n", ""},
		"an unknown provider":         {"provider: gcp", "provider: elsewhere"},
		"a puller that is no account": {"env-puller@project.iam.gserviceaccount.com", "someone@example.com"},
		"a repository with no path":   {"europe-west1-docker.pkg.dev/project/hypothesisloop-environments/environment", "europe-west1-docker.pkg.dev"},
		"a repository with a scheme":  {"europe-west1-docker.pkg.dev/project", "https://europe-west1-docker.pkg.dev/project"},
		"a repository with a digest":  {"/environment\n", "/environment@sha256:abc\n"},
	} {
		if err := loadText(t, productionConfig(t)+strings.Replace(environmentsSection, replace[0], replace[1], 1)); err == nil {
			t.Errorf("%s: loaded, want it refused", name)
		}
	}
}

// An offered image this deployment builds names its environment base under the same registry and
// tag, and one built elsewhere has none.
func TestAnOfferedJobImageNamesItsEnvironmentBaseAtTheRuntimesTag(t *testing.T) {
	c := &Config{Runtimes: RuntimesConfig{Registry: "registry.example.invalid/hl", ImageTag: "abc1234"}, JobImages: []JobImageConfig{
		{Name: "python", Repository: "hypothesisloop-job", EnvironmentBase: "hypothesisloop-job-base"},
		{Name: "theirs", Image: "registry.example.invalid/team/theirs:1"},
	}}
	offered := c.OfferedJobImages()
	if offered[0].Image != "registry.example.invalid/hl/hypothesisloop-job:abc1234" || offered[0].EnvironmentBase != "registry.example.invalid/hl/hypothesisloop-job-base:abc1234" || offered[1].EnvironmentBase != "" {
		t.Errorf("offered %+v", offered)
	}
}
