package vast

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

const testKey = "test-key"

type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
}

// fakeAPI answers like the vendor's REST API and its log store. A test sets the answer of the
// routes it exercises and reads back what the adapter sent.
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

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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

func instancesPage(nextToken string, instances ...string) string {
	token := "null"
	if nextToken != "" {
		token = `"` + nextToken + `"`
	}
	return `{"success":true,"instances":[` + strings.Join(instances, ",") + `],"next_token":` + token + `}`
}

func instanceJSON(id int, label, status string) string {
	quote := func(s string) string {
		if s == "" {
			return "null"
		}
		encoded, _ := json.Marshal(s)
		return string(encoded)
	}
	return `{"id":` + jsonInt(id) + `,"label":` + quote(label) + `,"actual_status":` + quote(status) + `,"start_date":1791145633.5}`
}

func jsonInt(n int) string {
	encoded, _ := json.Marshal(n)
	return string(encoded)
}

// upload answers a log download as the store does: stamped with when the host delivered it.
func upload(deliveredAt time.Time, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", deliveredAt.UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, body)
	}
}

func newTestProvider(t *testing.T, api *fakeAPI, cfg Settings) *Provider {
	t.Helper()
	t.Setenv("VAST_TEST_KEY", testKey)
	cfg.APIKeyEnv = "VAST_TEST_KEY"
	if cfg.MaxPricePerHour == 0 {
		cfg.MaxPricePerHour = 0.5
	}
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.baseURL = api.server.URL
	p.poll = 5 * time.Millisecond
	return p
}
