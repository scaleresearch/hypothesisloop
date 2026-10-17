package shared

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A coordinator looked for the baseline's harness on the default branch: the platform experiment
// names the branch and commit of its declared baseline job.
func TestGetNamesTheBranchThatHoldsTheBaseline(t *testing.T) {
	clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		if r.Path == "/experiments/job-b" {
			return map[string]any{"id": "job-b", "branch": "hl/pe-1/baseline", "code_ref": "https://git/repo@abc123"}
		}
		return map[string]any{"id": "pe-1", "status": "running", "baseline_job_id": "job-b"}
	})
	code, stdout, stderr := clitest.Output(t, PlatformExperimentsGet, "--platform-experiment", "pe-1")
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
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	})
	clitest.SetAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	if code := clitest.Run(PlatformExperimentsGet, []string{"--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuthorization != "Bearer test-token" {
		t.Errorf("Authorization: got = %q, want = %q", gotAuthorization, "Bearer test-token")
	}

	t.Setenv("HL_TOKEN", "")
	stderr.Reset()
	if code := clitest.Run(PlatformExperimentsGet, []string{"--platform-experiment", "pe-1"}, &stdout, &stderr); code == 0 {
		t.Fatal("exit code with no HL_TOKEN: got = 0, want a failure")
	}
	if !strings.Contains(stderr.String(), "HL_TOKEN") || calls != 1 {
		t.Errorf("with no HL_TOKEN: stderr = %q after %d calls, want it to name HL_TOKEN and send nothing", stderr.String(), calls)
	}
}

// Health prints each agent's line, the coordinator's among them, as the platform answers it.
func TestPlatformExperimentsHealthPrintsALinePerAgent(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"platform_experiment_id": "pe-1", "agents": []map[string]any{
			{"agent_id": "ag-1", "role": "competitor", "first_job_seconds": 42, "jobs_in_flight": 2, "restarts": 3, "coordinator_messages": 1},
			{"role": "coordinator", "jobs_in_flight": 0, "restarts": 1, "coordinator_messages": 0},
		}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(PlatformExperimentsHealth, []string{"--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
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

func TestJobStatusPrintsOneCompactLinePerJob(t *testing.T) {
	got := clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		id := strings.TrimPrefix(r.Path, "/experiments/")
		return map[string]any{"id": id, "status": "EVICTED",
			"eviction_reason": "silent", "not_admitted_reason": "", "phase_detail": map[string]any{},
			"hypothesis": "long text", "resolved_job": map[string]any{"image": "i"}, "job": map[string]any{"image": "i"}, "attempt_count": 0}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobStatus, []string{"job-1", "job-2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := `{"attempt_count":0,"eviction_reason":"silent","id":"job-1","job":{"image":"i"},"status":"EVICTED"}` + "\n" +
		`{"attempt_count":0,"eviction_reason":"silent","id":"job-2","job":{"image":"i"},"status":"EVICTED"}` + "\n"
	if len(*got) != 2 || stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestJobMetricsSummarizesEachMetric(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return []map[string]any{
			{"experiment_id": "j", "metric_name": "acc", "fraction_complete": 0.5, "metric_value": 0.8, "metric_basis": "raw"},
			{"experiment_id": "j", "metric_name": "loss", "fraction_complete": 0.5, "metric_value": 0.4, "metric_basis": "raw"},
			{"experiment_id": "j", "metric_name": "acc", "fraction_complete": 1, "metric_value": 0.7, "metric_basis": "raw"},
		}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobMetrics, []string{"j"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := `{"metric":"acc","basis":"raw","points":2,"last":0.7,"at":1,"min":0.7,"max":0.8}` + "\n" +
		`{"metric":"loss","basis":"raw","points":1,"last":0.4,"at":0.5,"min":0.4,"max":0.4}` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	if code := clitest.Run(JobMetrics, []string{"j", "--points"}, &stdout, &stderr); code != 0 || len(clitest.Lines(stdout.String())) != 3 || strings.Contains(stdout.String(), "experiment_id") {
		t.Fatalf("--points: exit %d, stdout %q", code, stdout.String())
	}
}

func TestJobStatusNotFound(t *testing.T) {
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
	})
	clitest.SetAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobStatus, []string{"--id", "missing"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestJobListSendsItsFilters(t *testing.T) {
	var gotQuery string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	clitest.SetAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := clitest.Run(JobList, []string{"--agent", "a", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "agent_id=a&platform_experiment_id=pe-1" {
		t.Fatalf("got query %s", gotQuery)
	}
}

func TestJobListPrintsWhyAJobWaitsAsItsCodeAndItsSentence(t *testing.T) {
	const message = "cluster main is short of 2 devices, held by 3 jobs of this platform experiment"
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return []map[string]any{{"id": "job-1", "status": "QUEUED", "not_admitted_reason": "capacity_unavailable", "not_admitted_message": message}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobList, []string{"--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if want := `"not_admitted_reason":"capacity_unavailable","not_admitted_message":"` + message + `"`; !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout = %s, want %s in it", stdout.String(), want)
	}
}

func TestJobListWithoutAgentListsWholePlatformExperiment(t *testing.T) {
	var gotQuery string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	clitest.SetAPIURL(t, url)

	var stdout, stderr bytes.Buffer
	code := clitest.Run(JobList, []string{"--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotQuery != "platform_experiment_id=pe-1" {
		t.Fatalf("got query %s", gotQuery)
	}
}

// hl once printed each caller's next step on stderr beside the record, and a reader that parsed
// both streams as one (2>&1) failed on it.
func TestPlatformExperimentsGetWritesTheRecordAloneWithItsReasonFirst(t *testing.T) {
	record := map[string]any{"id": "pe-1", "description": "a long brief", "name": "a run", "starts_at": "2000-01-01T00:00:00Z"}
	requests := 0
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		requests++
		return record
	})
	for _, token := range []string{clitest.DerivedToken("coordinator:pe-1"), clitest.DerivedToken("agent:pe-1/ag-r"), "hlk_person"} {
		for _, status := range []string{"open", "running"} {
			t.Setenv("HL_TOKEN", token)
			record["status"] = status
			delete(record, "status_reason")
			if status == "open" {
				record["status_reason"] = map[string]any{"reason": "baseline_required", "message": "Add a baseline agent"}
			}
			requests = 0
			var out bytes.Buffer
			if code := clitest.Run(PlatformExperimentsGet, []string{"--platform-experiment", "pe-1"}, &out, &out); code != 0 || requests != 1 {
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
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"reason":"`+reason+`","message":"agent jane holds no quota yet: prepare, then end your turn"}`, http.StatusNotFound)
	})
	clitest.SetAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	code := clitest.Run(Quota, []string{"--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 1 || requests != 1 || stdout.Len() != 0 || stderr.String() != "hl: quota: quota_not_granted: agent jane holds no quota yet: prepare, then end your turn\n" {
		t.Fatalf("open: exit %d after %d requests, stdout %q, stderr %q", code, requests, stdout.String(), stderr.String())
	}
	reason = "planner_has_no_quota"
	stderr.Reset()
	if code := clitest.Run(Quota, []string{"--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "hl: quota: planner_has_no_quota: ") {
		t.Fatalf("planner: exit %d, stderr %q, want the reason and the platform's word", code, stderr.String())
	}
	reason = "not_found"
	stderr.Reset()
	if code := clitest.Run(Quota, []string{"--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "HTTP 404") {
		t.Fatalf("running: exit %d, stderr %q, want the 404", code, stderr.String())
	}
}

func TestAWakeKindThePlatformDoesNotKnowIsRefusedBeforeAnyWatch(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return []any{map[string]any{"kind": "agent.finished"}} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/agent-a"))
	var stdout, stderr bytes.Buffer
	code := clitest.Run(Watch, []string{"--wake", "agent.finished,agent.finnished"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"agent.finnished" is no event kind`) || len(*got) != 1 || (*got)[0].Path != "/watch/kinds" {
		t.Fatalf("exit %d, stderr %q, requests %+v", code, stderr.String(), *got)
	}
}

// A coordinator was woken for a baseline job that completed, which it acts on only once the
// agent finishes: a kind wakes for the values its reader can act on.
func TestAWakeEntryNamesAKindOrAKindWithTheValueOrTheSubjectThatWakesOrTheValueThatDoesNot(t *testing.T) {
	type event struct{ subject, value string }
	for entry, wakes := range map[string]map[event]bool{
		"experiment.status":                 {{"e1", "FAILED"}: true, {"e2", "COMPLETED"}: true},
		"experiment.status=FAILED":          {{"e1", "FAILED"}: true, {"e1", "COMPLETED"}: false},
		"job.held!=stage_cut":               {{"e1", "stage_cut"}: false, {"e1", "capacity_unavailable"}: true},
		"agent.instructions@coordinator":    {{"coordinator", "2"}: true, {"ag-7", "2"}: false},
		"agent.instructions@coordinator=2":  {{"coordinator", "2"}: true, {"coordinator", "3"}: false, {"ag-7", "2"}: false},
		"agent.instructions@coordinator!=2": {{"coordinator", "3"}: true, {"coordinator", "2"}: false, {"ag-7", "3"}: false},
	} {
		kind := strings.FieldsFunc(entry, func(r rune) bool { return r == '@' || r == '=' || r == '!' })[0]
		if got := cutWake(entry).kind; got != kind {
			t.Errorf("%s: kind = %q", entry, got)
		}
		for e, want := range wakes {
			if got := cutWake(entry).wakes(kind, e.subject, e.value); got != want {
				t.Errorf("%s: an event of subject %s and value %s wakes = %v, want %v", entry, e.subject, e.value, got, want)
			}
		}
		if cutWake(entry).wakes("other.kind", "coordinator", "2") {
			t.Errorf("%s: wakes for another kind", entry)
		}
	}
}

// A tunnel in front of the platform answered 530 with a whole HTML page, which hl printed into
// the agent's context on every call.
//
// Every such answer then read as an outage. Only a gateway's 502, 503 and 504 and a request that got no answer
// say the platform could not be reached.
//
// That line is the one place an agent reads what an outage asks of it, and it says of the command
// only what is true: a read changed nothing, and a write may have been carried out.
func TestAnAnswerThatIsNotThePlatformsIsOneLineWithoutItsBody(t *testing.T) {
	const next = "A person runs it again later; an agent ends its turn, and an update arrives when the platform answers again"
	const unreachable = "the platform could not be reached, and this command changed nothing. " + next
	const inFront = "the answer is no JSON, so it is none of the platform's API: something in front of the platform answered in its place"
	for status, tc := range map[int]struct{ body, want string }{
		530: {"<!DOCTYPE html>\n<html><head><title>Cloudflare Tunnel error</title></head>\n<body>" + strings.Repeat("<p>x</p>\n", 500) + "</body></html>", inFront},
		500: {"Internal Server Error", inFront},
		502: {"error code: 502", unreachable},
		503: {"", unreachable},
		504: {"<html>gateway timeout</html>", unreachable},
		404: {"404 page not found\n", inFront},
	} {
		clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(tc.body))
		}))
		var stdout, stderr bytes.Buffer
		code := clitest.Run(JobStatus, []string{"job-1"}, &stdout, &stderr)
		want := fmt.Sprintf("hl: GET /experiments/job-1 -> HTTP %d: %s\n", status, tc.want)
		if code != 1 || stdout.Len() != 0 || stderr.String() != want {
			t.Errorf("HTTP %d: exit %d, stdout %q, stderr %q, want %q", status, code, stdout.String(), stderr.String(), want)
		}
	}

	// A 503 of the platform's own is JSON and is printed as it is.
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"reason":"object_store_unavailable","message":"nothing was stored"}`, http.StatusServiceUnavailable)
	}))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobStatus, []string{"job-1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), `"reason":"object_store_unavailable"`) || strings.Contains(stderr.String(), "could not be reached") {
		t.Errorf("the platform's own 503: exit %d, stderr %q, want its reason and no word of an outage", code, stderr.String())
	}

	down := clitest.WithServer(t, func(http.ResponseWriter, *http.Request) {})
	clitest.SetAPIURL(t, strings.Replace(down, "127.0.0.1", "127.0.0.1.invalid", 1))
	stderr.Reset()
	if code := clitest.Run(JobStatus, []string{"job-1"}, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "hl: GET /experiments/job-1: "+unreachable+" (") || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("a request that got no answer: exit %d, stderr %q, want one line saying the platform could not be reached and the cause", code, stderr.String())
	}

	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGatewayTimeout) }))
	stderr.Reset()
	want := "hl: POST /experiments/job-1/cancel -> HTTP 504: the platform could not be reached, and whether this command took effect is not known: read what it changes before running it again. " + next + "\n"
	if code := clitest.Run(JobCancel, []string{"job-1"}, &stdout, &stderr); code != 1 || stderr.String() != want {
		t.Errorf("a write that got no answer: exit %d, stderr %q, want %q", code, stderr.String(), want)
	}
}
