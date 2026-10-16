package config

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The local control plane and hl-admin load what `make render-settings` writes. A key the loader
// newly requires must be in the committed settings that rendering starts from, or neither starts.
func TestTheLocalSettingsAsRenderedLoad(t *testing.T) {
	for _, tool := range []string{"bash", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on this machine", tool)
		}
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rendered := filepath.Join(dir, "rendered.yaml")
	// The key is made in the test's directory, and what a developer's .env adds is left out: its
	// secrets, its identity provider and its registry are that machine's.
	render(t, repo, dir, rendered, `HL_ENVIRONMENTS_REPOSITORY=""`)
	t.Setenv("HYPOTHESISLOOP_SIGNING_SECRET", "a-signing-secret-only-this-test-knows")
	t.Setenv("DATABASE_URL", "postgres://test.example.invalid/hypothesisloop")
	t.Setenv("DATA_STORE_SECRET_ACCESS_KEY", "a-store-secret-only-this-test-knows")
	t.Setenv("MODEL_ANTHROPIC_OAUTH_TOKEN", "a-model-credential-only-this-test-knows")
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD", "a-password-only-this-test-knows")
	cfg, err := Load(rendered)
	if err != nil {
		t.Fatalf("the rendered local settings do not load: %v", err)
	}
	if providers := cfg.Auth.IdentityProviders; len(providers) != 1 || providers[0].Issuer != "http://localhost/hypothesisloop-dev" ||
		providers[0].Audience != "hypothesisloop-dev" || len(providers[0].JWTKeys) != 1 || providers[0].JWTKeys[0].Kid != "local-dev" {
		t.Errorf("identity providers = %+v, want the one key made on this machine", providers)
	}
	if want := (PlainGitConfig{BaseURL: "http://host.example.internal:8085", AllowHTTP: true, Username: "hypothesisloop", Password: "a-password-only-this-test-knows"}); cfg.GitHost.Provider != GitHostProviderPlain || cfg.GitHost.Plain == nil || *cfg.GitHost.Plain != want {
		t.Errorf("git host = %s, want the git server of this machine", cfg.GitHost.Provider)
	}
	if cfg.Agents.Launcher == "" || cfg.Agents.DefaultModel == "" || cfg.Agents.BootLimitSeconds <= 0 {
		t.Errorf("agents = %+v, want every key set", cfg.Agents)
	}
	if cfg.Environments != nil {
		t.Errorf("environments = %+v, want none without a registry in .env", cfg.Environments)
	}
	if want := (RuntimesConfig{MetricsURL: "http://host.example.internal:8084", Registry: "host.example.internal:5000", ImageTag: "abc1234", RegistryPlainHTTP: true}); cfg.Runtimes != want {
		t.Errorf("runtimes = %+v, want %+v: the dev registry serves plain HTTP", cfg.Runtimes, want)
	}

	// With a registry in .env the environments are the ones beside it.
	registry := `lib_env_value() { case "$1" in CONTAINER_REGISTRY_PROVIDER) echo gcp ;; CONTAINER_REGISTRY_GCP_PROJECT) echo my-project ;;
		CONTAINER_REGISTRY_GCP_LOCATION) echo europe-west1 ;; CONTAINER_REGISTRY_GCP_REPOSITORY) echo images ;; esac; } && lib_container_registry`
	render(t, repo, dir, rendered, registry)
	if cfg, err = Load(rendered); err != nil {
		t.Fatalf("the rendered local settings with environments do not load: %v", err)
	}
	if want := (EnvironmentsConfig{Repository: "europe-west1-docker.pkg.dev/my-project/images-environments/environment", Provider: RegistryProviderGCP,
		GCP: GCPRegistryConfig{Pusher: "env-pusher@my-project.iam.gserviceaccount.com", Puller: "env-puller@my-project.iam.gserviceaccount.com"}}); cfg.Environments == nil || *cfg.Environments != want {
		t.Errorf("environments = %+v, want %+v", cfg.Environments, want)
	}

	// The committed file is the one source: without rendering it is refused for its placeholders.
	if _, err := Load(filepath.Join(repo, "controlplane/settings/hypothesisloop.yaml")); err == nil {
		t.Error("the committed settings loaded without being rendered")
	}
}

// render runs lib_render_settings as make does, with the key made in dir and what a developer's
// .env adds left out except the environments variables set. The .env LOCAL_REACHABLE_ADDRESS is
// read from is dir's, which holds none unless the test writes one.
func render(t *testing.T, repo, dir, rendered, environments string) {
	t.Helper()
	// The tag is stated: computing it reads the git state of the tree, which is absent in an
	// exported checkout and changes under a tree others are writing to.
	script := `. localdev/lib/auth.sh && HL_DEV_JWT_KEY="$1/key.pem" && HL_GIT_HOST_PROVIDER=local && HL_IDENTITY_PROVIDER="" && LIB_NODE_REPO="$1" && ` + environments + ` && TAG=abc1234 lib_render_settings host.example.internal "$2"`
	cmd := exec.Command("bash", "-c", script, "render", dir, rendered)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
}

// A cluster on another machine is attached to the local stack with one .env key: every address
// it, its agents and its jobs are handed is that one, and none is the name only local containers
// resolve.
func TestTheLocalReachableAddressIsTheOneAddressEveryClusterIsHanded(t *testing.T) {
	for _, tool := range []string{"bash", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on this machine", tool)
		}
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LOCAL_REACHABLE_ADDRESS='100.64.0.5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rendered := filepath.Join(dir, "rendered.yaml")
	render(t, repo, dir, rendered, `HL_ENVIRONMENTS_REPOSITORY=""`)
	t.Setenv("HYPOTHESISLOOP_SIGNING_SECRET", "a-signing-secret-only-this-test-knows")
	t.Setenv("DATABASE_URL", "postgres://test.example.invalid/hypothesisloop")
	t.Setenv("DATA_STORE_SECRET_ACCESS_KEY", "a-store-secret-only-this-test-knows")
	t.Setenv("MODEL_ANTHROPIC_OAUTH_TOKEN", "a-model-credential-only-this-test-knows")
	t.Setenv("HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD", "a-password-only-this-test-knows")
	cfg, err := Load(rendered)
	if err != nil {
		t.Fatalf("the rendered local settings do not load: %v", err)
	}
	for what, got := range map[string]string{
		"services.public_api_url": cfg.Services.PublicAPIURL, "runtimes.metrics_url": cfg.Runtimes.MetricsURL,
		"runtimes.registry": cfg.Runtimes.Registry, "data_store.endpoint": cfg.DataStore.Endpoint, "git_host.plain.base_url": cfg.GitHost.Plain.BaseURL,
		"git_host.proxy_url": cfg.GitHost.ProxyURL,
	} {
		if !strings.Contains(got, "100.64.0.5:") {
			t.Errorf("%s = %q, want it at the reachable address", what, got)
		}
	}
	if !cfg.Runtimes.RegistryPlainHTTP {
		t.Error("runtimes.registry_plain_http = false, want the dev registry said to serve plain HTTP")
	}

	// A local node pulls an image named at the reachable address through its own route.
	out, err := bashIn(repo, `. localdev/lib/node.sh && LIB_NODE_REPO="$1" && lib_registries_yaml host.example.internal`, dir)
	if err != nil {
		t.Fatalf("lib_registries_yaml: %v\n%s", err, out)
	}
	if want := "\"100.64.0.5:5000\":\n    endpoint:\n      - \"http://host.example.internal:5000\""; !strings.Contains(string(out), want) {
		t.Errorf("registries.yaml =\n%s\nwant the reachable name mirrored to the local route:\n%s", out, want)
	}
}

func TestALocalReachableAddressOnlyThisMachineReachesIsRefused(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on this machine")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for address, accepted := range map[string]bool{
		"100.64.0.5": true, "box.example.net": true, "": true,
		"localhost": false, "127.0.0.1": false, "host.containers.internal": false, "http://100.64.0.5": false, "100.64.0.5:5000": false,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LOCAL_REACHABLE_ADDRESS='"+address+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := bashIn(repo, `. localdev/lib/node.sh && LIB_NODE_REPO="$1" && lib_advertised_address host.example.internal`, dir)
		switch {
		case accepted && err != nil:
			t.Errorf("LOCAL_REACHABLE_ADDRESS=%q refused: %s", address, out)
		case accepted && string(out) != cmp.Or(address, "host.example.internal"):
			t.Errorf("LOCAL_REACHABLE_ADDRESS=%q advertises %q", address, out)
		case !accepted && err == nil:
			t.Errorf("LOCAL_REACHABLE_ADDRESS=%q accepted, advertising %q", address, out)
		}
	}
}

func bashIn(repo, script string, args ...string) ([]byte, error) {
	cmd := exec.Command("bash", append([]string{"-c", script, "bash"}, args...)...)
	cmd.Dir = repo
	return cmd.CombinedOutput()
}
