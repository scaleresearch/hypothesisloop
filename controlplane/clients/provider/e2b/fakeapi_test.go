package e2b

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/imagedigest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const (
	testKey   = "test-key"
	testToken = "sandbox-token"

	sandboxesPath = "/v2/sandboxes"
	sandboxPath   = "/sandboxes/sb1"

	// The fake serves the envd of every sandbox under one path of the API's own server.
	envdFiles  = "/envd/sb1/files"
	envdStart  = "/envd/sb1/process.Process/Start"
	envdSignal = "/envd/sb1/process.Process/SendSignal"
)

type recorded struct {
	Method      string
	Path        string
	Query       string
	APIKey      string
	AccessToken string
	ContentType string
	Raw         []byte
}

func (r recorded) json(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Raw, &body); err != nil {
		t.Fatalf("%s %s: body is not a JSON object: %s", r.Method, r.Path, r.Raw)
	}
	return body
}

// fakeAPI answers like the vendor's API and like the envd inside a sandbox. A test sets the
// answer of the routes it exercises and reads back what the adapter sent.
type fakeAPI struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []recorded
	routes   map[string]http.HandlerFunc
}

func newFakeAPI(t *testing.T) *fakeAPI {
	api := &fakeAPI{t: t, routes: map[string]http.HandlerFunc{}}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	seen := recorded{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		APIKey: r.Header.Get("X-API-Key"), AccessToken: r.Header.Get("X-Access-Token"),
		ContentType: r.Header.Get("Content-Type"), Raw: raw,
	}
	a.mu.Lock()
	a.requests = append(a.requests, seen)
	handler := a.routes[r.Method+" "+r.URL.Path]
	a.mu.Unlock()
	if handler == nil {
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusTeapot)
		return
	}
	handler(w, r)
}

func (a *fakeAPI) on(method, path string, handler http.HandlerFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.routes[method+" "+path] = handler
}

func (a *fakeAPI) sent(method, path string) []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []recorded
	for _, r := range a.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// order is every request so far, as "METHOD path", in the order it arrived.
func (a *fakeAPI) order() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.requests {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func (a *fakeAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// inSequence answers the nth request with the nth handler and keeps repeating the last.
func inSequence(handlers ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	next := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handler := handlers[next]
		if next < len(handlers)-1 {
			next++
		}
		mu.Unlock()
		handler(w, r)
	}
}

func sandboxes(list ...string) string {
	return "[" + strings.Join(list, ",") + "]"
}

// sandboxJSON is a sandbox as the list shows it, of the size the agent spec asks for.
func sandboxJSON(id, name, state string) string {
	metadata := map[string]string{}
	if name != "" {
		metadata[nameKey] = name
	}
	encoded, _ := json.Marshal(map[string]any{
		"sandboxID": id, "templateID": "tpl1", "state": state, "startedAt": "2026-10-05T11:00:00.5Z",
		"metadata": metadata, "cpuCount": 2, "memoryMB": 4096, "diskSizeMB": 11484,
	})
	return string(encoded)
}

const (
	detailJSON = `{"sandboxID":"sb1","state":"running","envdAccessToken":"` + testToken + `"}`
	goneJSON   = `{"code":404,"message":"Sandbox \"sb1\" doesn't exist or you don't have access to it"}`
	noFileJSON = `{"code":404,"message":"path '/var/tmp/.hl-supervisor/state' does not exist"}`
	// deadEnvd is what the vendor's proxy answers for the envd of a sandbox that has ended.
	deadEnvd = `{"sandboxId":"sb1","message":"The sandbox was not found","code":502}`
)

// stream answers a process start with the given messages, each in its own envelope, and an
// end-of-stream envelope holding end.
func stream(end string, messages ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", streamContentType)
		for _, message := range messages {
			_, _ = w.Write(framed(0, message))
		}
		_, _ = w.Write(framed(endOfStream, end))
	}
}

func framed(flags byte, message string) []byte {
	head := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[1:], uint32(len(message)))
	return append(head, message...)
}

// sentProcess decodes the process start the adapter sent.
func sentProcess(t *testing.T, r recorded) map[string]any {
	t.Helper()
	if r.ContentType != streamContentType || len(r.Raw) < 5 || r.Raw[0] != 0 || int(binary.BigEndian.Uint32(r.Raw[1:5])) != len(r.Raw)-5 {
		t.Fatalf("process start is not one envelope of %s: %q", streamContentType, r.Raw)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Raw[5:], &body); err != nil {
		t.Fatalf("process start is not JSON: %s", r.Raw[5:])
	}
	return body
}

const (
	startedEvent = `{"event":{"start":{"pid":971}}}`
	endedEvent   = `{"event":{"end":{"exited":true,"status":"exit status 0"}}}`
)

func newTestProvider(t *testing.T, api *fakeAPI) *Provider {
	t.Helper()
	p, err := New(Settings{LifetimeSeconds: 3600}, testKey, nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.baseURL = api.server.URL
	p.envdURL = func(sandboxID string) string { return api.server.URL + "/envd/" + sandboxID }
	p.poll = 5 * time.Millisecond
	p.window = 300 * time.Millisecond
	p.images = &fakeImages{}
	api.on(http.MethodGet, "/templates", answer(200, `[{"templateID":"tpl1","buildID":"bld1","buildStatus":"ready","aliases":["`+agentTemplate+`"]}]`))
	return p
}

// fakeImages answers for the registries: a tag in pinned is pinned to its value, and every
// image's config is config.
type fakeImages struct {
	pinned             map[string]string
	err                error
	config             ocispec.ImageConfig
	configErr          error
	configs            int
	configArchitecture string
}

func (f *fakeImages) Resolve(_ context.Context, ref string, _ *domain.RegistryLogin) (imagedigest.Image, error) {
	if f.err != nil {
		return imagedigest.Image{}, f.err
	}
	pinned, ok := f.pinned[ref]
	if !ok {
		return imagedigest.Image{}, fmt.Errorf("%s is not in the fake registry", ref)
	}
	return imagedigest.Image{Pinned: pinned, Architectures: []string{"amd64"}}, nil
}

func (f *fakeImages) Config(_ context.Context, _, architecture string, _ *domain.RegistryLogin) (ocispec.ImageConfig, error) {
	f.configs++
	f.configArchitecture = architecture
	return f.config, f.configErr
}
