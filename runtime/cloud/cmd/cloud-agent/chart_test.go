package main

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/infra/helm/charttest"
)

// The control-plane chart mounts this process's config from a ConfigMap and hands it the
// Secret's keys as environment. Both are taken from a rendering of the chart, so a key the
// Deployment forgot fails here and not in a pod.
func TestTheChartStartsTheCloudAgentWithAConfigAndEnvironmentItAccepts(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	env := release.Env(t, "hypothesisloop-cloud-agent", "deployment-secrets")
	charttest.Setenv(t, env)

	if env["CLOUD_AGENT_CONFIG"] != "/settings/cloud-agent.yaml" {
		t.Errorf("CLOUD_AGENT_CONFIG = %q, want the mounted file", env["CLOUD_AGENT_CONFIG"])
	}
	// The runtime token is in no Secret: the pod's init container registers the cluster and
	// leaves its token, and the chart's tests hold that (controlplane/infra/helm/chart_test.go).
	cfg, err := loadConfig(release.ConfigFile(t, "hypothesisloop-cloud-agent-config", "cloud-agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newProvider(cfg.Provider, cfg.shapes); err != nil {
		t.Fatalf("provider: %v", err)
	}
	if cfg.ClusterName != "vast" || len(cfg.shapes) != 1 {
		t.Errorf("loaded cluster %q with %d shapes, want the values' config as it was written", cfg.ClusterName, len(cfg.shapes))
	}
}
