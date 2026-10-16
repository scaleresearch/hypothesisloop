package fly

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "FlyV1 test-token"
	testApp   = "hl-agents"

	machinesPath = "/v1/apps/" + testApp + "/machines"
	logsPath     = "/api/v1/apps/" + testApp + "/logs"
)

type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
}

// fakeAPI answers like the vendor's Machines API and its log search. A test sets the answer of
// the routes it exercises and reads back what the adapter sent.
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

func (a *fakeAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

// apiNow is the time the fake API states in its Date header, which is the adapter's clock.
var apiNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", apiNow.Format(http.TimeFormat))
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

func machines(list ...string) string {
	return "[" + strings.Join(list, ",") + "]"
}

// machineJSON is a Machine as the list shows it, last updated the given time before apiNow.
func machineJSON(id, name, state string, updatedAgo time.Duration) string {
	metadata := map[string]string{"fly_platform_version": "v2"}
	if name != "" {
		metadata[nameKey] = name
	}
	encoded, _ := json.Marshal(map[string]any{
		"id":         id,
		"name":       "shy-morning-2432",
		"state":      state,
		"created_at": "2026-10-05T11:00:00Z",
		"updated_at": apiNow.Add(-updatedAgo).Format(time.RFC3339),
		"config":     map[string]any{"metadata": metadata},
	})
	return string(encoded)
}

// logJSON is a log search answer holding messages, oldest first.
func logJSON(messages ...string) string {
	entries := make([]map[string]any, len(messages))
	for i, message := range messages {
		entries[i] = map[string]any{"type": "logs", "attributes": map[string]any{"message": message, "instance": "9001"}}
	}
	encoded, _ := json.Marshal(map[string]any{"data": entries, "meta": map[string]string{"next_token": "1"}})
	return string(encoded)
}

func newTestProvider(t *testing.T, api *fakeAPI) *Provider {
	t.Helper()
	t.Setenv("FLY_TEST_TOKEN", testToken)
	p, err := New(Settings{APITokenEnv: "FLY_TEST_TOKEN", App: testApp, Region: "ams", CPUKind: "shared"}, nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.machinesURL, p.logsURL = api.server.URL, api.server.URL
	p.poll = 5 * time.Millisecond
	p.confirm = 200 * time.Millisecond
	return p
}
