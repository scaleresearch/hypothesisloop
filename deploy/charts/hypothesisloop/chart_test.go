package hypothesisloop

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

// The Secret each ci values file names.
var secretNames = map[string]string{InCluster: "hypothesisloop", External: "deployment-secrets"}

// The program each Deployment runs, which loads the settings.
var programs = map[string]config.Program{"hypothesisloop-control-service": config.ProgramControlService, "hypothesisloop-metrics-service": config.ProgramMetricsService}

// loadRendered runs the loader of deployment's program on the config the chart mounts and the
// environment it gives the container, and nothing else, and returns the settings it loaded.
func loadRendered(t *testing.T, release *Release, values, deployment string) *config.Settings {
	t.Helper()
	env := release.Env(t, deployment, secretNames[values])
	if env[config.SettingsPath.Name] != "/settings/hypothesisloop.yaml" {
		t.Fatalf("%s: %s = %q, want the mounted file", deployment, config.SettingsPath.Name, env[config.SettingsPath.Name])
	}
	Setenv(t, env)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if _, set := env[name]; !set && config.IsKey(name) {
			t.Setenv(name, "")
		}
	}
	t.Setenv(config.SettingsPath.Name, release.ConfigFile(t, "hypothesisloop-config", "hypothesisloop.yaml"))
	var settings *config.Settings
	var err error
	switch programs[deployment] {
	case config.ProgramControlService:
		var c *config.ControlService
		if c, err = config.LoadControlService(); err == nil {
			settings = c.Settings
		}
	case config.ProgramMetricsService:
		var m *config.MetricsService
		if m, err = config.LoadMetricsService(); err == nil {
			settings = m.Settings
		}
	}
	if err != nil {
		t.Fatalf("%s with %s values does not start: %v", deployment, values, err)
	}
	return settings
}

// Each process is handed exactly the keys its program reads under the settings it is given.
func TestEachServiceIsGivenExactlyTheKeysItReads(t *testing.T) {
	for _, values := range []string{InCluster, External} {
		release := Render(t, values)
		for deployment, program := range programs {
			t.Run(values+"/"+deployment, func(t *testing.T) {
				settings := loadRendered(t, release, values, deployment)
				want := []string{config.SettingsPath.Name}
				for _, key := range settings.Needs(program) {
					want = append(want, key.Name)
				}
				env := release.Env(t, deployment, secretNames[values])
				var got []string
				for name := range env {
					if name != "GOOGLE_APPLICATION_CREDENTIALS" {
						got = append(got, name)
					}
				}
				slices.Sort(want)
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("env = %q, want %q", got, want)
				}
				if env[config.DatabaseURL.Name] != Secret["DATABASE_URL"] || env[config.AuthSigningSecret.Name] != Secret["AUTH_SIGNING_SECRET"] {
					t.Error("the database address or the signing secret is not the Secret's")
				}
				if settings.ObjectStore.Bucket != "hypothesisloop-data" || settings.ObjectStore.MaxBytesPerAgent == 0 || len(settings.AcceleratorTypes) == 0 {
					t.Errorf("platform policy did not come through from the settings file: %+v", settings.ObjectStore)
				}
			})
		}
	}
}

func TestNoSecretIsCreatedAndNoSecretValueIsRendered(t *testing.T) {
	for _, values := range []string{InCluster, External} {
		release := Render(t, values)
		if names := release.Kinds["Secret"]; len(names) != 0 {
			t.Errorf("%s: the chart created Secrets %q; it reads one and creates none", values, names)
		}
		for key, value := range Secret {
			if strings.Contains(release.Text, value) {
				t.Errorf("%s: the value of Secret key %s is in the rendered manifests", values, key)
			}
		}
		for _, line := range strings.Split(release.ConfigMaps["hypothesisloop-config"].Data["hypothesisloop.yaml"], "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "${") {
				t.Errorf("%s: the rendered config names an environment variable: %s", values, line)
			}
		}
	}
}

func TestEveryPodReadsTheOneNamedSecret(t *testing.T) {
	release := Render(t, External)
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
func googleCredentialFiles(release *Release, secretName string) map[string]string {
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

// A deployment that builds environments names its registry's provider, the two service accounts
// and the environments repository in the config, and control-service reads its own Google identity, which mints their tokens, as a
// file of the Secret in a mounted directory. One that builds none reads no Google credential.
func TestTheContainerRegistryOfTheValuesReachesTheConfigWithTheGoogleIdentityThatMintsItsTokens(t *testing.T) {
	release := Render(t, External)
	env := release.Env(t, "hypothesisloop-control-service", secretNames[External])
	for name := range env {
		if strings.HasPrefix(name, "HYPOTHESISLOOP_ENVIRONMENT_BUILDER") {
			t.Errorf("control-service is handed %s", name)
		}
	}
	files := googleCredentialFiles(release, secretNames[External])
	if len(files) != 1 || files[env["GOOGLE_APPLICATION_CREDENTIALS"]] != "CONTAINER_REGISTRY_GCP_CREDENTIALS" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q among the mounted files %v, want the one file of Secret key CONTAINER_REGISTRY_GCP_CREDENTIALS", env["GOOGLE_APPLICATION_CREDENTIALS"], files)
	}
	if _, known := Secret["CONTAINER_REGISTRY_GCP_CREDENTIALS"]; !known {
		t.Error("Secret has no CONTAINER_REGISTRY_GCP_CREDENTIALS")
	}
	for deployment := range programs {
		got := loadRendered(t, release, External, deployment).ContainerRegistry
		want := config.GCPRegistryConfig{Pusher: "env-pusher@made-up-project.iam.gserviceaccount.com", Puller: "env-puller@made-up-project.iam.gserviceaccount.com"}
		if got == nil || got.Provider != config.RegistryProviderGCP || got.GCP == nil || *got.GCP != want ||
			got.EnvironmentsRepository != "europe-west1-docker.pkg.dev/made-up-project/hypothesisloop-environments/environment" {
			t.Errorf("%s: container_registry = %+v, want the values'", deployment, got)
		}
	}

	inCluster := Render(t, InCluster)
	if env := inCluster.Env(t, "hypothesisloop-control-service", secretNames[InCluster]); env["GOOGLE_APPLICATION_CREDENTIALS"] != "" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q on a deployment that builds no environments", env["GOOGLE_APPLICATION_CREDENTIALS"])
	}
	if files := googleCredentialFiles(inCluster, secretNames[InCluster]); len(files) != 0 {
		t.Errorf("mounted %v on a deployment that builds no environments", files)
	}
	if cfg := loadRendered(t, inCluster, InCluster, "hypothesisloop-control-service"); cfg.ContainerRegistry != nil {
		t.Errorf("container_registry = %+v, want none", cfg.ContainerRegistry)
	}
}

func TestTwoRendersOfTheSameValuesAreIdentical(t *testing.T) {
	for _, values := range []string{InCluster, External} {
		if first, second := Render(t, values).Text, Render(t, values).Text; first != second {
			t.Errorf("%s: two renders differ, so every upgrade would restart the pods", values)
		}
	}
}

// The staged file names no git host. The chart adds the one gitHost.provider names: the App's ids
// from the values, or the plain host of the values. Its secret is control-service's alone, from the
// Secret.
func TestTheGitHostOfTheValuesReachesTheConfig(t *testing.T) {
	inCluster := Render(t, InCluster)
	release := Render(t, External)
	for deployment := range programs {
		plain := loadRendered(t, inCluster, InCluster, deployment)
		if want := (config.PlainGitConfig{BaseURL: "https://git.example.invalid/experiments", Username: "made-up-git-account"}); plain.GitHost.Provider != config.GitHostProviderPlain || plain.GitHost.GitHub != nil || plain.GitHost.Plain == nil || *plain.GitHost.Plain != want {
			t.Errorf("%s: in-cluster git host is %s, want the values' plain host", deployment, plain.GitHost.Provider)
		}
		cfg := loadRendered(t, release, External, deployment)
		if got := cfg.GitHost; got.Provider != config.GitHostProviderGitHub || got.Plain != nil || got.GitHub == nil || *got.GitHub != (config.GitHubConfig{AppID: 4242, AppSlug: "made-up-app"}) {
			t.Errorf("%s: the git host is %q, or its App is not the values'", deployment, got.Provider)
		}
	}
	if got := release.Env(t, "hypothesisloop-control-service", secretNames[External])[config.GitHostGitHubAppPrivateKey.Name]; got != Secret["GIT_HOST_GITHUB_APP_PRIVATE_KEY"] {
		t.Errorf("control-service's App key = %q, want the Secret's", got)
	}
	if got := inCluster.Env(t, "hypothesisloop-control-service", secretNames[InCluster])[config.GitHostPlainPassword.Name]; got != Secret["GIT_HOST_PLAIN_PASSWORD"] {
		t.Errorf("control-service's git password = %q, want the Secret's", got)
	}
}

func TestValuesTheChartCannotUseAreRefusedWithTheirName(t *testing.T) {
	for name, refused := range map[string]struct{ values, set, complaint string }{
		"no secret name":              {InCluster, `secretName=""`, "secretName is required"},
		"no image registry":           {InCluster, `images.registry=""`, "images.registry is required"},
		"no UI origin":                {InCluster, `auth.uiOrigin=""`, "auth.uiOrigin is required"},
		"no agent image":              {InCluster, `agents.image=""`, "agents.image is required"},
		"no API address":              {InCluster, `publicAPIURL=""`, "publicAPIURL is required"},
		"an environment builder":      {InCluster, `environmentBuilder.address=tcp://builder.example.invalid:31234`, "environmentBuilder"},
		"a registry with no pusher":   {External, `containerRegistry.gcp.pusher=""`, "containerRegistry.gcp.pusher is required"},
		"a registry with no puller":   {External, `containerRegistry.gcp.puller=""`, "containerRegistry.gcp.puller is required"},
		"accounts of no provider":     {External, `containerRegistry.provider=""`, "without containerRegistry.provider"},
		"a registry with no repo":     {External, `containerRegistry.environmentsRepository=""`, "containerRegistry.environmentsRepository is required"},
		"the removed environments":    {External, `environments.repository=x`, "environments"},
		"no template repository":      {External, `gitHost.experimentTemplateRepository=""`, "gitHost.experimentTemplateRepository is required"},
		"a sandbox of no provider":    {External, `sandbox.provider=null`, "sandbox"},
		"the API address per reader":  {InCluster, `agents.apiURL=http://api.example.invalid`, "apiURL"},
		"no runtime metrics address":  {InCluster, `runtimes.metricsURL=""`, "runtimes.metricsURL is required"},
		"no object store address":     {InCluster, `objectStore.endpoint=""`, "objectStore.endpoint is required"},
		"a store elsewhere, unnamed":  {InCluster, `objectStore.enabled=false`, "objectStore.controlPlaneEndpoint is required"},
		"two addresses for one store": {InCluster, `objectStore.controlPlaneEndpoint="http://store.example.invalid:9000"`, "objectStore.controlPlaneEndpoint is set while"},
		"no sign-in service":          {InCluster, `ui.identity.provider=""`, "with ui.enabled it is supabase"},
		"no Supabase address":         {InCluster, `ui.identity.supabase.url=""`, "ui.identity.supabase.url is required"},
		"no publishable key":          {InCluster, `ui.identity.supabase.publishableKey=""`, "ui.identity.supabase.publishableKey is required"},
		"no sign-in providers":        {InCluster, `ui.identity.signInProviders=[]`, "ui.identity.signInProviders is required"},
		"a sign-in provider unknown":  {InCluster, `ui.identity.signInProviders=["gitlab"]`, "/ui/identity/signInProviders/0"},
		"an App with no slug":         {External, `gitHost.github.appSlug=""`, "gitHost.github.appSlug is required"},
		"an App with no id":           {External, `gitHost.github.appId=0`, "gitHost.github.appId is required"},
		"no git host":                 {External, `gitHost.provider=""`, "gitHost/provider"},
		"a git host nobody wrote":     {External, `gitHost.provider="gitlab"`, "gitHost/provider"},
		"plain with no root":          {InCluster, `gitHost.plain.baseURL=""`, "gitHost.plain.baseURL is required"},
		"providers as text":           {External, `auth.identityProviders="supabase"`, "/auth/identityProviders"},
		"a provider with no audience": {External, `auth.identityProviders=[{"issuer":"https://a.example","jwt_keys":[{"kid":"a","kty":"EC","alg":"ES256"}]}]`, "/auth/identityProviders/0"},
		"a provider with no keys":     {External, `auth.identityProviders=[{"issuer":"https://a.example","audience":"a","jwt_keys":[]}]`, "/auth/identityProviders/0/jwt_keys"},
		"a key field nobody reads":    {External, `auth.identityProviders=[{"issuer":"https://a.example","audience":"a","jwt_keys":[{"kid":"a","kty":"EC","alg":"ES256","use":"sig"}]}]`, "/auth/identityProviders/0/jwt_keys/0"},
		"the issuer outside the list": {External, `auth.issuer="https://a.example"`, "issuer"},
		"an origin that is a number":  {External, `auth.uiOrigin=3000`, "/auth/uiOrigin"},
		"a flag that is text":         {External, `postgres.enabled="no"`, "/postgres/enabled"},
		"a secret passed as a value":  {External, `auth.signingSecret="made-up"`, "signingSecret"},
		"the removed extra section":   {External, `config={"extra":"auth: {}"}`, "config"},
		"a git proxy of its own":      {External, `gitHost.proxyURL=https://api.example.invalid/git`, "proxyURL"},
	} {
		if complaint := RenderError(t, refused.values, refused.set); !strings.Contains(complaint, refused.complaint) {
			t.Errorf("%s: helm said %q, want it to name %q", name, complaint, refused.complaint)
		}
	}
}

// A release given no image tag runs the images the chart was published with, its appVersion,
// and hands the same tag to every cluster's runtime.
func TestTheImageTagDefaultsToTheChartsAppVersion(t *testing.T) {
	chart, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var appVersion string
	for _, line := range strings.Split(string(chart), "\n") {
		if value, ok := strings.CutPrefix(line, "appVersion: "); ok {
			appVersion = strings.Trim(value, `"`)
		}
	}
	release := Render(t, InCluster, `images.tag=""`)
	for name, deployment := range release.Deployments {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if strings.HasPrefix(container.Image, "registry.example.invalid/hypothesisloop-") && !strings.HasSuffix(container.Image, ":"+appVersion) {
				t.Errorf("%s runs %s, want the tag %s", name, container.Image, appVersion)
			}
		}
	}
	if got := loadRendered(t, release, InCluster, "hypothesisloop-control-service").Runtimes.ImageTag; got != appVersion {
		t.Errorf("runtimes image_tag = %q, want %q", got, appVersion)
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
		"every store in the cluster": {InCluster, nil, map[string]access{
			"hypothesisloop-memgraph": memgraph, "hypothesisloop-greptimedb": greptimedb,
			"hypothesisloop-postgres": {5432, []string{control, metrics}},
		}},
		"the database elsewhere": {External, nil, map[string]access{
			"hypothesisloop-memgraph": memgraph, "hypothesisloop-greptimedb": greptimedb,
		}},
		"every store elsewhere": {External, []string{
			`memgraph.external={"enabled":true,"url":"bolt://memgraph.example.invalid:7687"}`,
			`greptimedb.external={"enabled":true,"url":"http://greptimedb.example.invalid:4000"}`,
		}, map[string]access{}},
	} {
		t.Run(name, func(t *testing.T) {
			release := Render(t, rendered.values, rendered.set...)
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

// Every secret the chart hands a process beside the platform's own few is named by the one rule,
// <SECTION>_<PROVIDER>_<FIELD>, with the provider the rendered settings name for that section.
func TestEveryProviderSecretTheChartHandsOverIsNamedBySectionProviderAndField(t *testing.T) {
	own := []string{config.SettingsPath.Name, config.DatabaseURL.Name, config.MetricsDBURL.Name, config.GraphDBURL.Name, config.AuthSigningSecret.Name, "GOOGLE_APPLICATION_CREDENTIALS"}
	for _, values := range []string{InCluster, External} {
		release := Render(t, values)
		for deployment := range programs {
			settings := loadRendered(t, release, values, deployment)
			providers := map[string][]string{
				config.SectionObjectStore: {settings.ObjectStore.Provider},
				config.SectionGitHost:     {settings.GitHost.Provider},
				"MODEL":                   {settings.Models.Provider},
			}
			if r := settings.ContainerRegistry; r != nil {
				providers[config.SectionContainerRegistry] = []string{r.Provider}
			}
			if s := settings.Sandbox; s != nil {
				providers[config.SectionSandbox] = []string{s.Provider}
			}
			for _, cc := range settings.CloudCompute {
				providers[config.SectionCloudCompute] = append(providers[config.SectionCloudCompute], cc.Provider)
			}
			for name := range release.Env(t, deployment, secretNames[values]) {
				if slices.Contains(own, name) {
					continue
				}
				if !config.IsKey(name) {
					t.Errorf("%s/%s: %s is no key of the platform", values, deployment, name)
					continue
				}
				named := false
				for section, names := range providers {
					for _, provider := range names {
						field, ok := strings.CutPrefix(name, section+"_"+strings.ToUpper(provider)+"_")
						named = named || ok && field != "" && strings.Trim(field, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == ""
					}
				}
				if !named {
					t.Errorf("%s/%s: %s is not <SECTION>_<PROVIDER>_<FIELD> of a provider the settings name (%v)", values, deployment, name, providers)
				}
			}
		}
	}
}
