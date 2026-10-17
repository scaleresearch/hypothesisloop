package helm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/infra/helm/charttest"
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

// googleCredentialFiles is each file control-service has mounted from the Secret, by path, and
// the key it holds.
func googleCredentialFiles(release *charttest.Release, secretName string) map[string]string {
	pod := release.Deployments["hypothesisloop-control-service"].Spec.Template.Spec
	var mount string
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "credentials" && m.SubPath == "" {
			mount = m.MountPath
		}
	}
	files := map[string]string{}
	for _, volume := range pod.Volumes {
		if volume.Name == "credentials" && volume.Secret != nil && volume.Secret.SecretName == secretName {
			for _, item := range volume.Secret.Items {
				files[mount+"/"+item.Path] = item.Key
			}
		}
	}
	return files
}

// A deployment that builds environments names their repository and the two service accounts in
// the config, and control-service reads its own Google identity, which mints their tokens, as a
// file of the Secret in a mounted directory. One that builds none reads no Google credential.
func TestEnvironmentsOfTheValuesReachTheConfigWithTheGoogleIdentityThatMintsTheirTokens(t *testing.T) {
	release := charttest.Render(t, charttest.External)
	env := release.Env(t, "hypothesisloop-control-service", secretNames[charttest.External])
	for name := range env {
		if strings.HasPrefix(name, "HYPOTHESISLOOP_ENVIRONMENT_BUILDER") {
			t.Errorf("control-service is handed %s", name)
		}
	}
	files := googleCredentialFiles(release, secretNames[charttest.External])
	if len(files) != 1 || files[env["GOOGLE_APPLICATION_CREDENTIALS"]] != "ENVIRONMENTS_GCP_CREDENTIALS" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q among the mounted files %v, want the one file of Secret key ENVIRONMENTS_GCP_CREDENTIALS", env["GOOGLE_APPLICATION_CREDENTIALS"], files)
	}
	if _, known := charttest.Secret["ENVIRONMENTS_GCP_CREDENTIALS"]; !known {
		t.Error("charttest.Secret has no ENVIRONMENTS_GCP_CREDENTIALS")
	}
	for _, deployment := range configLoaders {
		cfg := loadRendered(t, release, charttest.External, deployment)
		want := config.EnvironmentsConfig{Repository: "europe-west1-docker.pkg.dev/made-up-project/hypothesisloop-environments/environment", Provider: config.RegistryProviderGCP,
			GCP: config.GCPRegistryConfig{Pusher: "env-pusher@made-up-project.iam.gserviceaccount.com", Puller: "env-puller@made-up-project.iam.gserviceaccount.com"}}
		if cfg.Environments == nil || *cfg.Environments != want {
			t.Errorf("%s: environments = %+v, want the values'", deployment, cfg.Environments)
		}
	}

	inCluster := charttest.Render(t, charttest.InCluster)
	if env := inCluster.Env(t, "hypothesisloop-control-service", secretNames[charttest.InCluster]); env["GOOGLE_APPLICATION_CREDENTIALS"] != "" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q on a deployment that builds no environments", env["GOOGLE_APPLICATION_CREDENTIALS"])
	}
	if files := googleCredentialFiles(inCluster, secretNames[charttest.InCluster]); len(files) != 0 {
		t.Errorf("mounted %v on a deployment that builds no environments", files)
	}
	if cfg := loadRendered(t, inCluster, charttest.InCluster, "hypothesisloop-control-service"); cfg.Environments != nil {
		t.Errorf("environments = %+v, want none", cfg.Environments)
	}
}

func TestTwoRendersOfTheSameValuesAreIdentical(t *testing.T) {
	for _, values := range []string{charttest.InCluster, charttest.External} {
		if first, second := charttest.Render(t, values).Text, charttest.Render(t, values).Text; first != second {
			t.Errorf("%s: two renders differ, so every upgrade would restart the pods", values)
		}
	}
}

// The staged file names no git host. The chart adds the one gitHost.provider names: the App, the
// ids as values and the two secrets as references both services resolve from the Secret, or the
// plain host of the values with its password from the Secret.
func TestTheGitHostOfTheValuesReachesTheConfig(t *testing.T) {
	inCluster := charttest.Render(t, charttest.InCluster)
	release := charttest.Render(t, charttest.External)
	for _, deployment := range configLoaders {
		plain := loadRendered(t, inCluster, charttest.InCluster, deployment)
		if want := (config.PlainGitConfig{BaseURL: "https://git.example.invalid/experiments", Username: "made-up-git-account",
			Password: charttest.Secret["HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD"]}); plain.GitHost.Provider != config.GitHostProviderPlain || plain.GitHost.GitHub != nil || plain.GitHost.Plain == nil || *plain.GitHost.Plain != want {
			t.Errorf("%s: in-cluster git host is %s, want the values' plain host and the Secret's password", deployment, plain.GitHost.Provider)
		}
		cfg := loadRendered(t, release, charttest.External, deployment)
		want := config.GitHubConfig{AppID: 4242, AppSlug: "made-up-app", PrivateKey: charttest.Secret["HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY"]}
		if got := cfg.GitHost; got.Provider != config.GitHostProviderGitHub || got.Plain != nil || got.GitHub == nil || *got.GitHub != want {
			t.Errorf("%s: the git host is %q, or its App or key are not the values' and the Secret's", deployment, got.Provider)
		}
	}
}

func TestValuesTheChartCannotUseAreRefusedWithTheirName(t *testing.T) {
	for name, refused := range map[string]struct{ values, set, complaint string }{
		"no secret name":              {charttest.InCluster, `secretName=""`, "secretName is required"},
		"no image registry":           {charttest.InCluster, `images.registry=""`, "images.registry is required"},
		"no image tag":                {charttest.InCluster, `images.tag=""`, "images.tag is required"},
		"no UI origin":                {charttest.InCluster, `auth.uiOrigin=""`, "auth.uiOrigin is required"},
		"no agent image":              {charttest.InCluster, `agents.image=""`, "agents.image is required"},
		"no API address":              {charttest.InCluster, `publicAPIURL=""`, "publicAPIURL is required"},
		"an environment builder":      {charttest.InCluster, `environmentBuilder.address=tcp://builder.example.invalid:31234`, "environmentBuilder"},
		"environments with no pusher": {charttest.External, `environments.gcp.pusher=""`, "environments.gcp.pusher is required"},
		"environments with no puller": {charttest.External, `environments.gcp.puller=""`, "environments.gcp.puller is required"},
		"environments of no provider": {charttest.External, `environments.provider=""`, "with environments.repository it is gcp"},
		"accounts with no repository": {charttest.External, `environments.repository=""`, "without environments.repository"},
		"the API address per reader":  {charttest.InCluster, `agents.apiURL=http://api.example.invalid`, "apiURL"},
		"no runtime metrics address":  {charttest.InCluster, `runtimes.metricsURL=""`, "runtimes.metricsURL is required"},
		"no object store address":     {charttest.InCluster, `objectStore.endpoint=""`, "objectStore.endpoint is required"},
		"a store elsewhere, unnamed":  {charttest.InCluster, `objectStore.enabled=false`, "objectStore.controlPlaneEndpoint is required"},
		"two addresses for one store": {charttest.InCluster, `objectStore.controlPlaneEndpoint="http://store.example.invalid:9000"`, "objectStore.controlPlaneEndpoint is set while"},
		"no sign-in service":          {charttest.InCluster, `ui.identity.provider=""`, "with ui.enabled it is supabase"},
		"no Supabase address":         {charttest.InCluster, `ui.identity.supabase.url=""`, "ui.identity.supabase.url is required"},
		"no publishable key":          {charttest.InCluster, `ui.identity.supabase.publishableKey=""`, "ui.identity.supabase.publishableKey is required"},
		"an App with no slug":         {charttest.External, `gitHost.github.appSlug=""`, "gitHost.github.appSlug is required"},
		"an App with no id":           {charttest.External, `gitHost.github.appId=0`, "gitHost.github.appId is required"},
		"no git host":                 {charttest.External, `gitHost.provider=""`, "gitHost/provider"},
		"a git host nobody wrote":     {charttest.External, `gitHost.provider="gitlab"`, "gitHost/provider"},
		"plain with no root":          {charttest.InCluster, `gitHost.plain.baseURL=""`, "gitHost.plain.baseURL is required"},
		"providers as text":           {charttest.External, `auth.identityProviders="supabase"`, "/auth/identityProviders"},
		"a provider with no audience": {charttest.External, `auth.identityProviders=[{"issuer":"https://a.example","jwt_keys":[{"kid":"a","kty":"EC","alg":"ES256"}]}]`, "/auth/identityProviders/0"},
		"a provider with no keys":     {charttest.External, `auth.identityProviders=[{"issuer":"https://a.example","audience":"a","jwt_keys":[]}]`, "/auth/identityProviders/0/jwt_keys"},
		"a key field nobody reads":    {charttest.External, `auth.identityProviders=[{"issuer":"https://a.example","audience":"a","jwt_keys":[{"kid":"a","kty":"EC","alg":"ES256","use":"sig"}]}]`, "/auth/identityProviders/0/jwt_keys/0"},
		"the issuer outside the list": {charttest.External, `auth.issuer="https://a.example"`, "issuer"},
		"an origin that is a number":  {charttest.External, `auth.uiOrigin=3000`, "/auth/uiOrigin"},
		"a flag that is text":         {charttest.External, `postgres.enabled="no"`, "/postgres/enabled"},
		"a secret passed as a value":  {charttest.External, `auth.signingSecret="made-up"`, "signingSecret"},
		"the removed extra section":   {charttest.External, `config={"extra":"auth: {}"}`, "config"},
		"env names as one string":     {charttest.External, `providerSecretEnv="VAST_API_KEY"`, "/providerSecretEnv"},
	} {
		if complaint := charttest.RenderError(t, refused.values, refused.set); !strings.Contains(complaint, refused.complaint) {
			t.Errorf("%s: helm said %q, want it to name %q", name, complaint, refused.complaint)
		}
	}
}

// Memgraph and GreptimeDB take no credentials, so who may connect is all that guards them.
func TestEachStoreAcceptsOnlyThePodsThatUseIt(t *testing.T) {
	type access struct {
		port    int32
		clients []string
	}
	const control, metrics = "hypothesisloop-control-service", "hypothesisloop-metrics-service"
	memgraph := access{7687, []string{control}}
	greptimedb := access{4000, []string{control, metrics}}
	for name, rendered := range map[string]struct {
		values string
		set    []string
		want   map[string]access
	}{
		"every store in the cluster": {charttest.InCluster, nil, map[string]access{
			"hypothesisloop-memgraph": memgraph, "hypothesisloop-greptimedb": greptimedb,
			"hypothesisloop-postgres": {5432, []string{control, metrics}},
		}},
		"the database elsewhere": {charttest.External, nil, map[string]access{
			"hypothesisloop-memgraph": memgraph, "hypothesisloop-greptimedb": greptimedb,
		}},
		"every store elsewhere": {charttest.External, []string{
			`memgraph.external={"enabled":true,"url":"bolt://memgraph.example.invalid:7687"}`,
			`greptimedb.external={"enabled":true,"url":"http://greptimedb.example.invalid:4000"}`,
		}, map[string]access{}},
	} {
		t.Run(name, func(t *testing.T) {
			release := charttest.Render(t, rendered.values, rendered.set...)
			if len(release.StatefulSets) == 0 && len(rendered.want) != 0 {
				t.Fatal("the chart rendered no store")
			}
			for store, statefulSet := range release.StatefulSets {
				if _, guarded := rendered.want[store]; !guarded && store != "hypothesisloop-object-store" {
					t.Errorf("store %s is rendered and this test does not say who reaches it", store)
				}
				if got := statefulSet.Spec.Template.Labels["app"]; got != store {
					t.Errorf("the pods of %s are labelled app=%s, which no policy selects", store, got)
				}
			}
			if len(release.NetworkPolicies) != len(rendered.want) {
				t.Errorf("policies = %q, want one for each of %d stores", release.Kinds["NetworkPolicy"], len(rendered.want))
			}
			for store, want := range rendered.want {
				policy, found := release.NetworkPolicies[store]
				if !found {
					t.Errorf("no policy for %s", store)
					continue
				}
				if _, runs := release.StatefulSets[store]; !runs {
					t.Errorf("a policy for %s, which the chart does not run", store)
				}
				spec := policy.Spec
				if selected := spec.PodSelector; len(selected.MatchLabels) != 1 || selected.MatchLabels["app"] != store || len(selected.MatchExpressions) != 0 {
					t.Errorf("%s: the policy selects %+v, want the store's pods", store, selected)
				}
				if len(spec.PolicyTypes) != 1 || spec.PolicyTypes[0] != "Ingress" || len(spec.Ingress) != 1 {
					t.Fatalf("%s: policy types %v with %d ingress rules, want one rule that is all the store accepts", store, spec.PolicyTypes, len(spec.Ingress))
				}
				rule := spec.Ingress[0]
				if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != want.port || *rule.Ports[0].Protocol != "TCP" || rule.Ports[0].EndPort != nil {
					t.Errorf("%s: ports = %+v, want TCP %d alone", store, rule.Ports, want.port)
				}
				var clients []string
				for _, peer := range rule.From {
					if peer.NamespaceSelector != nil || peer.IPBlock != nil || peer.PodSelector == nil || len(peer.PodSelector.MatchLabels) != 1 || len(peer.PodSelector.MatchExpressions) != 0 {
						t.Fatalf("%s: accepts %+v, want one named app of this namespace", store, peer)
					}
					client := peer.PodSelector.MatchLabels["app"]
					if got := release.Deployments[client].Spec.Template.Labels["app"]; got != client {
						t.Errorf("%s: accepts app=%s, which is the label of no Deployment's pods", store, client)
					}
					clients = append(clients, client)
				}
				if !slices.Equal(clients, want.clients) {
					t.Errorf("%s: accepts %q, want %q", store, clients, want.clients)
				}
			}
		})
	}
}
