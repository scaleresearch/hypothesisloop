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
	// The launcher's own token is in no Secret: the pod's init container derives it, and the
	// chart's tests hold that (controlplane/infra/helm/chart_test.go).
	cfg, err := loadConfig(release.ConfigFile(t, "hypothesisloop-agent-launcher-config", "agent-launcher.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.New(cfg.Provider.Name, cfg.Provider.Settings, nil); err != nil {
		t.Fatalf("provider: %v", err)
	}
	for name, value := range env {
		if value == charttest.Secret["MODEL_ANTHROPIC_OAUTH_TOKEN"] {
			t.Errorf("the launcher is handed the model credential as %s: the agents' model calls go through control-service", name)
		}
	}
	if cfg.LauncherName != "main" {
		t.Errorf("launcher_name = %q, want the name the values give hosted agents to", cfg.LauncherName)
	}
}
