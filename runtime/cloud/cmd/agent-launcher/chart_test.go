package main

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/infra/helm/charttest"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/providers"
)

// The control-plane chart mounts this process's config from a ConfigMap and hands it the
// Secret's keys as environment. Both are taken from a rendering of the chart, so a key the
// Deployment forgot fails here and not in a pod.
func TestTheChartStartsTheLauncherWithAConfigAndEnvironmentItAccepts(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	env := release.Env(t, "hypothesisloop-agent-launcher", "deployment-secrets")
	charttest.Setenv(t, env)

	if env["AGENT_LAUNCHER_CONFIG"] != "/settings/agent-launcher.yaml" {
		t.Errorf("AGENT_LAUNCHER_CONFIG = %q, want the mounted file", env["AGENT_LAUNCHER_CONFIG"])
	}
	if env["LAUNCHER_TOKEN"] != charttest.Secret["AGENT_LAUNCHER_TOKEN"] {
		t.Errorf("LAUNCHER_TOKEN = %q, want the Secret's AGENT_LAUNCHER_TOKEN", env["LAUNCHER_TOKEN"])
	}
	cfg, err := loadConfig(release.ConfigFile(t, "hypothesisloop-agent-launcher-config", "agent-launcher.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.New(cfg.Provider.Name, cfg.Provider.Settings, nil); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if cfg.claudeToken != charttest.Secret["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Errorf("the agents' Claude credential = %q, want the Secret's", cfg.claudeToken)
	}
	if cfg.LauncherName != "main" {
		t.Errorf("launcher_name = %q, want the name the values give hosted agents to", cfg.LauncherName)
	}
}
