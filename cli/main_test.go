package main

import (
	"bytes"
	"encoding/json"
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

func TestHypothesisSubmitAndList(t *testing.T) {
	var submittedBody map[string]string
	var listQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/hypotheses":
			_ = json.NewDecoder(r.Body).Decode(&submittedBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "hyp-1"})
		case r.Method == "GET" && r.URL.Path == "/hypotheses":
			listQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "hyp-1", "text": submittedBody["text"]}})
		default:
			http.NotFound(w, r)
		}
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "H2 dissociates faster at high T"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("submit exit code = %d, stderr = %s", code, stderr.String())
	}
	if submittedBody["text"] != "H2 dissociates faster at high T" || submittedBody["agent_id"] != "jane" {
		t.Fatalf("unexpected submit body: %+v", submittedBody)
	}

	stdout.Reset()
	code = run([]string{"hypothesis", "list", "--platform-experiment", "pe-1", "--agent", "jane"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr = %s", code, stderr.String())
	}
	if listQuery != "platform_experiment_id=pe-1&agent=jane" {
		t.Fatalf("got query %q", listQuery)
	}
	if !strings.Contains(stdout.String(), "H2 dissociates faster at high T") {
		t.Fatalf("submitted text did not round-trip through list: %s", stdout.String())
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

func TestJobStatusFound(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "job-1", "status": "RUNNING"},
			{"id": "job-2", "status": "QUEUED"},
		})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "status", "--agent", "jane", "--id", "job-2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "QUEUED") {
		t.Fatalf("did not print matched job: %s", stdout.String())
	}
}

func TestJobStatusNotFound(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "job-1"}})
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "status", "--agent", "jane", "--id", "missing"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
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
