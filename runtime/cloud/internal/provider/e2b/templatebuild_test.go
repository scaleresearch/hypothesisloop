package e2b

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAnImageReferenceIsTakenApartAsARuntimeDoes(t *testing.T) {
	for image, want := range map[string]imageReference{
		"python":                                 {"registry-1.docker.io", "library/python", "latest"},
		"python:3.13-slim":                       {"registry-1.docker.io", "library/python", "3.13-slim"},
		"astral/uv:0.5":                          {"registry-1.docker.io", "astral/uv", "0.5"},
		"ghcr.io/owner/image":                    {"ghcr.io", "owner/image", "latest"},
		"europe-docker.pkg.dev/p/r/job:abc":      {"europe-docker.pkg.dev", "p/r/job", "abc"},
		"localhost:5000/job:abc":                 {"localhost:5000", "job", "abc"},
		"registry.example/a/b@sha256:0123abcd":   {"registry.example", "a/b", "sha256:0123abcd"},
		"localhost/hypothesisloop-job":           {"localhost", "hypothesisloop-job", "latest"},
		"registry.example:5000/a/b:tag@sha256:f": {"registry.example:5000", "a/b:tag", "sha256:f"},
	} {
		if got := parseReference(image); got != want {
			t.Errorf("parseReference(%q) = %+v, want %+v", image, got, want)
		}
	}
}

func TestTheImagesConfigBecomesBuildSteps(t *testing.T) {
	var config imageConfig
	config.Config.Env = []string{"PATH=/usr/bin", `QUOTED=it's "a"=b $HOME`, "EMPTY="}
	config.Config.WorkingDir, config.Config.User = "/workspace", "agent"
	built, err := buildSteps("image", config)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 3 || built[1].Type != "WORKDIR" || built[1].Args[0] != "/workspace" || built[2].Type != "USER" || built[2].Args[0] != "agent" {
		t.Fatalf("steps = %+v, want the env file, then WORKDIR, then USER last", built)
	}
	command := built[0].Args[0]
	encoded := strings.TrimSuffix(strings.TrimPrefix(command, "echo "), " | base64 -d > "+imageEnvPath+" && chmod 644 "+imageEnvPath)
	file, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || built[0].Args[1] != "root" {
		t.Fatalf("the first step is %q as %q", command, built[0].Args[1])
	}
	if want := "export PATH='/usr/bin'\n" + `export QUOTED='it'\''s "a"=b $HOME'` + "\nexport EMPTY=''\n"; string(file) != want {
		t.Errorf("the env file is %q, want %q", file, want)
	}

	// The file the names go into is read by the shell of every sandbox of the template, as root's
	// file, before the command starts.
	for name, entry := range map[string]string{
		"a command":       "A;touch /tmp/owned;B=1",
		"a substitution":  "A$(id)=1",
		"a space":         "A B=1",
		"a newline":       "A\nid\nB=1",
		"no name":         "=1",
		"a leading digit": "1A=1",
	} {
		config.Config.Env = []string{"PATH=/usr/bin", entry}
		if _, err := buildSteps("image", config); err == nil || !strings.Contains(err.Error(), "not an environment variable name") {
			t.Errorf("%s: err = %v, want a refusal of the name", name, err)
		}
	}
	built, err = buildSteps("image", imageConfig{})
	if err != nil || len(built) != 1 {
		t.Errorf("an image with no config: %+v, %v; want the env file alone", built, err)
	}
}

// One server stands in for the registry, its token service and E2B.
func TestATemplateIsBuiltFromTheAmd64ImageWithTheSecretsInHeadersOnly(t *testing.T) {
	var calls []string
	var image string
	var started map[string]any
	polls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		registryCall := strings.HasPrefix(r.URL.Path, "/v2/team/")
		if registryCall && r.Header.Get("Authorization") != "Bearer pull-token" {
			t.Errorf("%s was asked without the pull token", r.URL.Path)
		}
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.Contains(r.URL.Path, "/builds/") {
			if r.Header.Get("X-API-Key") != "made-up-e2b-key" {
				t.Errorf("%s was asked without the API key", r.URL.Path)
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.example"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "GET /token":
			if r.URL.Query().Get("scope") != "repository:team/job:pull" || r.URL.Query().Get("service") != "registry.example" {
				t.Errorf("the token was asked for %s", r.URL.RawQuery)
			}
			if user, password, ok := r.BasicAuth(); !ok || user != "puller" || password != "made-up-registry-password" {
				t.Error("the token service was asked without the registry credentials")
			}
			io.WriteString(w, `{"token":"pull-token"}`)
		case "GET /v2/team/job/manifests/abc":
			io.WriteString(w, `{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`)
		case "GET /v2/team/job/manifests/sha256:amd":
			io.WriteString(w, `{"config":{"digest":"sha256:config"}}`)
		case "GET /v2/team/job/blobs/sha256:config":
			io.WriteString(w, `{"config":{"Env":["PATH=/usr/bin"],"WorkingDir":"/workspace"}}`)
		case "POST /v3/templates":
			var request map[string]any
			json.Unmarshal(body, &request)
			if request["cpuCount"] != float64(2) || request["memoryMB"] != float64(4096) || request["name"] != templateName(image, size{CPUs: 2, MemoryMB: 4096}) {
				t.Errorf("the template was created as %v", request)
			}
			io.WriteString(w, `{"templateID":"tpl","buildID":"bld"}`)
		case "POST /v2/templates/tpl/builds/bld":
			json.Unmarshal(body, &started)
			w.WriteHeader(http.StatusAccepted)
		case "GET /templates/tpl/builds/bld/status":
			polls++
			if polls == 1 {
				if r.URL.Query().Get("logsOffset") != "0" {
					t.Errorf("the first poll asked from %s", r.URL.Query().Get("logsOffset"))
				}
				io.WriteString(w, `{"status":"building","logEntries":[{"message":"pulling\r"}]}`)
				return
			}
			if r.URL.Query().Get("logsOffset") != "1" {
				t.Errorf("the second poll asked from %s, want the lines already printed skipped", r.URL.Query().Get("logsOffset"))
			}
			io.WriteString(w, `{"status":"ready","logEntries":[{"message":"done"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	b := TemplateBuilder{APIKey: "made-up-e2b-key", RegistryUsername: "puller", RegistryPassword: "made-up-registry-password", Out: &out,
		e2bURL: server.URL, registryScheme: "http", client: server.Client(), pollEvery: time.Millisecond}
	image = strings.TrimPrefix(server.URL, "http://") + "/team/job:abc"
	if err := b.Build(image, 2, 4096); err != nil {
		t.Fatalf("%v\ncalls: %v", err, calls)
	}
	if started["fromImage"] != image || !reflect.DeepEqual(started["fromImageRegistry"], map[string]any{"type": "registry", "username": "puller", "password": "made-up-registry-password"}) {
		t.Errorf("the build was started as %v", started)
	}
	if kinds := started["steps"].([]any); len(kinds) != 2 || kinds[1].(map[string]any)["type"] != "WORKDIR" {
		t.Errorf("steps = %v, want the env file and the working directory", started["steps"])
	}
	said := out.String()
	for _, want := range []string{"2 cores, 4096 MiB", "steps taken from the image: RUN, WORKDIR", "    pulling\n", "    done\n", "is ready (template tpl)"} {
		if !strings.Contains(said, want) {
			t.Errorf("the output has no %q:\n%s", want, said)
		}
	}
	for _, secret := range []string{"made-up-e2b-key", "made-up-registry-password", "pull-token"} {
		if strings.Contains(said, secret) || strings.Contains(strings.Join(calls, " "), secret) {
			t.Errorf("%s is in the output or in an address", secret)
		}
	}
}

func TestAFailedBuildAndAnImageWithoutAmd64AreErrors(t *testing.T) {
	answers := map[string]string{
		"GET /v2/job/manifests/abc":            `{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}}]}`,
		"GET /v2/job/manifests/ok":             `{"config":{"digest":"sha256:config"}}`,
		"GET /v2/job/blobs/sha256:config":      `{"config":{}}`,
		"POST /v3/templates":                   `{"templateID":"tpl","buildID":"bld"}`,
		"POST /v2/templates/tpl/builds/bld":    `{}`,
		"GET /templates/tpl/builds/bld/status": `{"status":"error","logEntries":[],"reason":{"message":"base64: not found"}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer, known := answers[r.Method+" "+r.URL.Path]; known {
			io.WriteString(w, answer)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	b := TemplateBuilder{APIKey: "made-up-e2b-key", Out: io.Discard, e2bURL: server.URL, registryScheme: "http", client: server.Client(), pollEvery: time.Millisecond}
	host := strings.TrimPrefix(server.URL, "http://")
	if err := b.Build(host+"/job:abc", 1, 512); err == nil || !strings.Contains(err.Error(), "no linux/amd64 image") {
		t.Errorf("an arm64-only image: err = %v", err)
	}
	if err := b.Build(host+"/job:ok", 1, 512); err == nil || !strings.Contains(err.Error(), "failed: base64: not found") {
		t.Errorf("a failed build: err = %v", err)
	}
	b.APIKey = ""
	if err := b.Build(host+"/job:ok", 1, 512); err == nil || !strings.Contains(err.Error(), "E2B_API_KEY") {
		t.Errorf("no API key: err = %v", err)
	}
}
