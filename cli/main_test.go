package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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
	t.Setenv("HL_TOKEN", "test-token")
}

// A coordinator looked for the baseline's harness on the default branch: the platform experiment
// names the branch and commit of its declared baseline job.
func TestGetNamesTheBranchThatHoldsTheBaseline(t *testing.T) {
	recordingServer(t, func(_ int, r recorded) any {
		if r.Path == "/experiments/job-b" {
			return map[string]any{"id": "job-b", "branch": "hl/pe-1/baseline", "code_ref": "https://git/repo@abc123"}
		}
		return map[string]any{"id": "pe-1", "status": "running", "baseline_job_id": "job-b"}
	})
	code, stdout, stderr := hl(t, "platform-experiments", "get", "--platform-experiment", "pe-1")
	var out map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &out) != nil {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if out["baseline_branch"] != "hl/pe-1/baseline" || out["baseline_code_ref"] != "https://git/repo@abc123" {
		t.Errorf("printed %s, want baseline_branch and baseline_code_ref of job-b", stdout)
	}
}

func TestEveryRequestCarriesTheTokenAndNoTokenIsAnError(t *testing.T) {
	var gotAuthorization string
	calls := 0
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "get", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuthorization != "Bearer test-token" {
		t.Errorf("Authorization: got = %q, want = %q", gotAuthorization, "Bearer test-token")
	}

	t.Setenv("HL_TOKEN", "")
	stderr.Reset()
	if code := run([]string{"platform-experiments", "get", "--platform-experiment", "pe-1"}, &stdout, &stderr); code == 0 {
		t.Fatal("exit code with no HL_TOKEN: got = 0, want a failure")
	}
	if !strings.Contains(stderr.String(), "HL_TOKEN") || calls != 1 {
		t.Errorf("with no HL_TOKEN: stderr = %q after %d calls, want it to name HL_TOKEN and send nothing", stderr.String(), calls)
	}
}

func TestPlatformExperimentsCreateNeedsAProject(t *testing.T) {
	var gotBody map[string]any
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"pe-1"}`))
	})
	setAPIURL(t, url)
	file := t.TempDir() + "/pe.yaml"
	if err := os.WriteFile(file, []byte("name: one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HL_PROJECT", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "create", file}, &stdout, &stderr); code != 2 || gotBody != nil {
		t.Fatalf("with no project: exit code = %d, sent = %v, want 2 and nothing sent", code, gotBody)
	}
	t.Setenv("HL_PROJECT", "prj-env")
	if code := run([]string{"platform-experiments", "create", file}, &stdout, &stderr); code != 0 || gotBody["project_id"] != "prj-env" {
		t.Fatalf("with $HL_PROJECT: exit code = %d, project_id = %v, stderr = %s", code, gotBody["project_id"], stderr.String())
	}
	if code := run([]string{"platform-experiments", "create", "--project", "prj-flag", file}, &stdout, &stderr); code != 0 || gotBody["project_id"] != "prj-flag" {
		t.Fatalf("with --project: exit code = %d, project_id = %v", code, gotBody["project_id"])
	}
}

// Every list of hl is one compact JSON object per line, this one included.
func TestPlatformExperimentsListIsOneCompactLineEach(t *testing.T) {
	list := []map[string]string{
		{"id": "pe-1", "status": "closed", "close_reason": "baseline_limit_reached", "description": "a long brief", "summary": "a long summary"},
		{"id": "pe-2", "status": "open"},
	}
	var gotQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})
	setAPIURL(t, url)

	for _, args := range [][]string{{"platform-experiments", "--limit", "5"}, {"platform-experiments", "list", "--limit", "5"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || gotQuery != "limit=5" {
			t.Fatalf("%v: exit %d, query %q, stderr %s", args, code, gotQuery, stderr.String())
		}
		want := `{"close_reason":"baseline_limit_reached","id":"pe-1","status":"closed"}` + "\n" + `{"id":"pe-2","status":"open"}` + "\n"
		if stdout.String() != want {
			t.Fatalf("%v: stdout %q, want %q", args, stdout.String(), want)
		}
	}
	list = []map[string]string{}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "list"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.String() != "hl: platform-experiments list: none\n" {
		t.Fatalf("empty: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// A file once held several platform experiments under experiments:, created one request after
// another, so a refusal of the second left the first created and the command failed. One file is
// one platform experiment, sent as written, and a list is refused before anything is created.
func TestPlatformExperimentsCreateSendsTheOneMappingOfItsFile(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "pe-1"} })
	file := writeFile(t, "name: first\nbudget_accelerator_hours: 10\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "create", "--project", "prj-1", file}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"id": "pe-1"`) {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %s", code, stdout.String(), stderr.String())
	}
	// A time the file leaves out is the platform's to default or refuse.
	want := []recorded{{Method: "POST", Path: "/platform-experiments", Body: map[string]any{"name": "first", "budget_accelerator_hours": float64(10), "project_id": "prj-1"}}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v, want %+v", *got, want)
	}
	for _, several := range []string{"project_id: prj-1\nexperiments:\n  - name: first\n  - name: second\n", "- name: first\n- name: second\n"} {
		stderr.Reset()
		if code := run([]string{"platform-experiments", "create", writeFile(t, several)}, &stdout, &stderr); code != 2 || len(*got) != 1 {
			t.Errorf("a file of several: exit %d with %d requests, want 2 and none more. stderr: %s", code, len(*got), stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "not valid YAML") && !strings.Contains(stderr.String(), "mapping") {
		t.Errorf("a bare list: stderr = %q, want it to ask for a mapping", stderr.String())
	}
}

func TestJobSubmitFromYAML(t *testing.T) {
	var gotBody map[string]any
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","agent_id":"jane","code_ref":"repo@abc","job":{"image":"foo:latest"},` +
			`"estimated_cost_acch":0.5,"capacity_tier":"guaranteed","status":"QUEUED","not_admitted_reason":"capacity_unavailable"}`))
	})
	setAPIURL(t, url)

	dir := t.TempDir()
	t.Chdir(dir)
	jobFile := dir + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", "--platform-experiment", "pe-env", jobFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing on a submit that went through", stderr.String())
	}
	if want := `{"id":"job-1","status":"QUEUED","code_ref":"repo@abc","capacity_tier":"guaranteed","estimated_cost_acch":0.5}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	metadata, ok := gotBody["metadata"].(map[string]any)
	if !ok || metadata["agent_id"] != "jane" || metadata["platform_experiment_id"] != "pe-env" {
		t.Fatalf("agent_id and platform_experiment_id not stamped onto metadata: %+v", gotBody)
	}
	// The platform issues the id, and hl prints the one in the answer.
	if id, sent := gotBody["id"]; sent {
		t.Fatalf("hl sent an id of its own: %q", id)
	}
}

// The refusal is the platform's whole word on a job sent before the start: hl asks nothing more
// and prints it as any other.
func TestJobSubmitBeforeTheStartPrintsTheRefusalAndAsksNothingMore(t *testing.T) {
	requests := 0
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"reason":"experiment_not_started","message":"platform experiment pe-1 has not started"}`, http.StatusUnprocessableEntity)
	})
	setAPIURL(t, url)
	jobFile := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 1 || requests != 1 || stdout.Len() != 0 || strings.Count(stderr.String(), "\n") != 1 ||
		!strings.Contains(stderr.String(), `"reason":"experiment_not_started"`) || !strings.Contains(stderr.String(), "pe-1 has not started") {
		t.Fatalf("exit %d after %d requests, stdout %q, stderr %q", code, requests, stdout.String(), stderr.String())
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

func TestIDIsPositionalOrFlag(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "x", "status": "QUEUED"} })
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"job", "status", "job-2"}, "/experiments/job-2"},
		{[]string{"job", "status", "--id", "job-2"}, "/experiments/job-2"},
		{[]string{"job", "cost", "job-2"}, "/experiments/job-2/cost"},
		{[]string{"job", "data", "--id", "job-2"}, "/experiments/job-2/data"},
		{[]string{"job", "cancel", "job-2"}, "/experiments/job-2/cancel"},
		{[]string{"hypothesis", "get", "--id", "h-1"}, "/hypotheses/h-1"},
		{[]string{"hypothesis", "status", "--id", "h-1", "--agent", "a", "--status", "refuted"}, "/hypotheses/h-1/status"},
		{[]string{"hypothesis", "comment", "--agent", "a", "--id", "h-1", "--text", "t"}, "/hypotheses/h-1/comments"},
		{[]string{"hypothesis", "link", "h-1", "--agent", "a", "--type", "related", "--to", "h-2"}, "/hypotheses/h-1/links"},
		{[]string{"platform-experiments", "get", "--platform-experiment", "pe-1"}, "/platform-experiments/pe-1"},
		{[]string{"platform-experiments", "close", "--platform-experiment", "pe-1"}, "/platform-experiments/pe-1/close"},
		{[]string{"results", "--platform-experiment", "pe-1"}, "/platform-experiments/pe-1/results"},
	} {
		*got = nil
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit code = %d, stderr = %s", tc.args, code, stderr.String())
		}
		if len(*got) != 1 || (*got)[0].Path != tc.path {
			t.Fatalf("%v: requests %+v, want one to %s", tc.args, *got, tc.path)
		}
	}
}

// Health prints each agent's line, the coordinator's among them, as the platform answers it.
func TestPlatformExperimentsHealthPrintsALinePerAgent(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"platform_experiment_id": "pe-1", "agents": []map[string]any{
			{"agent_id": "ag-1", "role": "competitor", "first_job_seconds": 42, "jobs_in_flight": 2, "restarts": 3, "coordinator_messages": 1},
			{"role": "coordinator", "jobs_in_flight": 0, "restarts": 1, "coordinator_messages": 0},
		}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "health", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if len(*got) != 1 || (*got)[0].Path != "/platform-experiments/pe-1/health" {
		t.Fatalf("requests %+v, want one to /platform-experiments/pe-1/health", *got)
	}
	want := `{"agent_id":"ag-1","coordinator_messages":1,"first_job_seconds":42,"jobs_in_flight":2,"restarts":3,"role":"competitor"}
{"coordinator_messages":0,"jobs_in_flight":0,"restarts":1,"role":"coordinator"}
`
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestMisuseIsAnsweredWithTheErrorAndTheSynopsis(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	for _, tc := range []struct {
		args  []string
		usage string
	}{
		{[]string{"job", "metrics", "--agent", "a", "job-1"}, "usage: hl job metrics ID"},
		{[]string{"job", "status"}, "usage: hl job status ID"},
		{[]string{"platform-experiments", "get", "--pe", "pe-1"}, "usage: hl platform-experiments get [--platform-experiment PE]"},
		{[]string{"platform-experiments", "get", "pe-1"}, "usage: hl platform-experiments get [--platform-experiment PE]"},
		{[]string{"platform-experiments", "propose", "--pe", "pe-1"}, "usage: hl platform-experiments propose [--platform-experiment PE]"},
		{[]string{"hypothesis", "get", "--bogus", "h-1"}, "usage: hl hypothesis get ID"},
		{[]string{"hypothesis", "status", "h-1", "h-2", "--status", "open"}, "usage: hl hypothesis status ID"},
		{[]string{"quota", "--bogus"}, "usage: hl quota"},
		{[]string{"job", "list", "--platform-experiment-id", "pe-1"}, "usage: hl job list [--agent A]"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.usage) {
			t.Fatalf("%v: exit %d, stderr %q, want 2 and %q", tc.args, code, stderr.String(), tc.usage)
		}
		if got := strings.Count(stderr.String(), "\n"); got != 2 || !strings.HasPrefix(stderr.String(), "hl: ") {
			t.Fatalf("%v: want one error line and the synopsis, got %q", tc.args, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "list", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "--status") {
		t.Fatalf("--help: exit %d, want the flag list, got %q", code, stderr.String())
	}
	if len(*got) != 0 {
		t.Fatalf("misuse sent requests: %+v", *got)
	}
}

func TestJobStatusPrintsOneCompactLinePerJob(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		id := strings.TrimPrefix(r.Path, "/experiments/")
		return map[string]any{"$schema": "s", "id": id, "status": "EVICTED",
			"eviction_reason": "silent", "not_admitted_reason": "", "phase_detail": map[string]any{},
			"hypothesis": "long text", "resolved_job": map[string]any{"image": "i"}, "job": map[string]any{"image": "i"}, "attempt_count": 0}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "status", "job-1", "job-2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := `{"attempt_count":0,"eviction_reason":"silent","id":"job-1","job":{"image":"i"},"status":"EVICTED"}` + "\n" +
		`{"attempt_count":0,"eviction_reason":"silent","id":"job-2","job":{"image":"i"},"status":"EVICTED"}` + "\n"
	if len(*got) != 2 || stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestCompactJSONKeepsKeyOrder(t *testing.T) {
	got, err := compactJSON([]byte(`{"z": 1, "a": {"y": "", "b": [], "c": null, "d": "<x>"}, "m": 0.10, "$schema": "s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":{"b":[],"d":"<x>"},"m":0.10}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestJobMetricsSummarizesEachMetric(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return []map[string]any{
			{"experiment_id": "j", "metric_name": "acc", "fraction_complete": 0.5, "metric_value": 0.8, "metric_basis": "raw"},
			{"experiment_id": "j", "metric_name": "loss", "fraction_complete": 0.5, "metric_value": 0.4, "metric_basis": "raw"},
			{"experiment_id": "j", "metric_name": "acc", "fraction_complete": 1, "metric_value": 0.7, "metric_basis": "raw"},
		}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "metrics", "j"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := `{"metric":"acc","basis":"raw","points":2,"last":0.7,"at":1,"min":0.7,"max":0.8}` + "\n" +
		`{"metric":"loss","basis":"raw","points":1,"last":0.4,"at":0.5,"min":0.4,"max":0.4}` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	if code := run([]string{"job", "metrics", "j", "--points"}, &stdout, &stderr); code != 0 || len(lines(stdout.String())) != 3 || strings.Contains(stdout.String(), "experiment_id") {
		t.Fatalf("--points: exit %d, stdout %q", code, stdout.String())
	}
}

// A refusal of the platform is one line of reason and message, for a cancel and a summary as
// for a submit.
func TestARefusedCancelOrSummaryPrintsItsReasonAndMessage(t *testing.T) {
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"reason": "invalid_transition", "message": "job job-1 is COMPLETED: only a QUEUED, SUBMITTED or RUNNING job is cancelled"}`, http.StatusConflict)
	}))
	for _, args := range [][]string{{"job", "cancel", "job-1"}, {"job", "summary", "job-1", "--text", "t"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || strings.Count(stderr.String(), "\n") != 1 ||
			!strings.HasSuffix(stderr.String(), `HTTP 409: {"reason":"invalid_transition","message":"job job-1 is COMPLETED: only a QUEUED, SUBMITTED or RUNNING job is cancelled"}`+"\n") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
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
	for _, args := range [][]string{
		{"job", "summary", "job-1", "--text", `LR 0.1 "won": +2%`},
		{"job", "summary", "--id", "job-1", "--text", `LR 0.1 "won": +2%`},
	} {
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit code = %d, stderr = %s", args, code, stderr.String())
		}
		if gotMethod != "POST" || gotPath != "/experiments/job-1/summary" || gotBody["summary"] != `LR 0.1 "won": +2%` {
			t.Fatalf("%v: got %s %s %+v", args, gotMethod, gotPath, gotBody)
		}
	}
}

func TestJobListSendsItsFilters(t *testing.T) {
	var gotQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "list", "--agent", "a", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "agent_id=a&platform_experiment_id=pe-1" {
		t.Fatalf("got query %s", gotQuery)
	}
}

func TestJobListPrintsWhyAJobWaitsAsItsCodeAndItsSentence(t *testing.T) {
	const message = "cluster main is short of 2 devices, held by 3 jobs of this platform experiment"
	recordingServer(t, func(int, recorded) any {
		return []map[string]any{{"id": "job-1", "status": "QUEUED", "not_admitted_reason": "capacity_unavailable", "not_admitted_message": message}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "list", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if want := `"not_admitted_reason":"capacity_unavailable","not_admitted_message":"` + message + `"`; !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout = %s, want %s in it", stdout.String(), want)
	}
}

func TestJobListWithoutAgentListsWholePlatformExperiment(t *testing.T) {
	var gotQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "list", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "platform_experiment_id=pe-1" {
		t.Fatalf("got query %s", gotQuery)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// AGENT_ID and PLATFORM_EXPERIMENT_ID once stood in for the flags. A token that names neither
// leaves both to the flags, whatever an environment still holds.
func TestATokenThatNamesNoAgentOrExperimentLeavesBothToTheFlags(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	t.Setenv("AGENT_ID", "agent-env")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(dir+"/job.yaml", []byte("metadata:\n  code_ref: repo@abc\n  hypothesis_id: hyp-1\njob:\n  image: foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"hypothesis", "submit", "--text", "t", "--builds-on", "root"},
		{"hypothesis", "list"},
		{"hypothesis", "query", "RETURN 1"},
		{"job", "submit", "job.yaml"},
		{"quota"},
		{"quota", "--agent", "jane"},
		{"results"},
		{"data-key"},
		{"platform-experiments", "get"},
		{"platform-experiments", "agents", "list"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(*got))
	}
}

// A watch is of what the token says unless a flag says otherwise. The job id a job's container
// holds was once read as --experiment, which turned an agent's own updates into one job's.
func TestWatchScopeDefaultsToTheTokenOnlyWithoutExperiment(t *testing.T) {
	var queries []string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		http.Error(w, `{"error":"refused"}`, http.StatusBadRequest)
	})
	setAPIURL(t, url)
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/agent-env"))
	t.Setenv("HL_STATE_DIR", t.TempDir())
	t.Setenv("HYPOTHESISLOOP_EXPERIMENT_ID", "job-stale")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"watch"}, "agent_id=agent-env&platform_experiment_id=pe-env"},
		{[]string{"watch", "--agent", "jane"}, "agent_id=jane&platform_experiment_id=pe-env"},
		{[]string{"watch", "--experiment", "job-1"}, "experiment_id=job-1"},
	} {
		queries = nil
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, stderr %s", tc.args, code, stderr.String())
		}
		if len(queries) != 1 || queries[0] != tc.want {
			t.Fatalf("%v: queries = %v, want %q", tc.args, queries, tc.want)
		}
	}
}

// hl once printed each caller's next step on stderr beside the record, and a reader that parsed
// both streams as one (2>&1) failed on it.
func TestPlatformExperimentsGetWritesTheRecordAloneWithItsReasonFirst(t *testing.T) {
	record := map[string]any{"id": "pe-1", "description": "a long brief", "name": "a run", "starts_at": "2000-01-01T00:00:00Z"}
	requests := 0
	recordingServer(t, func(int, recorded) any {
		requests++
		return record
	})
	for _, token := range []string{derivedToken("coordinator:pe-1"), derivedToken("agent:pe-1/ag-r"), "hlk_person"} {
		for _, status := range []string{"open", "running"} {
			t.Setenv("HL_TOKEN", token)
			record["status"] = status
			delete(record, "status_reason")
			if status == "open" {
				record["status_reason"] = map[string]any{"reason": "baseline_required", "message": "Add a baseline agent"}
			}
			requests = 0
			var out bytes.Buffer
			if code := run([]string{"platform-experiments", "get", "--platform-experiment", "pe-1"}, &out, &out); code != 0 || requests != 1 {
				t.Fatalf("%s %s: exit %d after %d requests, output %s", token, status, code, requests, out.String())
			}
			var parsed map[string]any
			if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || !reflect.DeepEqual(parsed, record) {
				t.Fatalf("%s %s: both streams together are not the record alone: %v, %q", token, status, err, out.String())
			}
		}
	}
}

// Quota is granted at the start. Before it the platform refuses with what the caller does until
// then, and hl quota prints the reason and that sentence, with no second request. It once exited
// 0 with nothing on stdout, which a script read as an agent with no quota line and no refusal,
// and then printed the sentence without the reason an agent's harness tells the refusal by.
func TestQuotaBeforeTheStartPrintsThePlatformsWordAndFails(t *testing.T) {
	reason, requests := "quota_not_granted", 0
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"reason":"`+reason+`","message":"agent jane holds no quota yet: prepare, then end your turn"}`, http.StatusNotFound)
	})
	setAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	code := run([]string{"quota", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 1 || requests != 1 || stdout.Len() != 0 || stderr.String() != "hl: quota: quota_not_granted: agent jane holds no quota yet: prepare, then end your turn\n" {
		t.Fatalf("open: exit %d after %d requests, stdout %q, stderr %q", code, requests, stdout.String(), stderr.String())
	}
	reason = "planner_has_no_quota"
	stderr.Reset()
	if code := run([]string{"quota", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "hl: quota: planner_has_no_quota: ") {
		t.Fatalf("planner: exit %d, stderr %q, want the reason and the platform's word", code, stderr.String())
	}
	reason = "not_found"
	stderr.Reset()
	if code := run([]string{"quota", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "HTTP 404") {
		t.Fatalf("running: exit %d, stderr %q, want the 404", code, stderr.String())
	}
}

func TestDataKeyPrintsTheJobVariablesAsExportLines(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri": "s3://hl-data/pe-1/bot-1/", "shared": "s3://hl-data/pe-1/",
			"endpoint": "http://store:9000", "region": "us-east-1",
			"access_key_id": "AK", "secret_access_key": "S'K",
		})
	})
	setAPIURL(t, url)
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-1/bot-1"))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"data-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != "POST" || gotPath != "/platform-experiments/pe-1/data-key" || gotBody["agent_id"] != "bot-1" {
		t.Fatalf("got %s %s %+v", gotMethod, gotPath, gotBody)
	}
	want := `export AWS_ACCESS_KEY_ID='AK'
export AWS_SECRET_ACCESS_KEY='S'\''K'
export AWS_ENDPOINT_URL='http://store:9000'
export AWS_ENDPOINT_URL_S3='http://store:9000'
export AWS_REGION='us-east-1'
export HYPOTHESISLOOP_DATA_URI='s3://hl-data/pe-1/bot-1/'
export HYPOTHESISLOOP_DATA_SHARED='s3://hl-data/pe-1/'
`
	if stdout.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
}

func TestDataKeyNeedsAnAgentAndAPlatformExperiment(t *testing.T) {
	t.Setenv("HL_TOKEN", "hlk_x")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"data-key"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: hl data-key") {
		t.Fatalf("exit %d, stderr %q, want 2 and the usage", code, stderr.String())
	}
}

func TestAWakeKindThePlatformDoesNotKnowIsRefusedBeforeAnyWatch(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return []any{map[string]any{"kind": "agent.finished"}} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-1/agent-a"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"watch", "--wake", "agent.finished,agent.finnished"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"agent.finnished" is no event kind`) || len(*got) != 1 || (*got)[0].Path != "/watch/kinds" {
		t.Fatalf("exit %d, stderr %q, requests %+v", code, stderr.String(), *got)
	}
}

// A coordinator was woken for a baseline job that completed, which it acts on only once the
// agent finishes: a kind wakes for the values its reader can act on.
func TestAWakeEntryNamesAKindOrAKindWithTheValueThatWakesOrTheOneThatDoesNot(t *testing.T) {
	for entry, wakes := range map[string]map[string]bool{
		"experiment.status":        {"FAILED": true, "COMPLETED": true},
		"experiment.status=FAILED": {"FAILED": true, "COMPLETED": false},
		"job.held!=stage_cut":      {"stage_cut": false, "capacity_unavailable": true},
	} {
		kind, value, woken := cutWake(entry)
		if wantKind, _, _ := strings.Cut(strings.ReplaceAll(entry, "!", ""), "="); kind != wantKind {
			t.Errorf("%s: kind = %q", entry, kind)
		}
		for of, want := range wakes {
			if woken(of == value) != want {
				t.Errorf("%s: an event of value %s wakes = %v, want %v", entry, of, !want, want)
			}
		}
	}
}

// A tunnel in front of the platform answered 530 with a whole HTML page, which hl printed into
// the agent's context on every call.
//
// Every such answer then read as an outage, the router's own 404 for a route this hl is too old
// or too new for among them. Only a gateway's 502, 503 and 504 and a request that got no answer
// say the platform could not be reached.
//
// That line is the one place an agent reads what an outage asks of it, and it says of the command
// only what is true: a read changed nothing, and a write may have been carried out.
func TestAnAnswerThatIsNotThePlatformsIsOneLineWithoutItsBody(t *testing.T) {
	const next = "A person runs it again later; an agent ends its turn, and an update arrives when the platform answers again"
	const unreachable = "the platform could not be reached, and this command changed nothing. " + next
	const inFront = "the answer is no JSON, so it is none of the platform's API: something in front of the platform answered in its place"
	const noRoute = "the platform serves no GET /experiments/job-1: this hl and the platform are of different versions"
	for status, tc := range map[int]struct{ body, want string }{
		530: {"<!DOCTYPE html>\n<html><head><title>Cloudflare Tunnel error</title></head>\n<body>" + strings.Repeat("<p>x</p>\n", 500) + "</body></html>", inFront},
		500: {"Internal Server Error", inFront},
		502: {"error code: 502", unreachable},
		503: {"", unreachable},
		504: {"<html>gateway timeout</html>", unreachable},
		404: {"404 page not found\n", noRoute},
		405: {"Method Not Allowed\n", noRoute},
	} {
		setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(tc.body))
		}))
		var stdout, stderr bytes.Buffer
		code := run([]string{"job", "status", "job-1"}, &stdout, &stderr)
		want := fmt.Sprintf("hl: GET /experiments/job-1 -> HTTP %d: %s\n", status, tc.want)
		if code != 1 || stdout.Len() != 0 || stderr.String() != want {
			t.Errorf("HTTP %d: exit %d, stdout %q, stderr %q, want %q", status, code, stdout.String(), stderr.String(), want)
		}
	}

	// A 503 of the platform's own is JSON and is printed as it is.
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"reason":"object_store_unavailable","message":"nothing was stored"}`, http.StatusServiceUnavailable)
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "status", "job-1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), `"reason":"object_store_unavailable"`) || strings.Contains(stderr.String(), "could not be reached") {
		t.Errorf("the platform's own 503: exit %d, stderr %q, want its reason and no word of an outage", code, stderr.String())
	}

	down := withServer(t, func(http.ResponseWriter, *http.Request) {})
	setAPIURL(t, strings.Replace(down, "127.0.0.1", "127.0.0.1.invalid", 1))
	stderr.Reset()
	if code := run([]string{"job", "status", "job-1"}, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "hl: GET /experiments/job-1: "+unreachable+" (") || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("a request that got no answer: exit %d, stderr %q, want one line saying the platform could not be reached and the cause", code, stderr.String())
	}

	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGatewayTimeout) }))
	stderr.Reset()
	want := "hl: POST /experiments/job-1/cancel -> HTTP 504: the platform could not be reached, and whether this command took effect is not known: read what it changes before running it again. " + next + "\n"
	if code := run([]string{"job", "cancel", "job-1"}, &stdout, &stderr); code != 1 || stderr.String() != want {
		t.Errorf("a write that got no answer: exit %d, stderr %q, want %q", code, stderr.String(), want)
	}
}

// A coordinator read a baseline job's hypothesis_id, then the platform experiment's id, and ran
// hl hypothesis get on it: 404. Such a job prints that it is on the objective, and no hypothesis.
func TestABaselineJobPrintsAsOnTheObjective(t *testing.T) {
	recordingServer(t, func(_ int, r recorded) any {
		job := map[string]any{"id": "job-base", "status": "COMPLETED", "platform_experiment_id": "pe-1", "objective": true}
		if r.Path == "/experiments/job-base" {
			return job
		}
		return []map[string]any{job, {"id": "job-own", "status": "COMPLETED", "hypothesis_id": "hyp-1"}}
	})
	for _, args := range [][]string{{"job", "list", "--platform-experiment", "pe-1"}, {"job", "status", "job-base"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("hl %v: exit %d, stderr %s", args, code, stderr.String())
		}
		baseline, _, _ := strings.Cut(stdout.String(), "\n")
		if !strings.Contains(baseline, `"id":"job-base"`) || !strings.Contains(baseline, `"objective":true`) || strings.Contains(baseline, "hypothesis_id") {
			t.Errorf("hl %v: baseline job = %s, want objective true and no hypothesis_id", args, baseline)
		}
	}
	var stdout, stderr bytes.Buffer
	run([]string{"job", "list", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if own := strings.Split(stdout.String(), "\n")[1]; !strings.Contains(own, `"hypothesis_id":"hyp-1"`) || strings.Contains(own, "objective") {
		t.Errorf("a researcher's job = %s, want its hypothesis_id and no objective", own)
	}
}
