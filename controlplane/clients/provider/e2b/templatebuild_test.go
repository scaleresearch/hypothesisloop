package e2b

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

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
	templatesPath  = "/v2/templates"
	buildStart     = "/v2/templates/tpl1/builds/bld1"
	templateBuilds = "/templates/tpl1"
	buildStatus    = "/templates/tpl1/builds/bld2/status"
)

func templatesJSON(status string) string {
	return "[" + templateJSON("other", "notebook", "ready", "2026-10-01T10:00:00Z", "") + "," +
		templateJSON("tpl1", agentTemplate, status, "2026-10-05T14:07:23Z", "") + "]"
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
	api.on(http.MethodGet, sandboxesPath, answer(200, `[]`))
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
	api.on(http.MethodGet, templateBuilds, answer(200, `{"templateID":"tpl1","builds":[{"buildID":"bld2","status":"error"}]}`))
	api.on(http.MethodGet, buildStatus, answer(200, `{"status":"error","logEntries":[],"reason":{"message":"base64: not found"}}`))

	for range 2 {
		_, err := p.Create(context.Background(), agentSpec())
		if err == nil || errors.Is(err, errBuilding) || !strings.Contains(err.Error(), "failed: base64: not found") || !strings.Contains(err.Error(), "tpl1") {
			t.Fatalf("err = %v, want the failure with its reason and the template", err)
		}
	}
	if api.total() != 6 {
		t.Errorf("requests = %v, want the list, the latest build and its reason twice and nothing built", api.order())
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

// A template on a later page of the list is found, and no build is started for it.
func TestTheTemplateIsFoundOnALaterPage(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, templatesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("nextToken") == "" {
			w.Header().Set("X-Next-Token", "page2")
			_, _ = io.WriteString(w, "["+templateJSON("other", "notebook", "ready", "2026-10-01T10:00:00Z", "")+"]")
			return
		}
		_, _ = io.WriteString(w, templatesJSON("ready"))
	})
	if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "sb1" {
		t.Fatalf("Create: %q, %v", id, err)
	}
	if listed := api.sent(http.MethodGet, templatesPath); len(listed) != 2 || listed[0].Query != "limit=100" || listed[1].Query != "limit=100&nextToken=page2" {
		t.Errorf("template list requests = %+v, want both pages", listed)
	}
}

// Starting a build deletes the platform's templates that no sandbox uses and that have been
// unused for templateIdle, and only those; one that cannot be deleted does not stop the pass.
func TestABuildDeletesThePlatformsIdleTemplates(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	old := time.Now().Add(-templateIdle - time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	idle := func(id string) string {
		return templateJSON(id, "hl-00000000000000000000-1c-1024m", "ready", old, old)
	}
	api.on(http.MethodGet, templatesPath, answer(200, "["+strings.Join([]string{
		idle("idle1"),
		idle("idle2"),
		templateJSON("failed", "hl-11111111111111111111-1c-1024m", "error", old, ""),
		idle("running"),
		idle("paused"),
		templateJSON("spawned", "hl-22222222222222222222-1c-1024m", "ready", old, recent),
		templateJSON("new", "hl-33333333333333333333-1c-1024m", "building", recent, ""),
		templateJSON("theirs", "notebook", "ready", old, old),
		templateJSON("theirs2", "hl-notebook", "ready", old, old),
	}, ",")+"]"))
	api.on(http.MethodGet, sandboxesPath, answer(200, `[{"sandboxID":"a","templateID":"running","alias":"x","state":"running"},`+
		`{"sandboxID":"b","templateID":"paused","state":"paused"}]`))
	api.on(http.MethodPost, "/v3/templates", answer(202, `{"templateID":"tpl1","buildID":"bld1"}`))
	api.on(http.MethodPost, buildStart, answer(202, ``))
	api.on(http.MethodDelete, "/templates/idle1", answer(500, `{"code":500,"message":"try later"}`))
	api.on(http.MethodDelete, "/templates/idle2", answer(204, ``))
	api.on(http.MethodDelete, "/templates/failed", answer(204, ``))

	if _, err := p.Create(context.Background(), agentSpec()); !errors.Is(err, errBuilding) {
		t.Fatalf("err = %v, want the build started", err)
	}
	var deleted []string
	for _, r := range api.requests {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.Path)
		}
	}
	if want := []string{"/templates/idle1", "/templates/idle2", "/templates/failed"}; !reflect.DeepEqual(deleted, want) {
		t.Errorf("deleted = %v, want %v", deleted, want)
	}
}

// A template is kept when the sandboxes cannot be listed, and the build is not failed for it.
func TestTemplatesAreKeptWhenTheSandboxesCannotBeListed(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	old := time.Now().Add(-templateIdle - time.Hour).UTC().Format(time.RFC3339)
	api.on(http.MethodGet, templatesPath, answer(200, "["+templateJSON("idle", "hl-00000000000000000000-1c-1024m", "ready", old, old)+"]"))
	api.on(http.MethodGet, sandboxesPath, answer(500, `{"code":500,"message":"down"}`))
	api.on(http.MethodPost, "/v3/templates", answer(202, `{"templateID":"tpl1","buildID":"bld1"}`))
	api.on(http.MethodPost, buildStart, answer(202, ``))

	if _, err := p.Create(context.Background(), agentSpec()); !errors.Is(err, errBuilding) {
		t.Fatalf("err = %v, want the build started", err)
	}
}
