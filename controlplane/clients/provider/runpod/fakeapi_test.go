package runpod

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const testKey = "test-key"

type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
}

// fakeAPI answers like the vendor's REST API v2. A test sets the answer of the routes it
// exercises and reads back what the adapter sent.
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
	seen := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &seen.Body); err != nil {
			a.t.Errorf("%s %s: body is not a JSON object: %s", r.Method, r.URL.Path, raw)
		}
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

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func problem(status int, detail string) http.HandlerFunc {
	encoded, _ := json.Marshal(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
	return reply(status, string(encoded))
}

func testSettings() Settings {
	return Settings{
		AccountID: "acct", MaxPricePerHour: 0.9, Cloud: "SECURE",
		Accelerators: []Accelerator{{Product: "NVIDIA RTX A4500", GPUID: "NVIDIA RTX A4500"}},
	}
}

func newTestProvider(t *testing.T, api *fakeAPI, cfg Settings) *Provider {
	t.Helper()
	p, err := New(cfg, testKey, nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.baseURL = api.server.URL
	p.poll = time.Millisecond
	p.logQuiet = 100 * time.Millisecond
	return p
}

// podJSON is a pod as the API shows one.
func podJSON(id, name, status string) string {
	encoded, _ := json.Marshal(map[string]any{
		"id": id, "name": name, "status": status, "dataCenterId": "EU-RO-1", "cost": 0.06,
		"createdAt": "2026-10-24T12:50:39.728Z", "startedAt": "2026-10-24T12:50:40Z",
		"cpu":              map[string]any{"id": "cpu3c", "vcpuCount": 2, "memory": 4},
		"globalNetworking": map[string]any{"enabled": false},
		"args":             `{"entrypoint":["/bin/sh","-c","x \"name\":\"decoy\""]}`,
	})
	return string(encoded)
}

func podsPage(pods ...string) string {
	body := `{"pagination":{"hasNextPage":false,"nextCursor":null},"pods":[`
	for n, p := range pods {
		if n > 0 {
			body += ","
		}
		body += p
	}
	return body + "]}"
}
