package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func withServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func setAPIURL(t *testing.T, url string) {
	t.Helper()
	old := os.Getenv("API_URL")
	os.Setenv("API_URL", url)
	t.Cleanup(func() { os.Setenv("API_URL", old) })
}

func TestRegister(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": gotBody["id"], "kind": gotBody["kind"]})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"register", "--id", "jane", "--kind", "human"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != "POST" || gotPath != "/agents" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if gotBody["id"] != "jane" || gotBody["name"] != "jane" || gotBody["kind"] != "human" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
	if !strings.Contains(stdout.String(), `"jane"`) {
		t.Fatalf("stdout missing echoed id: %s", stdout.String())
	}
}

func TestRegisterMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"register"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestSignup(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "signed_up"})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"signup", "--platform-experiment", "pe-1", "--agent", "jane"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotPath != "/platform-experiments/pe-1/signup" {
		t.Fatalf("got path %s", gotPath)
	}
	if gotBody["agent_id"] != "jane" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
	if _, present := gotBody["quota_tier"]; present {
		t.Fatalf("quota_tier should be omitted when --quota-tier is not passed, got %+v", gotBody)
	}
}

func TestSignupWithQuotaTierOverride(t *testing.T) {
	var gotBody map[string]string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "signed_up"})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"signup", "--platform-experiment", "pe-1", "--agent", "bot-1", "--quota-tier", "guaranteed"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotBody["quota_tier"] != "guaranteed" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
}

func TestSignupRejectsUnknownQuotaTier(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"signup", "--platform-experiment", "pe-1", "--agent", "jane", "--quota-tier", "vip"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (rejected client-side, no request sent)", code)
	}
}

func TestPlatformExperimentsList(t *testing.T) {
	var gotQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "pe-1"}})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "--limit", "5"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "limit=5" {
		t.Fatalf("got query %q", gotQuery)
	}
}

func TestPlatformExperimentsCreateManyFromYAMLList(t *testing.T) {
	var submittedBodies []map[string]any
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		submittedBodies = append(submittedBodies, body)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("pe-%d", len(submittedBodies))})
	})
	setAPIURL(t, url)

	dir := t.TempDir()
	file := dir + "/experiments.yaml"
	yamlContent := "budget_accelerator_hours: 10\n" +
		"experiments:\n" +
		"  - name: first\n" +
		"  - name: second\n" +
		"    budget_accelerator_hours: 20\n"
	if err := os.WriteFile(file, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "create", file}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if len(submittedBodies) != 2 {
		t.Fatalf("expected 2 POSTs, got %d: %+v", len(submittedBodies), submittedBodies)
	}
	if submittedBodies[0]["name"] != "first" || submittedBodies[0]["budget_accelerator_hours"] != float64(10) {
		t.Fatalf("item 0 unexpected: %+v", submittedBodies[0])
	}
	if submittedBodies[1]["name"] != "second" || submittedBodies[1]["budget_accelerator_hours"] != float64(20) {
		t.Fatalf("item 1 (override) unexpected: %+v", submittedBodies[1])
	}
	for i, body := range submittedBodies {
		if body["starts_at"] == nil || body["ends_at"] == nil {
			t.Fatalf("item %d missing defaulted starts_at/ends_at: %+v", i, body)
		}
	}
}

func TestJobSubmitFromYAML(t *testing.T) {
	var gotBody map[string]any
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	})
	setAPIURL(t, url)

	dir := t.TempDir()
	jobFile := dir + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("job:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	metadata, ok := gotBody["metadata"].(map[string]any)
	if !ok || metadata["agent_id"] != "jane" {
		t.Fatalf("agent_id not stamped onto metadata: %+v", gotBody)
	}
}

func TestJobSubmitBadYAML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	jobFile := dir + "/bad.yaml"
	if err := os.WriteFile(jobFile, []byte("not: valid: yaml: at: all: -"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, stderr = %s", code, stderr.String())
	}
}

func TestJobGetters(t *testing.T) {
	var gotPath string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-2", "status": "QUEUED"})
	})
	setAPIURL(t, url)

	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"job", "status", "--id", "job-2"}, "/experiments/job-2"},
		{[]string{"job", "metrics", "--id", "job-2"}, "/experiments/job-2/metrics"},
		{[]string{"platform-experiments", "get", "--id", "pe-1"}, "/platform-experiments/pe-1"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit code = %d, stderr = %s", tc.args, code, stderr.String())
		}
		if gotPath != tc.path || !strings.Contains(stdout.String(), "QUEUED") {
			t.Fatalf("%v: got path %s, stdout %s", tc.args, gotPath, stdout.String())
		}
	}
}

func TestJobStatusNotFound(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "status", "--id", "missing"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestJobSummary(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "summary", "--id", "job-1", "--text", `LR 0.1 "won": +2%`}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != "POST" || gotPath != "/experiments/job-1/summary" || gotBody["summary"] != `LR 0.1 "won": +2%` {
		t.Fatalf("got %s %s %+v", gotMethod, gotPath, gotBody)
	}
}

func TestJobListNeedsSummary(t *testing.T) {
	var gotQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "list", "--agent", "a", "--platform-experiment", "pe-1", "--needs-summary"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "agent=a&platform_experiment_id=pe-1&needs_summary=true" {
		t.Fatalf("got query %s", gotQuery)
	}
}

func TestHTTPErrorSurfacesPlatformMessage(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"agent already exists"}`))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"register", "--id", "jane"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent already exists") {
		t.Fatalf("stderr did not surface platform error: %s", stderr.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
