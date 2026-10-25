package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// committedConfig is the committed settings file.
var committedConfig = filepath.Join("..", "settings", "hypothesisloop.yaml")

// productionConfig is the committed settings file as a deployment renders it: with its git host.
func productionConfig(t *testing.T) string {
	t.Helper()
	return withoutGitHost(t) + "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: https://git.example.invalid\n"
}

// withoutGitHost is the rendered settings file before the deployment's git host is added.
func withoutGitHost(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(committedConfig)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.ReplaceAll(string(b), Placeholder+"HOST-ADDRESS", "host.example.invalid")
	rendered = strings.ReplaceAll(rendered, Placeholder+"UI-ORIGIN", "http://localhost:3000")
	rendered = strings.ReplaceAll(rendered, Placeholder+"AGENT-IMAGE", "registry.example.invalid/hypothesisloop-harness-base:abc1234")
	return strings.ReplaceAll(rendered, Placeholder+"IMAGE-TAG", "abc1234")
}

func loadText(t *testing.T, text string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSettings(path)
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

func loadConfigText(t *testing.T, text string) (*Settings, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadSettings(path)
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
	if _, err := LoadSettings(committedConfig); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("Load of the committed settings: got = %v, want it refused for its placeholders", err)
	}
	rendered := productionConfig(t)
	for _, line := range []string{
		"  public_api_url: http://host.example.invalid:8081",
		"  metrics_url: http://host.example.invalid:8084",
		"  endpoint: http://host.example.invalid:9000",
		"  registry: host.example.invalid:5000",
		"  image_tag: abc1234",
		"  image: registry.example.invalid/hypothesisloop-harness-base:abc1234",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("the settings file has no %q", line)
		}
		unfilled := strings.Replace(rendered, line, strings.NewReplacer("host.example.invalid", Placeholder+"HOST-ADDRESS", "abc1234", Placeholder+"IMAGE-TAG",
			"registry.example.invalid/hypothesisloop-harness-base:abc1234", Placeholder+"AGENT-IMAGE").Replace(line), 1)
		if err := loadText(t, unfilled); err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("one value left unfilled (%q): Load = %v, want it refused as a placeholder", line, err)
		}
	}
	if err := loadText(t, strings.Replace(rendered, "  endpoint: http://host.example.invalid:9000", "  endpoint: http://0.0.0.0:9000", 1)); err == nil {
		t.Error("an object store at the unspecified address: Load = nil, want it refused")
	}
}

func TestLoadRequiresTheControlPlanesOwnAddressOfTheObjectStore(t *testing.T) {
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
		"no section":             {"runtimes:\n" + metrics + "\n  registry: host.example.invalid:5000\n  image_tag: abc1234\n  registry_plain_http: false", ""},
		"no api url":             {api + "\n", ""},
		"api url with no host":   {api, "  public_api_url: /api"},
		"api url not http":       {api, "  public_api_url: ftp://example.invalid"},
		"api url under agents":   {"  default_model: anthropic/claude-sonnet-5", "  default_model: anthropic/claude-sonnet-5\n  api_url: http://host.example.invalid:8081"},
		"api url under runtimes": {metrics, metrics + "\n  api_url: http://host.example.invalid:8081"},
		"no metrics url":         {metrics, `  metrics_url: ""`},
		"metrics url no host":    {metrics, "  metrics_url: /metrics"},
		"metrics url not http":   {metrics, "  metrics_url: grpc://metrics.example.invalid"},
		"metrics url localhost":  {metrics, "  metrics_url: http://localhost:8084"},
		"metrics url loopback":   {metrics, "  metrics_url: http://127.0.0.1:8084"},
		"metrics url any":        {metrics, "  metrics_url: http://0.0.0.0:8084"},
		"metrics url any v6":     {metrics, "  metrics_url: http://[::]:8084"},
		"api url loopback":       {api, "  public_api_url: http://[::1]:8081"},
		"misspelled key":         {metrics, "  metrics_url: http://metrics.example.invalid\n  metric_url: http://metrics.example.invalid"},
	} {
		text := productionConfig(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the production config has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil || !strings.Contains(err.Error(), "runtimes") && !strings.Contains(err.Error(), "metric_url") && !strings.Contains(err.Error(), "api_url") {
			t.Errorf("%s: Load = %v, want it refused naming the address", name, err)
		}
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSettings(path)
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
		"no image":                 {"  image: registry.example.invalid/hypothesisloop-harness-base:abc1234", `  image: ""`},
		"misspelled agents key":    {"  default_model: anthropic/claude-sonnet-5", "  default_model: anthropic/claude-sonnet-5\n  launchers: [main]"},
		"no default model":         {"  default_model: anthropic/claude-sonnet-5\n", ""},
		"no coordinator model":     {"  coordinator_model: anthropic/claude-opus-5-5\n", ""},
		"coordinator model unsafe": {"  coordinator_model: anthropic/claude-opus-5-5", "  coordinator_model: $(id)"},
		"no boot limit":            {"  boot_limit_seconds: 300\n", ""},
		"no progress limit":        {"  progress_limit_seconds: 300\n", ""},
		"progress limit too short": {"  progress_limit_seconds: 300", "  progress_limit_seconds: 60"},
		"negative boot limit":      {"  boot_limit_seconds: 300", "  boot_limit_seconds: -1"},
		"no answer size":           {"  max_new_instances_per_answer: 10\n", ""},
		"no sandboxed bound":       {"  max_agentsandboxes_per_platform_experiment: 25\n", ""},
		"zero sandboxed bound":     {"  max_agentsandboxes_per_platform_experiment: 25", "  max_agentsandboxes_per_platform_experiment: 0"},
		"no idle notice":           {"  idle_notice_minutes: 10\n", ""},
		"zero idle notice":         {"  idle_notice_minutes: 10", "  idle_notice_minutes: 0"},
		"default model unsafe":     {"  default_model: anthropic/claude-sonnet-5", "  default_model: $(id)"},
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
		"no limit":          {"  limit_minutes: 30\n", ""},
		"no baseline limit": {"  baseline_limit_minutes: 60\n", ""},
		"no baseline floor": {"  min_baseline_minutes: 15\n", ""},
		"floor over limit":  {"  min_baseline_minutes: 15", "  min_baseline_minutes: 61"},
		"no wait":           {"  proposed_for_days: 7\n", ""},
		"misspelled wait":   {"  proposed_for_days: 7", "  proposed_for_day: 7"},
		"no planner kept":   {"  planner_kept_minutes: 10\n", ""},
		"zero planner kept": {"  planner_kept_minutes: 10", "  planner_kept_minutes: 0"},
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
  experiment_template_repository: scaleresearch/hypothesisloop-experiment-template
  github:
    app_id: 4242
    app_slug: hypothesisloop
    client_id: Iv23made-up
`

func TestLoadTakesOneGitHostAndRefusesAHalfStatedOne(t *testing.T) {
	const plain = "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: https://git.example/experiments\n"
	const account = "    username: platform\n"
	const app = "  github:\n    app_id: 4242\n    app_slug: hypothesisloop\n"
	for name, section := range map[string]string{
		"a plain host that asks for no credential": plain,
		"a plain host with an account":             plain + account,
		"a plain host over http that says so":      "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: http://git.example\n    allow_http: true\n",
		"github with its App":                      githubSection,
	} {
		if err := loadText(t, withoutGitHost(t)+section); err != nil {
			t.Errorf("%s: got = %v, want it loaded", name, err)
		}
	}
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(withoutGitHost(t)+plain+account), 0o600); err != nil {
		t.Fatal(err)
	}
	want := PlainGitConfig{BaseURL: "https://git.example/experiments", Username: "platform"}
	if cfg, err := LoadSettings(path); err != nil || cfg.GitHost.Provider != GitHostProviderPlain || cfg.GitHost.Plain == nil || *cfg.GitHost.Plain != want || cfg.GitHost.GitHub != nil {
		t.Fatalf("Load: %v, want the plain host and its account", err)
	}
	for name, section := range map[string]string{
		"no git_host section":         "",
		"no provider":                 "\ngit_host:\n  plain:\n    base_url: https://git.example\n",
		"a provider nobody wrote":     "\ngit_host:\n  provider: gitlab\n",
		"github with no App":          "\ngit_host:\n  provider: github\n",
		"github with a plain section": githubSection + "  plain:\n    base_url: https://git.example\n",
		"plain beside a GitHub App":   plain + app,
		"plain with no section":       "\ngit_host:\n  provider: plain\n",
		"plain with no base_url":      "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    allow_http: false\n",
		"plain over http":             "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: http://git.example\n",
		"plain over another scheme":   "\ngit_host:\n  provider: plain\n  experiment_template_repository: experiment-template.git\n  plain:\n    base_url: git://git.example\n    allow_http: true\n",
		"plain with a password":       plain + account + "    password: secret\n",
		"the old github section":      "\ngithub:\n  app_id: 4242\ngit:\n  provider: github\n",
		"a proxy_url":                 strings.Replace(plain, "git_host:\n", "git_host:\n  proxy_url: https://api.example.invalid/git\n", 1),
		"a GitHub App's private key":  githubSection + "    private_key: pem\n",
		"the App's client secret":     githubSection + "    client_secret: secret\n",
	} {
		if err := loadText(t, withoutGitHost(t)+section); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRejectsAHalfConfiguredGitHubApp(t *testing.T) {
	for _, line := range []string{"    app_id: 4242\n", "    app_slug: hypothesisloop\n", "    client_id: Iv23made-up\n"} {
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
	cfg, err := LoadSettings(path)
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
	cfg, err := LoadSettings(path)
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

const containerRegistrySection = `
container_registry:
  provider: gcp
  gcp:
    project: my-project
    location: europe-west1
`

func TestTheContainerRegistryIsLeftOutOrStatedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadSettings(path); err != nil || cfg.ContainerRegistry != nil {
		t.Fatalf("Load without the section: %v, container_registry %+v, want it loaded with none", err, cfg.ContainerRegistry)
	}
	if err := os.WriteFile(path, []byte(productionConfig(t)+containerRegistrySection), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("Load with the section: %v", err)
	}
	if got, want := cfg.ContainerRegistry, (GCPRegistryConfig{Project: "my-project", Location: "europe-west1"}); got.Provider != RegistryProviderGCP || got.GCP == nil || *got.GCP != want {
		t.Fatalf("container_registry = %+v, want the section's", got)
	}
	for name, replace := range map[string][2]string{
		"no project":          {"    project: my-project\n", ""},
		"no gcp section":      {"  gcp:\n    project: my-project\n    location: europe-west1\n", ""},
		"no provider":         {"  provider: gcp\n", ""},
		"an unknown provider": {"provider: gcp", "provider: elsewhere"},
		"a bad project":       {"my-project", "Not A Project"},
		"a bad location":      {"europe-west1", "somewhere"},
	} {
		if err := loadText(t, productionConfig(t)+strings.Replace(containerRegistrySection, replace[0], replace[1], 1)); err == nil {
			t.Errorf("%s: loaded, want it refused", name)
		}
	}
}

// The settings hold no secret: a key that once held one, or named the variable holding one, is
// refused as any unknown key is.
func TestASettingsFileWithASecretIsRefused(t *testing.T) {
	text := productionConfig(t)
	for name, edit := range map[string][2]string{
		"signing_secret":    {"  ui_origin:", "  signing_secret: x\n  ui_origin:"},
		"database url":      {"database:\n", "database:\n  url: postgres://x\n"},
		"metrics_db_url":    {"  metric_controller_port: 8084", "  metric_controller_port: 8084\n  metrics_db_url: http://x"},
		"oauth_token":       {"  anthropic:\n", "  anthropic:\n    oauth_token: x\n"},
		"api_key":           {"  anthropic:\n", "  openrouter:\n    api_key: x\n  anthropic:\n"},
		"secret_access_key": {"    access_key_id: hypothesisloop\n", "    access_key_id: hypothesisloop\n    secret_access_key: x\n"},
	} {
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the settings file has no %q", name, edit[0])
		}
		if err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheGitProxyIsTheAPIsGitPath(t *testing.T) {
	cfg, err := loadConfigText(t, productionConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GitProxyURL(); got != "http://host.example.invalid:8081/git" {
		t.Errorf("git proxy = %q, want public_api_url with /git after it", got)
	}
}

// A cluster_name names its account key one to one, so it is lowercase letters and digits joined
// by single dashes, and listed once.
func TestACloudComputeClusterNameIsOneItsAccountKeyCanBeMadeOf(t *testing.T) {
	for _, name := range []string{"vast", "gcp-europe-west4", "a1"} {
		if err := loadText(t, productionConfig(t)+"\ncloud_compute:\n"+strings.Replace(cluster, "cluster_name: a", "cluster_name: "+name, 1)); err != nil {
			t.Errorf("cluster_name %s: %v", name, err)
		}
	}
	for _, name := range []string{`""`, "Vast", "vast_b", "vast--b", "-vast", "vast-"} {
		if err := loadText(t, productionConfig(t)+"\ncloud_compute:\n"+strings.Replace(cluster, "cluster_name: a", "cluster_name: "+name, 1)); err == nil || !strings.Contains(err.Error(), "CLOUD_COMPUTE_<CLUSTER_NAME>_API_KEY") {
			t.Errorf("cluster_name %s: got %v, want it refused", name, err)
		}
	}
	if err := loadText(t, productionConfig(t)+"\ncloud_compute:\n"+cluster+cluster); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("one cluster_name twice: got %v, want it refused", err)
	}
}

// The committed cloud_compute example rents shapes of types the committed catalog prices, on
// vast.ai and Runpod at once, one cluster each.
func TestTheCommittedCloudComputeExampleLoadsWithTheCommittedCatalog(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "settings", "cloud-compute.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(example), Placeholder+"ACCOUNT-ID", "1")
	var section struct {
		CloudCompute []struct {
			ClusterName string `yaml:"cluster_name"`
			Provider    string `yaml:"provider"`
		} `yaml:"cloud_compute"`
	}
	if err := yaml.Unmarshal([]byte(text), &section); err != nil {
		t.Fatal(err)
	}
	var providers []string
	for _, cc := range section.CloudCompute {
		providers = append(providers, cc.Provider)
	}
	if !slices.Equal(providers, []string{"vast", "runpod"}) {
		t.Fatalf("the example's entries are on %v, want vast and runpod", providers)
	}
	if err := loadText(t, productionConfig(t)+"\n"+text); err != nil {
		t.Errorf("hypothesisloop.yaml with cloud-compute.yaml: %v", err)
	}
}

// A catalog string is written as the runtimes report it, and a rented shape offers only a priced
// type, or no job could ever be admitted to it.
func TestAcceleratorTypesAreCanonicalAndEveryRentedShapeIsPriced(t *testing.T) {
	const priced = "nvidia.com/gpu.product=NVIDIA-L40"
	shape := func(acceleratorType string) string {
		return "\ncloud_compute:\n" + cluster + "    shapes:\n      - name: gpu\n        accelerator_type: " + acceleratorType +
			"\n        accelerators: 1\n        cpu: \"8\"\n        memory: 32Gi\n        storage: 100Gi\n        max_instances: 1\n"
	}
	if err := loadText(t, productionConfig(t)+shape(priced)); err != nil {
		t.Fatalf("a shape of a priced type: %v", err)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"a catalog name GFD never labels": {strings.Replace(productionConfig(t), priced, "nvidia.com/gpu.product=NVIDIA L40", 1),
			`accelerator_types: accelerator type "nvidia.com/gpu.product=NVIDIA L40" must be written "nvidia.com/gpu.product=NVIDIA-L40"`},
		"a catalog name with no key": {strings.Replace(productionConfig(t), priced, "L40", 1), `accelerator type "L40" is not a key=value catalog string`},
		"a shape of an unpriced type": {productionConfig(t) + shape("nvidia.com/gpu.product=NVIDIA-B200"),
			`cloud_compute a shape "gpu": accelerator_type "nvidia.com/gpu.product=NVIDIA-B200" is not priced in accelerator_types`},
		"a shape of a type in another casing": {productionConfig(t) + shape("nvidia.com/gpu.product=NVIDIA-l40"),
			`cloud_compute a shape "gpu": accelerator_type "nvidia.com/gpu.product=NVIDIA-l40" is not priced in accelerator_types`},
	} {
		if err := loadText(t, tc.text); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error saying %q", name, err, tc.want)
		}
	}
}

// A sandbox or cloud_compute section names its provider and holds that provider's section alone
// beside its own keys: another provider's section, a second one or none at all is refused, since
// what the adapter does not read would be dropped with no word said.
func TestARentedSectionHoldsTheSectionOfTheProviderItNamesAndNoOther(t *testing.T) {
	sandbox := "\nsandbox:\n  provider: fly\n  name: main\n  reconcile_interval_seconds: 5\n  create_pause_milliseconds: 0\n  warm_pool_size: 0\n  fly: {}\n" +
		"  instance:\n    command: [run]\n    cpu: \"1\"\n    memory: 1Gi\n    storage: 1Gi\n"
	computed := "\ncloud_compute:\n" + cluster
	if err := loadText(t, productionConfig(t)+sandbox+computed); err != nil {
		t.Fatalf("a sandbox and a cloud_compute entry each with its provider's section: %v", err)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"sandbox: another provider's section":  {strings.Replace(sandbox, "  fly: {}\n", "  e2b: {}\n", 1), `"e2b" is neither a key of the section nor the section of its provider fly`},
		"sandbox: a second provider's section": {strings.Replace(sandbox, "  fly: {}\n", "  fly: {}\n  e2b: {}\n", 1), `"e2b" is neither`},
		"sandbox: no provider section":         {strings.Replace(sandbox, "  fly: {}\n", "", 1), "sandbox provider fly takes the sandbox section of the same name"},
		"sandbox: no provider":                 {strings.Replace(sandbox, "  provider: fly\n", "", 1), "sandbox provider is required"},
		"sandbox: a misspelt key":              {strings.Replace(sandbox, "  name: main\n", "  name: main\n  warm_pool: 1\n", 1), `"warm_pool" is neither`},
		"cloud_compute: another provider's section": {
			strings.Replace(computed, "    vast:\n      account_id: \"1\"\n", "    fly:\n      org: x\n", 1), `"fly" is neither a key of the section nor the section of its provider vast`},
		"cloud_compute: no provider section": {strings.Replace(computed, "    vast:\n      account_id: \"1\"\n", "", 1), "cloud_compute a provider vast takes"},
		"cloud_compute: no provider":         {strings.Replace(computed, "    provider: vast\n", "", 1), "cloud_compute a provider is required"},
	} {
		var text string
		if strings.HasPrefix(name, "sandbox") {
			text = productionConfig(t) + tc.text + computed
		} else {
			text = productionConfig(t) + sandbox + tc.text
		}
		if err := loadText(t, text); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error saying %q", name, err, tc.want)
		}
	}
}

// warm_pool_size and create_pause_milliseconds have no default in code: each is stated, 0 for
// none.
func TestASandboxStatesItsWarmPoolAndItsPause(t *testing.T) {
	sandbox := "\nsandbox:\n  provider: fly\n  name: main\n  reconcile_interval_seconds: 5\n  create_pause_milliseconds: 0\n  warm_pool_size: 2\n  fly: {}\n" +
		"  instance:\n    command: [run]\n    cpu: \"1\"\n    memory: 1Gi\n    storage: 1Gi\n"
	path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
	if err := os.WriteFile(path, []byte(productionConfig(t)+sandbox), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadSettings(path); err != nil || cfg.Sandbox == nil || *cfg.Sandbox.WarmPoolSize != 2 || cfg.Sandbox.CreatePause() != 0 {
		t.Fatalf("Load: %v, want a pool of 2 and no pause", err)
	}
	for name, tc := range map[string]struct{ from, to, want string }{
		"no warm_pool_size":            {"  warm_pool_size: 2\n", "", "sandbox warm_pool_size is required"},
		"a negative warm_pool_size":    {"  warm_pool_size: 2\n", "  warm_pool_size: -1\n", "sandbox warm_pool_size is required"},
		"no create_pause_milliseconds": {"  create_pause_milliseconds: 0\n", "", "sandbox create_pause_milliseconds is required"},
	} {
		if err := loadText(t, productionConfig(t)+strings.Replace(sandbox, tc.from, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

// The object store names its provider and holds that provider's section.
func TestTheObjectStoreHoldsTheSectionOfTheProviderItNames(t *testing.T) {
	const minio = "  provider: minio\n  minio:\n    access_key_id: hypothesisloop\n"
	if !strings.Contains(productionConfig(t), minio) {
		t.Fatalf("the committed object_store no longer starts with %q", minio)
	}
	for name, tc := range map[string]struct{ to, want string }{
		"no minio section":      {"  provider: minio\n", "object_store provider minio takes the object_store minio section"},
		"no access_key_id":      {"  provider: minio\n  minio: {}\n", "object_store provider minio takes the object_store minio section"},
		"a provider not built":  {"  provider: s3\n  minio:\n    access_key_id: hypothesisloop\n", `object_store provider "s3" is not a store provider this build knows`},
		"the minio secret here": {minio + "    secret_access_key: s\n", "secret_access_key"},
	} {
		if err := loadText(t, strings.Replace(productionConfig(t), minio, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestLoadRejectsAModelPolicyThatCannotBeApplied(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no catalog":                          {"    catalog:\n", "    old_catalog:\n"},
		"a default model outside the catalog": {"  default_model: anthropic/claude-sonnet-5", "  default_model: anthropic/claude-sonnet-4-6"},
		"a coordinator model outside it":      {"  coordinator_model: anthropic/claude-opus-5-5", "  coordinator_model: anthropic/claude-fable-5-1"},
		"a bare default model":                {"  default_model: anthropic/claude-sonnet-5", "  default_model: claude-sonnet-5"},
		"a default of no known provider":      {"  default_model: anthropic/claude-sonnet-5", "  default_model: openai/gpt-5"},
		"the old provider key":                {"  max_calls_in_flight_per_org: 64\n", "  max_calls_in_flight_per_org: 64\n  provider: anthropic\n"},
		"a model priced at nothing":           {"cache_read_usd_per_mtok: 0.01}", "cache_read_usd_per_mtok: 0}"},
		"no calls in flight":                  {"  max_calls_in_flight_per_org: 64\n", ""},
		"no default model budget":             {"  default_model_budget_usd: 50\n", ""},
		"a default above the most":            {"  default_model_budget_usd: 50", "  default_model_budget_usd: 501"},
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

func TestBudgetReferenceIsH100UnlessNamed(t *testing.T) {
	load := func(text string) *Settings {
		t.Helper()
		path := filepath.Join(t.TempDir(), "hypothesisloop.yaml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := load(productionConfig(t)).BudgetReference; got != "H100" {
		t.Errorf("committed budget_reference = %q, want H100", got)
	}
	without := strings.Replace(productionConfig(t), "budget_reference: H100\n", "", 1)
	if got := load(without).BudgetReference; got != "H100" {
		t.Errorf("budget_reference left out = %q, want H100", got)
	}
	named := strings.Replace(productionConfig(t), "budget_reference: H100\n", "budget_reference: B200\n", 1)
	if got := load(named).BudgetReference; got != "B200" {
		t.Errorf("budget_reference named = %q, want B200", got)
	}
}
