package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
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
		"project_id: prj-1\n" +
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
	// The file is sent as written: a time it leaves out is the platform's to default or refuse.
	for i, body := range submittedBodies {
		if _, sent := body["starts_at"]; sent {
			t.Fatalf("item %d carries a time the file did not state: %+v", i, body)
		}
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
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "arrives as an update with its metrics when you end your turn") {
		t.Fatalf("stderr = %q, want the note that the result arrives as an update", stderr.String())
	}
	if want := `{"id":"job-1","status":"QUEUED","code_ref":"repo@abc","capacity_tier":"guaranteed","estimated_cost_acch":0.5}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	metadata, ok := gotBody["metadata"].(map[string]any)
	if !ok || metadata["agent_id"] != "jane" || metadata["platform_experiment_id"] != "pe-env" {
		t.Fatalf("agent_id and platform_experiment_id not stamped onto metadata: %+v", gotBody)
	}
	if id, _ := gotBody["id"].(string); !regexp.MustCompile(`^job-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("empty id not generated: got %q", gotBody["id"])
	}
}

func TestJobSubmitHeldForSummariesNamesEachCommand(t *testing.T) {
	var listQuery string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			listQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`[{"id":"job-1"},{"id":"job-2"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"job-3","status":"QUEUED","not_admitted_reason":"summary_gate"}`))
	})
	setAPIURL(t, url)
	jobFile := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 0 || listQuery != "agent_id=jane&needs_summary=true&platform_experiment_id=pe-1" {
		t.Fatalf("exit %d, list query %q, stderr %s", code, listQuery, stderr.String())
	}
	if want := `{"id":"job-3","status":"QUEUED"}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	got := stderr.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "hl job summary job-1 --text T; hl job summary job-2 --text T\n") {
		t.Fatalf("stderr = %q, want one line ending in the summary command of each job", got)
	}
}

func TestJobSummaryPointsAtTheVerdictThatIsTheAgentsToSet(t *testing.T) {
	var reply map[string]any
	recordingServer(t, func(int, recorded) any { return reply })
	for verdict, want := range map[string]string{
		"open":      "hl: job summary: the verdict of hyp-1 is open and yours to set: hl hypothesis status hyp-1 inconclusive, or confirmed|refuted --against CONTROL_JOB once a second run agrees\n",
		"confirmed": "hl: job summary: the verdict of hyp-1 is confirmed and yours to change if this finding does not fit it\n",
		"":          "",
	} {
		reply = map[string]any{"status": "ok"}
		if verdict != "" {
			reply["hypothesis_id"], reply["verdict"] = "hyp-1", verdict
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"job", "summary", "job-1", "--text", "it held"}, &stdout, &stderr)
		if code != 0 || stdout.String() != `{"status":"ok"}`+"\n" || stderr.String() != want {
			t.Fatalf("verdict %q: exit %d, stdout %q, stderr %q", verdict, code, stdout.String(), stderr.String())
		}
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

func TestJobSubmitHeldForSummariesStaysOneAcceptedLineWhenTheLookupFails(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			http.Error(w, `{"detail":"failed to list experiments"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":"job-3","status":"QUEUED","not_admitted_reason":"summary_gate"}`))
	})
	setAPIURL(t, url)
	jobFile := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr)
	want := "hl: job submit: accepted; it starts once each of your COMPLETED jobs has a summary: hl job list --needs-summary --agent jane lists them\n"
	if code != 0 || stderr.String() != want {
		t.Fatalf("exit %d, stderr = %q, want exit 0 and %q", code, stderr.String(), want)
	}
}

func TestJobSubmitKeepsExplicitID(t *testing.T) {
	var gotBody map[string]any
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "mine"})
	})
	setAPIURL(t, url)
	dir := t.TempDir()
	t.Chdir(dir)
	jobFile := dir + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("id: mine\nmetadata:\n  code_ref: repo@abc\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "submit", "--agent", "jane", jobFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotBody["id"] != "mine" {
		t.Fatalf("explicit id replaced: got %q", gotBody["id"])
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
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
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
		{[]string{"platform-experiments", "get"}, "/platform-experiments/pe-env"},
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
	if code := run([]string{"job", "list", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "--needs-summary\n") {
		t.Fatalf("--help: exit %d, want the flag list, got %q", code, stderr.String())
	}
	if len(*got) != 0 {
		t.Fatalf("misuse sent requests: %+v", *got)
	}
}

// The briefing of an agent carries the group listings, so each names the flags of its commands,
// taken from the command's own --help.
func TestAGroupListsEachCommandWithTheSynopsisOfItsOwnHelp(t *testing.T) {
	for group, subs := range map[string][]subcommand{"job": jobCommands, "hypothesis": hypothesisCommands} {
		var listing, none bytes.Buffer
		if code := run([]string{group, "--help"}, &listing, &none); code != 0 || none.Len() != 0 {
			t.Fatalf("hl %s --help: exit %d, stderr %q", group, code, none.String())
		}
		for _, sub := range subs {
			var help bytes.Buffer
			if code := run([]string{group, sub.name, "--help"}, &none, &help); code != 0 {
				t.Fatalf("hl %s %s --help: exit %d", group, sub.name, code)
			}
			usage, _, _ := strings.Cut(help.String(), "\n")
			synopsis, ok := strings.CutPrefix(usage, "usage: hl "+group+" ")
			if !ok || !strings.Contains(listing.String(), "\n  "+synopsis) {
				t.Errorf("hl %s --help does not list %q:\n%s", group, usage, listing.String())
			}
		}
	}
	var listing, none bytes.Buffer
	run([]string{"job", "--help"}, &listing, &none)
	for _, flag := range []string{"--hypothesis ID", "--env K=V", "--theory T", "--needs-summary"} {
		if !strings.Contains(listing.String(), flag) {
			t.Errorf("hl job --help does not name %s", flag)
		}
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

// A researcher fed hl job metrics --points to a parser of one JSON document: the help says what
// the output is, and the output is that.
func TestJobMetricsPointsAreJSONLinesAndTheHelpSaysSo(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return []map[string]any{
			{"experiment_id": "job-1", "metric_name": "loss", "metric_value": 0.5, "fraction_complete": 0.5},
			{"experiment_id": "job-1", "metric_name": "loss", "metric_value": 0.25, "fraction_complete": 1},
		}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "metrics", "job-1", "--points"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout %q, want one line per point", stdout.String())
	}
	for _, line := range lines {
		var point map[string]any
		if err := json.Unmarshal([]byte(line), &point); err != nil || point["metric_name"] != "loss" {
			t.Fatalf("line %q is not one point: %v", line, err)
		}
	}
	var help bytes.Buffer
	run([]string{"job", "metrics", "--help"}, &help, &help)
	if !strings.Contains(help.String(), "JSON lines, one object per line and no enclosing array") {
		t.Fatalf("help does not say what the output is:\n%s", help.String())
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
	if gotQuery != "agent_id=a&needs_summary=true&platform_experiment_id=pe-1" {
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

func TestHTTPErrorSurfacesPlatformMessage(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"$schema":"http://api/schemas/ErrorModel.json", "error":"already closed"}`))
	})
	setAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "close", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "hl: POST /platform-experiments/pe-1/close -> HTTP 409: {\"error\":\"already closed\"}\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// The platform starts a run itself once its baseline is declared, so hl has no start: the answer
// to it is the synopsis, which names the command that declares one.
func TestThereIsNoStartCommand(t *testing.T) {
	calls := recordingServer(t, func(int, recorded) any { return map[string]any{} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "start", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 2 || len(*calls) != 0 || !strings.Contains(stderr.String(), `unknown subcommand "start"`) ||
		!strings.Contains(stderr.String(), "--baseline-job") || strings.Contains(usage, "| start |") {
		t.Fatalf("exit %d, %d requests, stderr %q", code, len(*calls), stderr.String())
	}
}

func TestIdentityFlagsDefaultToEnv(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		if r.Method == "GET" {
			return []any{}
		}
		return map[string]any{"id": "x"}
	})
	t.Setenv("AGENT_ID", "agent-env")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	dir := t.TempDir()
	t.Chdir(dir)
	jobFile := dir + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\njob:\n  image: foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args      []string
		path      string
		agent, pe string
	}{
		{[]string{"hypothesis", "submit", "--text", "t", "--builds-on", "root"}, "/hypotheses", "agent-env", "pe-env"},
		{[]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "t", "--builds-on", "root"}, "/hypotheses", "jane", "pe-1"},
		{[]string{"hypothesis", "link", "--from", "h-2", "--type", "related", "--to", "h-1"}, "/hypotheses/h-2/links", "agent-env", ""},
		{[]string{"hypothesis", "comment", "--text", "t", "h-1"}, "/hypotheses/h-1/comments", "agent-env", ""},
		{[]string{"hypothesis", "status", "--status", "refuted", "h-1"}, "/hypotheses/h-1/status", "agent-env", ""},
	} {
		*got = nil
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %s", tc.args, code, stderr.String())
		}
		body := (*got)[0].Body
		if (*got)[0].Path != tc.path || body["agent_id"] != tc.agent || (tc.pe != "" && body["platform_experiment_id"] != tc.pe) {
			t.Fatalf("%v: request = %+v", tc.args, (*got)[0])
		}
	}

	*got = nil
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "submit", jobFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("job submit: exit %d, stderr %s", code, stderr.String())
	}
	if metadata, _ := (*got)[0].Body["metadata"].(map[string]any); metadata["agent_id"] != "agent-env" {
		t.Fatalf("job submit body = %+v", (*got)[0].Body)
	}

	*got = nil
	if code := run([]string{"hypothesis", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hypothesis list: exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Query != "platform_experiment_id=pe-env" {
		t.Fatalf("hypothesis list query = %q", (*got)[0].Query)
	}
}

func TestIdentityFlagsRequiredWithoutEnv(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	t.Setenv("AGENT_ID", "")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "")
	for _, args := range [][]string{
		{"hypothesis", "submit", "--text", "t", "--builds-on", "root"},
		{"hypothesis", "comment", "h-1"},
		{"hypothesis", "list"},
		{"job", "submit", "job.yaml"},
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

func TestWatchScopeDefaultsToEnvOnlyWithoutExperiment(t *testing.T) {
	var queries []string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		http.Error(w, `{"error":"refused"}`, http.StatusBadRequest)
	})
	setAPIURL(t, url)
	t.Setenv("HYPOTHESISLOOP_EXPERIMENT_ID", "")
	t.Setenv("AGENT_ID", "agent-env")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")

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

func TestJobStatusPrintsTheRecordAndNothingElse(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return map[string]any{"id": "job-1", "status": "QUEUED", "not_admitted_reason": "summary_gate"}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "status", "job-1"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q: want the one JSON line and no other output", code, stdout.String(), stderr.String())
	}
}

// hl once printed each caller's next step on stderr beside the record, and a reader that parsed
// both streams as one (2>&1) failed on it. The platform says the step in status_reason, and hl
// writes the record alone, with what it waits for ahead of its long description.
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
			lead := "{\n  \"id\": \"pe-1\",\n  \"status\": \"" + status + "\",\n  \"description\""
			if status == "open" {
				lead = "{\n  \"id\": \"pe-1\",\n  \"status\": \"open\",\n  \"status_reason\": {\n    \"message\": \"Add a baseline agent\",\n    \"reason\": \"baseline_required\"\n  },\n  \"description\""
			}
			if !strings.HasPrefix(out.String(), lead) {
				t.Errorf("%s %s: output begins %q, want %q", token, status, out.String(), lead)
			}
		}
	}
}

// Quota is granted at the start. Before it the platform refuses with what the caller does until
// then, and hl quota prints that with no error and no second request.
func TestQuotaBeforeTheStartPrintsThePlatformsWordAndNoError(t *testing.T) {
	reason, requests := "quota_not_granted", 0
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"reason":"`+reason+`","message":"agent jane holds no quota yet: prepare, then end your turn"}`, http.StatusNotFound)
	})
	setAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	code := run([]string{"quota", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 || requests != 1 || stdout.Len() != 0 || stderr.String() != "hl: quota: agent jane holds no quota yet: prepare, then end your turn\n" {
		t.Fatalf("open: exit %d after %d requests, stdout %q, stderr %q", code, requests, stdout.String(), stderr.String())
	}
	reason = "not_found"
	stderr.Reset()
	if code := run([]string{"quota", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "HTTP 404") {
		t.Fatalf("running: exit %d, stderr %q, want the 404", code, stderr.String())
	}
}

func TestGuessedFormsPointToTheCommand(t *testing.T) {
	recordingServer(t, func(_ int, r recorded) any {
		if strings.HasSuffix(r.Path, "/results") {
			return map[string]any{"metrics": []any{map[string]any{"metric": "val_accuracy"}}}
		}
		return []any{}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"hypothesis", "comments", "--help"}, 2, "hl hypothesis get ID"},
		{[]string{"hypothesis", "query", "RETURN 1", "--params", `{"pe":"pe-1"}`}, 2, "--param k=v"},
		{[]string{"hypothesis", "links", "--help"}, 2, "hl hypothesis link"},
		{[]string{"job", "summary", "job-1"}, 2, "hl job status job-1 prints it as summary"},
		{[]string{"results"}, 0, "no results yet on val_accuracy"},
		{[]string{"capacity", "--platform-experiment", "pe-1"}, 2, "hl: capacity: takes no --platform-experiment"},
		{[]string{"hypothesis", "get", "h-1", "--platform-experiment", "pe-1"}, 2, "hl: hypothesis get: takes no --platform-experiment"},
		{[]string{"job", "metrics", "job-1", "--points"}, 0, "no metric reported yet"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stderr.String(), tc.want) || strings.Count(stderr.String(), "\n") != 1 {
			t.Fatalf("%v: exit %d, stderr %q", tc.args, code, stderr.String())
		}
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
	t.Setenv("AGENT_ID", "bot-1")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")

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
	t.Setenv("AGENT_ID", "")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"data-key"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: hl data-key") {
		t.Fatalf("exit %d, stderr %q, want 2 and the usage", code, stderr.String())
	}
}

func TestAWakeKindThePlatformDoesNotKnowIsRefusedBeforeAnyWatch(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return []any{map[string]any{"kind": "agent.finished"}} })
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	t.Setenv("AGENT_ID", "agent-a")
	var stdout, stderr bytes.Buffer
	code := run([]string{"watch", "--wake", "agent.finished,agent.finnished"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"agent.finnished" is no event kind`) || len(*got) != 1 || (*got)[0].Path != "/watch/kinds" {
		t.Fatalf("exit %d, stderr %q, requests %+v", code, stderr.String(), *got)
	}
}

// A tunnel in front of the platform answered 530 with a whole HTML page, which hl printed into
// the agent's context on every call.
func TestAnAnswerThatIsNotThePlatformsIsOneLineWithoutItsBody(t *testing.T) {
	for status, body := range map[int]string{530: "<!DOCTYPE html>\n<html><head><title>Cloudflare Tunnel error</title></head>\n<body>" + strings.Repeat("<p>x</p>\n", 500) + "</body></html>", 502: "error code: 502"} {
		setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		var stdout, stderr bytes.Buffer
		code := run([]string{"job", "status", "job-1"}, &stdout, &stderr)
		want := fmt.Sprintf("hl: GET /experiments/job-1 -> HTTP %d: the platform could not be reached and something in front of it answered. Nothing in your request caused this: run the same command again later\n", status)
		if code != 1 || stdout.Len() != 0 || stderr.String() != want {
			t.Errorf("HTTP %d: exit %d, stdout %q, stderr %q, want %q", status, code, stdout.String(), stderr.String(), want)
		}
	}
}

// A planner had propose refuse its file, then tried to install a YAML parser to check the file
// before proposing again: the help says a refusal changes nothing and propose is the check.
func TestProposeSaysARefusedProposalChangesNothing(t *testing.T) {
	requests := 0
	setAPIURL(t, withServer(t, func(http.ResponseWriter, *http.Request) { requests++ }))
	file := t.TempDir() + "/proposal.yaml"
	if err := os.WriteFile(file, []byte("experiment:\n  name: a: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "propose", "--platform-experiment", "pe-1", file}, &stdout, &stderr); code != 2 || requests != 0 || !strings.Contains(stderr.String(), "line 2, column") {
		t.Fatalf("a file that is no YAML: exit %d after %d requests, stderr %s, want 2, none sent and the line", code, requests, stderr.String())
	}
	stderr.Reset()
	if run([]string{"platform-experiments", "propose", "-h"}, &stdout, &stderr); !strings.Contains(stderr.String(), "A\nrefused proposal changes nothing, so fix the file and run propose again.") {
		t.Fatalf("the help does not say a refused proposal changes nothing:\n%s", stderr.String())
	}
}
