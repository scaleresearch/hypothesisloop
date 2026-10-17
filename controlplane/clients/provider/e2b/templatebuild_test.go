package e2b

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestTheImagesConfigBecomesBuildSteps(t *testing.T) {
	var config ocispec.ImageConfig
	config.Env = []string{"PATH=/usr/bin", `QUOTED=it's "a"=b $HOME`, "EMPTY="}
	config.WorkingDir, config.User = "/workspace", "agent"
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
		config.Env = []string{"PATH=/usr/bin", entry}
		if _, err := buildSteps("image", config); err == nil || !strings.Contains(err.Error(), "not an environment variable name") {
			t.Errorf("%s: err = %v, want a refusal of the name", name, err)
		}
	}
	built, err = buildSteps("image", ocispec.ImageConfig{})
	if err != nil || len(built) != 1 {
		t.Errorf("an image with no config: %+v, %v; want the env file alone", built, err)
	}
}

const (
	templatesPath = "/templates"
	buildStart    = "/v2/templates/tpl1/builds/bld1"
	buildStatus   = "/templates/tpl1/builds/bld1/status"
)

func templatesJSON(status string) string {
	return `[{"templateID":"other","buildID":"x","buildStatus":"ready","aliases":["notebook"]},` +
		`{"templateID":"tpl1","buildID":"bld1","buildStatus":"` + status + `","aliases":["` + agentTemplate + `"]}]`
}

// The first Create of an image and size starts the build of its template and fails while it runs;
// the next one finds the build and starts no other; once it is ready a Create starts the sandbox.
func TestCreateBuildsAMissingTemplateOnceAndStartsTheSandboxOnceItIsReady(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, templatesPath, inSequence(answer(200, `[]`), answer(200, templatesJSON("building")), answer(200, templatesJSON("ready"))))
	api.on(http.MethodPost, "/v3/templates", answer(202, `{"templateID":"tpl1","buildID":"bld1"}`))
	api.on(http.MethodPost, buildStart, answer(202, ``))
	images := p.images.(*fakeImages)
	images.config = ocispec.ImageConfig{Env: []string{"PATH=/usr/bin"}, WorkingDir: "/workspace"}

	for i := range 2 {
		if _, err := p.Create(context.Background(), agentSpec()); !errors.Is(err, errBuilding) || !strings.Contains(err.Error(), agentTemplate) {
			t.Fatalf("Create %d: err = %v, want the template named as being built", i+1, err)
		}
	}
	created := api.sent(http.MethodPost, "/v3/templates")
	if len(created) != 1 || created[0].APIKey != testKey {
		t.Fatalf("templates created = %+v, want one, with the API key", created)
	}
	if body, want := created[0].json(t), map[string]any{"name": agentTemplate, "cpuCount": 2.0, "memoryMB": 4096.0}; !reflect.DeepEqual(body, want) {
		t.Errorf("template body = %v, want %v", body, want)
	}
	started := api.sent(http.MethodPost, buildStart)
	if len(started) != 1 {
		t.Fatalf("builds started = %d, want one", len(started))
	}
	body := started[0].json(t)
	if body["fromImage"] != agentSpec().Image {
		t.Errorf("the build is from %v, want the pinned image", body["fromImage"])
	}
	if steps := body["steps"].([]any); len(steps) != 2 || steps[1].(map[string]any)["type"] != "WORKDIR" {
		t.Errorf("steps = %v, want the env file and the working directory", body["steps"])
	}
	if images.configs != 1 || images.configArchitecture != "amd64" {
		t.Errorf("the image's config was read %d times for %q, want once for amd64", images.configs, images.configArchitecture)
	}
	if len(api.sent(http.MethodPost, sandboxesPath)) != 0 {
		t.Fatal("a sandbox was asked for before its template was ready")
	}

	if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "sb1" {
		t.Fatalf("Create once the template is ready: %q, %v", id, err)
	}
	if made := api.sent(http.MethodPost, sandboxesPath); len(made) != 1 || made[0].json(t)["templateID"] != agentTemplate {
		t.Errorf("creates = %+v, want one of the built template", made)
	}
	if len(api.sent(http.MethodPost, "/v3/templates")) != 1 {
		t.Error("a ready template was built again")
	}
}

// A tag is pinned to the digest its registry answers now, and the template is named and built
// from that digest.
func TestATaggedImageIsBuiltFromItsDigest(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	api.on(http.MethodGet, templatesPath, answer(200, `[]`))
	api.on(http.MethodPost, "/v3/templates", answer(202, `{"templateID":"tpl1","buildID":"bld1"}`))
	api.on(http.MethodPost, buildStart, answer(202, ``))
	images := p.images.(*fakeImages)
	images.pinned = map[string]string{"registry.example/agent:1": agentSpec().Image}
	spec := agentSpec()
	spec.Image = "registry.example/agent:1"

	if _, err := p.Create(context.Background(), spec); !errors.Is(err, errBuilding) {
		t.Fatalf("err = %v, want the build started", err)
	}
	if name := api.sent(http.MethodPost, "/v3/templates")[0].json(t)["name"]; name != agentTemplate {
		t.Errorf("template name = %v, want %s, the one of the digest", name, agentTemplate)
	}
	if from := api.sent(http.MethodPost, buildStart)[0].json(t)["fromImage"]; from != agentSpec().Image {
		t.Errorf("the build is from %v, want the pinned image", from)
	}

	images.err = errors.New("manifest unknown")
	if _, err := p.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "manifest unknown") || errors.Is(err, errBuilding) {
		t.Errorf("an image the registry does not answer for: err = %v", err)
	}
}

// A failed build is reported with the vendor's reason, on every Create, and not started again.
func TestAFailedBuildIsReportedWithItsReasonAndNotRepeated(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	api.on(http.MethodGet, templatesPath, answer(200, templatesJSON("error")))
	api.on(http.MethodGet, buildStatus, answer(200, `{"status":"error","logEntries":[],"reason":{"message":"base64: not found"}}`))

	for range 2 {
		_, err := p.Create(context.Background(), agentSpec())
		if err == nil || errors.Is(err, errBuilding) || !strings.Contains(err.Error(), "failed: base64: not found") || !strings.Contains(err.Error(), "tpl1") {
			t.Fatalf("err = %v, want the failure with its reason and the template", err)
		}
	}
	if api.total() != 4 {
		t.Errorf("requests = %v, want the list and the reason twice and nothing built", api.order())
	}
}

// A build that cannot start is reported as that, and so is an image whose config cannot be read.
func TestABuildThatCannotStartIsAnError(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	api.on(http.MethodGet, templatesPath, answer(200, `[]`))
	api.on(http.MethodPost, "/v3/templates", answer(400, `{"code":400,"message":"cpuCount too high"}`))

	if _, err := p.Create(context.Background(), agentSpec()); err == nil || errors.Is(err, errBuilding) || !strings.Contains(err.Error(), "cpuCount too high") {
		t.Fatalf("a refused template: err = %v", err)
	}
	p.images.(*fakeImages).configErr = errors.New("has no linux/amd64 image")
	if _, err := p.Create(context.Background(), agentSpec()); err == nil || !strings.Contains(err.Error(), "has no linux/amd64 image") {
		t.Fatalf("an image without amd64: err = %v", err)
	}
	if len(api.sent(http.MethodPost, "/v3/templates")) != 1 {
		t.Error("a template was created for an image whose config could not be read")
	}
}
