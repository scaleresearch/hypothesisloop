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
	// The key is made in the test's directory, and the section a developer's .env adds is left
	// out: its secrets are that machine's.
	script := `. localdev/lib/auth.sh && HL_DEV_JWT_KEY="$1/key.pem" && HL_GITHUB_APP_ID="" && lib_render_settings host.example.internal "$2"`
	cmd := exec.Command("bash", "-c", script, "render", dir, rendered)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	t.Setenv("HYPOTHESISLOOP_SIGNING_SECRET", "a-signing-secret-only-this-test-knows")
	cfg, err := Load(rendered)
	if err != nil {
		t.Fatalf("the rendered local settings do not load: %v", err)
	}
	if cfg.Agents.Launcher == "" || cfg.Agents.DefaultModel == "" || cfg.Agents.BootLimitSeconds <= 0 {
		t.Errorf("agents = %+v, want every key set", cfg.Agents)
	}

	// The committed file is the one source: without rendering it is refused for its placeholders.
	if _, err := Load(filepath.Join(repo, "controlplane/settings/hypothesisloop.yaml")); err == nil {
		t.Error("the committed settings loaded without being rendered")
	}
}
