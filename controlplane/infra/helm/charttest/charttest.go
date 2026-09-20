// Package charttest renders the control-plane chart for tests, with no cluster and no secret:
// what each process would be started with, so its own loader can be run on it.
package charttest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// The two values files under the chart's ci directory.
const (
	InCluster = "in-cluster"
	External  = "external"
)

// Secret is every key the chart can read from the deployment's Secret, with made-up values. A
// test's processes get these through the environment and through nothing else.
var Secret = map[string]string{
	"DATABASE_URL":                            "postgres://made-up-user:made-up-database-password@db.example.invalid:5432/postgres?sslmode=require",
	"POSTGRES_PASSWORD":                       "made-up-database-password",
	"HYPOTHESISLOOP_SIGNING_SECRET":           "made-up-signing-secret-of-forty-characters",
	"OBJECT_STORE_ACCESS_KEY_ID":              "made-up-object-store-key-id",
	"OBJECT_STORE_SECRET_ACCESS_KEY":          "made-up-object-store-secret-key",
	"HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET": "made-up-app-client-secret",
	"HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY":   "-----BEGIN RSA PRIVATE KEY-----\nmade-up: key # one string\n-----END RSA PRIVATE KEY-----\n",
	"VAST_API_KEY":                            "made-up-vast-api-key",
	"FLY_API_TOKEN":                           "FlyV1 made-up-fly-token",
	"E2B_API_KEY":                             "made-up-e2b-api-key",
	"CLAUDE_CODE_OAUTH_TOKEN":                 "made-up-claude-subscription-token",
}

// Release is one rendering of the chart.
type Release struct {
	// Text is helm's whole output, and Kinds the names it holds of each kind.
	Text        string
	Kinds       map[string][]string
	ConfigMaps  map[string]corev1.ConfigMap
	Deployments map[string]appsv1.Deployment
	Services    map[string]corev1.Service
}

func chartDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "hypothesisloop")
}

func settingsFile() string {
	return filepath.Join(chartDir(), "..", "..", "..", "settings", "hypothesisloop.yaml")
}

// stage copies the chart beside the settings file as it is now, which is what `make
// helm-prepare` does in place. A test never reads a copy staged by an earlier run.
func stage(t testing.TB) string {
	t.Helper()
	staged := filepath.Join(t.TempDir(), "hypothesisloop")
	if err := os.CopyFS(staged, os.DirFS(chartDir())); err != nil {
		t.Fatalf("copy the chart: %v", err)
	}
	settings, err := os.ReadFile(settingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staged, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "files", "hypothesisloop.yaml"), settings, 0o644); err != nil {
		t.Fatal(err)
	}
	return staged
}

func template(t testing.TB, values string, set []string) (stdout, stderr string, err error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm is not installed: the chart's tests render it with `helm template`")
	}
	staged := stage(t)
	args := []string{"template", "hypothesisloop", staged, "--values", filepath.Join(staged, "ci", values+"-values.yaml")}
	for _, assignment := range set {
		args = append(args, "--set-json", assignment)
	}
	var out, errOut bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// Render renders the chart with one of the ci values files. Each set is a `path=<json>` that
// overrides one value.
func Render(t testing.TB, values string, set ...string) *Release {
	t.Helper()
	stdout, stderr, err := template(t, values, set)
	if err != nil {
		t.Fatalf("helm template with %s values: %v\n%s", values, err, stderr)
	}
	release := &Release{
		Text:        stdout,
		Kinds:       map[string][]string{},
		ConfigMaps:  map[string]corev1.ConfigMap{},
		Deployments: map[string]appsv1.Deployment{},
		Services:    map[string]corev1.Service{},
	}
	documents := yaml.NewYAMLReader(bufio.NewReader(strings.NewReader(stdout)))
	for {
		document, err := documents.Read()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("helm's output is not YAML: %v", err)
		}
		var object struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		decode(t, document, &object)
		if object.Kind == "" {
			continue
		}
		release.Kinds[object.Kind] = append(release.Kinds[object.Kind], object.Metadata.Name)
		switch object.Kind {
		case "ConfigMap":
			var configMap corev1.ConfigMap
			decode(t, document, &configMap)
			release.ConfigMaps[configMap.Name] = configMap
		case "Deployment":
			var deployment appsv1.Deployment
			decode(t, document, &deployment)
			release.Deployments[deployment.Name] = deployment
		case "Service":
			var service corev1.Service
			decode(t, document, &service)
			release.Services[service.Name] = service
		}
	}
	return release
}

// RenderError renders values the chart must refuse and returns helm's complaint.
func RenderError(t testing.TB, values string, set ...string) string {
	t.Helper()
	stdout, stderr, err := template(t, values, set)
	if err == nil {
		t.Fatalf("helm template with %s values and %q rendered:\n%s", values, set, stdout)
	}
	return stderr
}

// ConfigFile writes one file of a ConfigMap where the process would find it mounted.
func (r *Release) ConfigFile(t testing.TB, configMap, file string) string {
	t.Helper()
	content, ok := r.ConfigMaps[configMap].Data[file]
	if !ok {
		t.Fatalf("the chart rendered no %s in ConfigMap %s", file, configMap)
	}
	path := filepath.Join(t.TempDir(), file)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Env is the environment the chart starts a Deployment's only container with: each literal
// value, and each key it reads from the Secret named secretName, taken from Secret.
func (r *Release) Env(t testing.TB, deployment, secretName string) map[string]string {
	t.Helper()
	containers := r.Deployments[deployment].Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("Deployment %s has %d containers, want one", deployment, len(containers))
	}
	return env(t, deployment, containers[0], secretName)
}

// TokenInit is the init container that leaves a runtime process its token: the script it runs
// and the environment it runs it with.
func (r *Release) TokenInit(t testing.TB, deployment, secretName string) (script string, environment map[string]string) {
	t.Helper()
	pod := r.Deployments[deployment].Spec.Template.Spec
	if len(pod.InitContainers) != 1 || len(pod.InitContainers[0].Command) != 3 {
		t.Fatalf("Deployment %s: init containers = %+v, want the one that runs hl-admin through a shell", deployment, pod.InitContainers)
	}
	return pod.InitContainers[0].Command[2], env(t, deployment, pod.InitContainers[0], secretName)
}

func env(t testing.TB, deployment string, container corev1.Container, secretName string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, entry := range container.Env {
		if entry.ValueFrom == nil {
			env[entry.Name] = entry.Value
			continue
		}
		ref := entry.ValueFrom.SecretKeyRef
		if ref == nil || ref.Name != secretName {
			t.Fatalf("Deployment %s takes %s from %+v, want a key of Secret %s", deployment, entry.Name, entry.ValueFrom, secretName)
		}
		value, known := Secret[ref.Key]
		if !known {
			t.Fatalf("Deployment %s reads Secret key %s, which charttest.Secret does not have", deployment, ref.Key)
		}
		env[entry.Name] = value
	}
	return env
}

// Setenv makes env the test's environment as far as secrets go: what env holds is set, and
// every other name a Secret key could arrive under is removed, so a value the developer's shell
// exports cannot stand in for one the chart forgot.
func Setenv(t *testing.T, env map[string]string) {
	t.Helper()
	names := []string{"RUNTIME_TOKEN", "LAUNCHER_TOKEN"}
	for key := range Secret {
		names = append(names, key)
	}
	for _, name := range names {
		if _, set := env[name]; set {
			continue
		}
		if previous, was := os.LookupEnv(name); was {
			os.Unsetenv(name)
			t.Cleanup(func() { os.Setenv(name, previous) })
		}
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func decode(t testing.TB, document []byte, into any) {
	t.Helper()
	encoded, err := yaml.ToJSON(document)
	if err != nil {
		t.Fatalf("helm's output is not YAML: %v", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		t.Fatalf("helm's output: %v", err)
	}
}
