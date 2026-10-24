package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// Every list of hl is one compact JSON object per line, this one included.
func TestPlatformExperimentsListIsOneCompactLineEach(t *testing.T) {
	list := []map[string]string{
		{"id": "pe-1", "status": "closed", "close_reason": "baseline_limit_reached", "description": "a long brief", "summary": "a long summary"},
		{"id": "pe-2", "status": "open"},
	}
	var gotQuery string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})
	clitest.SetAPIURL(t, url)

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

func TestIDIsPositionalOrFlag(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"id": "x", "status": "QUEUED"} })
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

func TestMisuseIsAnsweredWithTheErrorAndTheSynopsis(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return nil })
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

// A refusal of the platform is one line of reason and message, for a cancel and a summary as
// for a submit.
func TestARefusedCancelOrSummaryPrintsItsReasonAndMessage(t *testing.T) {
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
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

func TestJobSummary(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	clitest.SetAPIURL(t, url)

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
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return nil })
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
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		http.Error(w, `{"message":"refused"}`, http.StatusBadRequest)
	})
	clitest.SetAPIURL(t, url)
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/agent-env"))
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

// A coordinator read a baseline job's hypothesis_id, then the platform experiment's id, and ran
// hl hypothesis get on it: 404. Such a job prints that it is on the objective, and no hypothesis.
func TestABaselineJobPrintsAsOnTheObjective(t *testing.T) {
	clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
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
		t.Errorf("an executor's job = %s, want its hypothesis_id and no objective", own)
	}
}

// An agent's --text in backticks ran as a command in its shell. Every flag of free text also
// reads it from a file, or stdin for -, where the shell leaves it alone.
func TestEveryFreeTextFlagReadsItFromAFileOrStdin(t *testing.T) {
	const text = "uses `lr=3e-4` and $(not run)"
	for _, tc := range []struct {
		token string
		args  []string
		flag  string
		sent  func(map[string]any) any
	}{
		{"agent:pe-1/ag-1", []string{"hypothesis", "submit", "--builds-on", "root"}, "text", clitest.Field("text")},
		{"agent:pe-1/ag-1", []string{"hypothesis", "comment", "root"}, "text", clitest.Field("text")},
		{"agent:pe-1/ag-1", []string{"hypothesis", "status", "hyp-1", "confirmed"}, "despite", clitest.Field("despite")},
		{"agent:pe-1/ag-1", []string{"job", "summary", "job-1"}, "text", clitest.Field("summary")},
		{"agent:pe-1/ag-1", []string{"job", "submit", clitest.WriteJobFile(t)}, "theory", func(body map[string]any) any {
			metadata, _ := body["metadata"].(map[string]any)
			return metadata["theory"]
		}},
		{"agent:pe-1/ag-1", []string{"platform-experiments", "agents", "finish"}, "reason", clitest.Field("reason")},
		{"coordinator:pe-1", []string{"platform-experiments", "agents", "message", "ag-1"}, "text", clitest.Field("text")},
		{"coordinator:pe-1", []string{"platform-experiments", "summary"}, "text", clitest.Field("summary")},
		{"coordinator:pe-1", []string{"platform-experiments", "close"}, "reason", clitest.Field("reason")},
		{"coordinator:pe-1", []string{"platform-experiments", "update"}, "description", clitest.Field("description")},
		{"coordinator:pe-1", []string{"platform-experiments", "update", "--baseline-job", "job-2"}, "reason", clitest.Field("baseline_reason")},
		{"agent:pe-1/ag-1", []string{"platform-experiments", "decline", "--goal-version", "1"}, "reason", clitest.Field("reason")},
	} {
		got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
			return map[string]any{"id": "hyp-1", "status": "QUEUED", "messages": []any{}}
		})
		t.Setenv("HL_TOKEN", clitest.DerivedToken(tc.token))
		name := strings.Join(tc.args, " ") + " --" + tc.flag
		file := clitest.WriteFile(t, text+"\n\n")
		for how, args := range map[string][]string{
			"inline": {"--" + tc.flag, text},
			"file":   {"--" + tc.flag + "-file", file},
			"stdin":  {"--" + tc.flag + "-file", "-"},
		} {
			if how == "stdin" {
				clitest.Stdin(t, text+"\n")
			}
			*got = nil
			code, _, stderr := hl(t, append(append([]string{}, tc.args...), args...)...)
			if code != 0 || len(*got) == 0 {
				t.Errorf("%s from %s: exit %d with %d requests, stderr %s", name, how, code, len(*got), stderr)
				continue
			}
			if sent := tc.sent((*got)[len(*got)-1].Body); sent != text {
				t.Errorf("%s from %s: sent %q, want %q", name, how, sent, text)
			}
		}
		*got = nil
		code, _, stderr := hl(t, append(append([]string{}, tc.args...), "--"+tc.flag, "t", "--"+tc.flag+"-file", file)...)
		if code != 2 || len(*got) != 0 || !strings.Contains(stderr, "two ways to say one thing") {
			t.Errorf("%s with both forms: exit %d with %d requests, stderr %q, want 2, none sent and why", name, code, len(*got), stderr)
		}
	}
}

func TestTextFromStdinIsReadOnce(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/ag-1"))
	for _, args := range [][]string{
		{"job", "submit", "--theory-file", "-"},
		{"hypothesis", "submit", "--text-file", "-", "--file", "-"},
	} {
		clitest.Stdin(t, "text\n")
		code, _, stderr := hl(t, args...)
		if code != 2 || !strings.Contains(stderr, "both read stdin") {
			t.Errorf("%v: exit %d, stderr %q, want 2 and that only one reads stdin", args, code, stderr)
		}
	}
	empty := clitest.WriteFile(t, "\n")
	if code, _, stderr := hl(t, "hypothesis", "comment", "root", "--text-file", empty); code != 2 || !strings.Contains(stderr, "holds no text") {
		t.Errorf("an empty file: exit %d, stderr %q, want 2 and why", code, stderr)
	}
	if len(*got) != 0 {
		t.Errorf("a refused command was sent: %+v", *got)
	}
}
