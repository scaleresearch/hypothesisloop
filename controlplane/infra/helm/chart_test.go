package helm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/infra/helm/charttest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

// The Secret each ci values file names.
var secretNames = map[string]string{charttest.InCluster: "hypothesisloop", charttest.External: "deployment-secrets"}

var configLoaders = []string{"hypothesisloop-control-service", "hypothesisloop-metrics-service"}

func loadRendered(t *testing.T, release *charttest.Release, values, deployment string) *config.Config {
	t.Helper()
	env := release.Env(t, deployment, secretNames[values])
	if env["HYPOTHESISLOOP_CONFIG"] != "/settings/hypothesisloop.yaml" {
		t.Fatalf("%s: HYPOTHESISLOOP_CONFIG = %q, want the mounted file", deployment, env["HYPOTHESISLOOP_CONFIG"])
	}
	charttest.Setenv(t, env)
	cfg, err := config.Load(release.ConfigFile(t, "hypothesisloop-config", "hypothesisloop.yaml"))
	if err != nil {
		t.Fatalf("%s with %s values: the rendered config does not load: %v", deployment, values, err)
	}
	return cfg
}

func TestTheRenderedConfigLoadsWithTheEnvironmentEachServiceIsGiven(t *testing.T) {
	for _, values := range []string{charttest.InCluster, charttest.External} {
		release := charttest.Render(t, values)
		for _, deployment := range configLoaders {
			t.Run(values+"/"+deployment, func(t *testing.T) {
				cfg := loadRendered(t, release, values, deployment)
				if got := release.Env(t, deployment, secretNames[values])["DATABASE_URL"]; got != charttest.Secret["DATABASE_URL"] {
					t.Errorf("DATABASE_URL = %q, want the Secret's", got)
				}
				if cfg.Auth.SigningSecret != charttest.Secret["HYPOTHESISLOOP_SIGNING_SECRET"] {
					t.Errorf("signing secret = %q, want the Secret's", cfg.Auth.SigningSecret)
				}
				if cfg.DataStore.AccessKeyID != charttest.Secret["OBJECT_STORE_ACCESS_KEY_ID"] ||
					cfg.DataStore.SecretAccessKey != charttest.Secret["OBJECT_STORE_SECRET_ACCESS_KEY"] {
					t.Errorf("object store key = %q / %q, want the Secret's", cfg.DataStore.AccessKeyID, cfg.DataStore.SecretAccessKey)
				}
				if cfg.DataStore.Bucket != "hypothesisloop-data" || cfg.DataStore.MaxBytesPerAgent == 0 || len(cfg.AcceleratorTypes) == 0 {
					t.Errorf("platform policy did not come through from the settings file: %+v", cfg.DataStore)
				}
			})
		}
	}
}

func TestEachValueLandsInItsOwnSectionOfTheConfig(t *testing.T) {
	inCluster := loadRendered(t, charttest.Render(t, charttest.InCluster), charttest.InCluster, configLoaders[0])
	if got := inCluster.Auth; got.Issuer != "" || got.Audience != "" || len(got.JWTKeys) != 0 || got.UIOrigin != "http://ui.example.invalid:30300" {
		t.Errorf("in-cluster auth = %+v, want no identity provider and the UI's origin", got)
	}
	if got, want := inCluster.Agents, (config.AgentsConfig{
		Launcher: "main",
		Image:    "registry.example.invalid/hypothesisloop-experimentator-base:test",
		APIURL:   "http://api.example.invalid:30081",
		// Not a value of the chart: the staged settings file decides it.
		DefaultModel:                   "claude-sonnet-5",
		BootLimitSeconds:               300,
		MaxNewInstancesPerAnswer:       10,
		MaxHostedPerPlatformExperiment: 25,
	}); got != want {
		t.Errorf("in-cluster agents = %+v, want %+v", got, want)
	}
	if got, want := inCluster.Runtimes, (config.RuntimesConfig{
		APIURL:     "http://api.example.invalid:30081",
		MetricsURL: "http://metrics.example.invalid:30084",
	}); got != want {
		t.Errorf("in-cluster runtimes = %+v, want %+v", got, want)
	}
	if got := inCluster.DataStore.Endpoint; got != "http://object-store.hypothesisloop.svc:9000" {
		t.Errorf("in-cluster data_store.endpoint = %q", got)
	}
	if got := inCluster.Services; got.MetricsDBURL != "http://greptimedb:4000" || got.GraphDBURL != "bolt://memgraph:7687" {
		t.Errorf("in-cluster services = %+v, want the in-cluster stores", got)
	}

	external := loadRendered(t, charttest.Render(t, charttest.External), charttest.External, configLoaders[0])
	wantKey := config.JWTKey{Kid: "made-up-key", Kty: "EC", Alg: "ES256", Crv: "P-256",
		X: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", Y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}
	auth := external.Auth
	if auth.Issuer != "https://project.supabase.example.invalid/auth/v1" || auth.Audience != "authenticated" ||
		len(auth.JWTKeys) != 1 || auth.JWTKeys[0] != wantKey || auth.UIOrigin != "https://ui.example.invalid" {
		t.Errorf("external auth = %+v, want the identity provider of the values", auth)
	}
}

func TestNoSecretIsCreatedAndNoSecretValueIsRendered(t *testing.T) {
	for _, values := range []string{charttest.InCluster, charttest.External} {
		release := charttest.Render(t, values)
		if names := release.Kinds["Secret"]; len(names) != 0 {
			t.Errorf("%s: the chart created Secrets %q; it reads one and creates none", values, names)
		}
		for key, value := range charttest.Secret {
			if strings.Contains(release.Text, value) {
				t.Errorf("%s: the value of Secret key %s is in the rendered manifests", values, key)
			}
		}
		rendered := release.ConfigMaps["hypothesisloop-config"].Data["hypothesisloop.yaml"]
		for _, reference := range []string{
			`signing_secret: "${HYPOTHESISLOOP_SIGNING_SECRET}"`,
			`access_key_id: "${OBJECT_STORE_ACCESS_KEY_ID}"`,
			`secret_access_key: "${OBJECT_STORE_SECRET_ACCESS_KEY}"`,
		} {
			if !strings.Contains(rendered, reference) {
				t.Errorf("%s: the rendered config has no %s, so that secret is a literal in a ConfigMap", values, reference)
			}
		}
	}
}

func TestEveryPodReadsTheOneNamedSecret(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	if strings.Contains(release.Text, "name: hypothesisloop\n") {
		t.Error("something still reads a Secret by the chart's old default name")
	}
	if got := strings.Count(release.Text, "secretKeyRef:"); got != strings.Count(release.Text, "name: deployment-secrets\n") {
		t.Errorf("%d secretKeyRefs, and not all of them name deployment-secrets", got)
	}
	for name, deployment := range release.Deployments {
		reads := name != "hypothesisloop-ui"
		if _, stamped := deployment.Spec.Template.Annotations["checksum/secret"]; stamped != reads {
			t.Errorf("Deployment %s: checksum/secret stamped = %v, want %v so a changed Secret restarts exactly its readers", name, stamped, reads)
		}
	}
}

func TestTwoRendersOfTheSameValuesAreIdentical(t *testing.T) {
	for _, values := range []string{charttest.InCluster, charttest.External} {
		if first, second := charttest.Render(t, values).Text, charttest.Render(t, values).Text; first != second {
			t.Errorf("%s: two renders differ, so every upgrade would restart the pods", values)
		}
	}
}

func TestPostgresRunsInTheClusterOnlyWhenAskedTo(t *testing.T) {
	inCluster := charttest.Render(t, charttest.InCluster)
	if !slices.Contains(inCluster.Kinds["StatefulSet"], "hypothesisloop-postgres") || !strings.Contains(inCluster.Text, "wait-for-postgres") {
		t.Error("in-cluster values rendered no Postgres or nothing waits for it")
	}
	external := charttest.Render(t, charttest.External)
	for _, trace := range []string{"hypothesisloop-postgres", "wait-for-postgres", "POSTGRES_PASSWORD"} {
		if strings.Contains(external.Text, trace) {
			t.Errorf("external values still render %s", trace)
		}
	}
}

func TestTheRuntimeProcessesAreOffUntilEnabled(t *testing.T) {
	inCluster := charttest.Render(t, charttest.InCluster)
	for _, name := range []string{"hypothesisloop-cloud-agent", "hypothesisloop-agent-launcher"} {
		if _, rendered := inCluster.Deployments[name]; rendered {
			t.Errorf("%s is rendered without being enabled", name)
		}
		if _, rendered := charttest.Render(t, charttest.External).Deployments[name]; !rendered {
			t.Errorf("%s is enabled and not rendered", name)
		}
	}
}

// The github section is in no staged file: the chart adds it for a deployment with an App, the
// ids as values and the two secrets as references both services resolve from the Secret.
func TestTheGitHubAppReachesTheConfigOnlyWhenTheDeploymentHasOne(t *testing.T) {
	if cfg := loadRendered(t, charttest.Render(t, charttest.InCluster), charttest.InCluster, configLoaders[0]); cfg.GitHub.Configured() {
		t.Errorf("in-cluster github = %+v, want no App", cfg.GitHub.AppID)
	}
	release := charttest.Render(t, charttest.External)
	for _, deployment := range configLoaders {
		got := loadRendered(t, release, charttest.External, deployment).GitHub
		want := config.GitHubConfig{AppID: 4242, AppSlug: "made-up-app", ClientID: "Iv23made-up",
			ClientSecret: charttest.Secret["HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET"], PrivateKey: charttest.Secret["HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY"]}
		if got != want {
			t.Errorf("%s: github app %d (%s, %s), or its secrets, are not the values' and the Secret's", deployment, got.AppID, got.AppSlug, got.ClientID)
		}
	}
}

// A runtime's token is derived from the signing secret, which only the control plane's tool
// reads. The pod runs that tool first, with what control-service is started with, and the
// process is started with what it printed.
func TestEachRuntimeProcessTakesItsTokenFromTheControlPlanesTool(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	for deployment, want := range map[string]struct{ script, start string }{
		"hypothesisloop-cloud-agent": {
			"/hl-admin cluster add vast default > /token/token && /hl-admin cluster share vast on",
			`RUNTIME_TOKEN="$(cat /token/token)" exec /cloud-agent`},
		"hypothesisloop-agent-launcher": {
			"/hl-admin token launcher:main > /token/token",
			`LAUNCHER_TOKEN="$(cat /token/token)" exec /agent-launcher`},
	} {
		script, env := release.TokenInit(t, deployment, secretNames[charttest.External])
		if script != want.script {
			t.Errorf("%s: the init container runs %q, want %q", deployment, script, want.script)
		}
		charttest.Setenv(t, env)
		if _, err := config.Load(release.ConfigFile(t, "hypothesisloop-config", "hypothesisloop.yaml")); err != nil {
			t.Errorf("%s: hl-admin could not load the config with the init container's environment: %v", deployment, err)
		}
		if env["DATABASE_URL"] != charttest.Secret["DATABASE_URL"] {
			t.Errorf("%s: the init container has no DATABASE_URL, which registering a cluster needs", deployment)
		}
		pod := release.Deployments[deployment].Spec.Template.Spec
		if command := pod.Containers[0].Command; len(command) != 3 || command[2] != want.start {
			t.Errorf("%s: started with %q, want %q", deployment, command, want.start)
		}
		for _, entry := range pod.Containers[0].Env {
			if entry.ValueFrom != nil && (entry.ValueFrom.SecretKeyRef.Key == "HYPOTHESISLOOP_SIGNING_SECRET" || entry.ValueFrom.SecretKeyRef.Key == "DATABASE_URL") {
				t.Errorf("%s: the process itself is handed %s", deployment, entry.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
}

func TestTheTunnelledPortsAreNodePorts(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	for name, want := range map[string][2]int32{
		"control-service": {8081, 30081},
		"metrics-service": {8084, 30084},
		"object-store":    {9000, 30900},
	} {
		ports := release.Services[name].Spec.Ports
		if len(ports) != 1 || ports[0].Port != want[0] || ports[0].NodePort != want[1] {
			t.Errorf("Service %s ports = %+v, want %d on node port %d", name, ports, want[0], want[1])
		}
	}
}

func TestTheUIIsGivenItsPublicValues(t *testing.T) {
	env := charttest.Render(t, charttest.External).Env(t, "hypothesisloop-ui", secretNames[charttest.External])
	if env["NEXT_PUBLIC_SUPABASE_URL"] != "https://project.supabase.example.invalid" || env["NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY"] != "sb_publishable_made-up" {
		t.Errorf("UI environment = %v", env)
	}
}

func TestValuesTheChartCannotUseAreRefusedWithTheirName(t *testing.T) {
	for name, refused := range map[string]struct{ values, set, complaint string }{
		"no secret name":              {charttest.InCluster, `secretName=""`, "secretName is required"},
		"no image registry":           {charttest.InCluster, `images.registry=""`, "images.registry is required"},
		"no image tag":                {charttest.InCluster, `images.tag=""`, "images.tag is required"},
		"no UI origin":                {charttest.InCluster, `auth.uiOrigin=""`, "auth.uiOrigin is required"},
		"no launcher":                 {charttest.InCluster, `agents.launcher=""`, "agents.launcher is required"},
		"no agent image":              {charttest.InCluster, `agents.image=""`, "agents.image is required"},
		"no agent API address":        {charttest.InCluster, `agents.apiURL=""`, "agents.apiURL is required"},
		"no runtime API address":      {charttest.InCluster, `runtimes.apiURL=""`, "runtimes.apiURL is required"},
		"no runtime metrics address":  {charttest.InCluster, `runtimes.metricsURL=""`, "runtimes.metricsURL is required"},
		"no object store address":     {charttest.InCluster, `objectStore.endpoint=""`, "objectStore.endpoint is required"},
		"no Supabase address":         {charttest.InCluster, `ui.supabaseURL=""`, "ui.supabaseURL is required"},
		"no publishable key":          {charttest.InCluster, `ui.supabasePublishableKey=""`, "ui.supabasePublishableKey is required"},
		"cloud-agent without config":  {charttest.InCluster, `cloudAgent.enabled=true`, "cloudAgent.config is required"},
		"launcher without config":     {charttest.InCluster, `agentLauncher.enabled=true`, "agentLauncher.config is required"},
		"cloud-agent with no cluster": {charttest.External, `cloudAgent.clusterName=""`, "cloudAgent.clusterName is required"},
		"cloud-agent with no org":     {charttest.External, `cloudAgent.org=""`, "cloudAgent.org is required"},
		"a cluster name with a shell": {charttest.External, `cloudAgent.clusterName="a; rm"`, "/cloudAgent/clusterName"},
		"an App with no client id":    {charttest.External, `github.clientId=""`, "github.clientId is required"},
		"keys as text":                {charttest.External, `auth.jwtKeys="kid"`, "/auth/jwtKeys"},
		"a key field nobody reads":    {charttest.External, `auth.jwtKeys=[{"kid":"a","kty":"EC","alg":"ES256","use":"sig"}]`, "/auth/jwtKeys/0"},
		"an origin that is a number":  {charttest.External, `auth.uiOrigin=3000`, "/auth/uiOrigin"},
		"a flag that is text":         {charttest.External, `postgres.enabled="no"`, "/postgres/enabled"},
		"a secret passed as a value":  {charttest.External, `auth.signingSecret="made-up"`, "signingSecret"},
		"the removed extra section":   {charttest.External, `config={"extra":"auth: {}"}`, "config"},
		"env names as one string":     {charttest.External, `cloudAgent.secretEnv="VAST_API_KEY"`, "/cloudAgent/secretEnv"},
	} {
		if complaint := charttest.RenderError(t, refused.values, refused.set); !strings.Contains(complaint, refused.complaint) {
			t.Errorf("%s: helm said %q, want it to name %q", name, complaint, refused.complaint)
		}
	}
}
