package config

import (
	"os/exec"
	"path/filepath"
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
// .env adds left out except the environments variables set.
func render(t *testing.T, repo, dir, rendered, environments string) {
	t.Helper()
	// The tag is stated: computing it reads the git state of the tree, which is absent in an
	// exported checkout and changes under a tree others are writing to.
	script := `. localdev/lib/auth.sh && HL_DEV_JWT_KEY="$1/key.pem" && HL_GIT_HOST_PROVIDER=local && HL_IDENTITY_PROVIDER="" && ` + environments + ` && TAG=abc1234 lib_render_settings host.example.internal "$2"`
	cmd := exec.Command("bash", "-c", script, "render", dir, rendered)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
}
