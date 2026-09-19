package main

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestQuota(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		row := map[string]any{"id": "q", "agent_id": "a", "platform_experiment_id": "pe-1",
			"guaranteed_accelerator_hours": 0.8, "used_guaranteed_acch": 0.1, "created_at": "t"}
		if strings.HasSuffix(r.Path, "/quotas") {
			return []any{row, row}
		}
		return row
	})
	t.Setenv("AGENT_ID", "a")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"quota"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if want := `{"agent_id":"a","guaranteed_accelerator_hours":0.8,"used_guaranteed_acch":0.1}` + "\n"; (*got)[0].Path != "/platform-experiments/pe-1/quotas/a" || stdout.String() != want {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"quota", "--all"}, &stdout, &stderr); code != 0 || (*got)[1].Path != "/platform-experiments/pe-1/quotas" || len(lines(stdout.String())) != 2 {
		t.Fatalf("--all: exit %d, request %+v, stdout %q", code, (*got)[1], stdout.String())
	}
}

func TestResultsFlattensStandings(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"status": "running", "metrics": []any{map[string]any{"metric": "acc", "direction": "maximize",
			"standings": []any{map[string]any{"rank": 1, "agent_id": "a", "best": 0.9}, map[string]any{"rank": 2, "agent_id": "b", "best": 0.8}}}}}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"results"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"metric":"acc","direction":"maximize","agent_id":"a","best":0.9,"rank":1}`,
		`{"metric":"acc","direction":"maximize","agent_id":"b","best":0.8,"rank":2}`,
	}
	if (*got)[0].Path != "/platform-experiments/pe-1/results" || !reflect.DeepEqual(lines(stdout.String()), want) {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

func TestCapacityOneLinePerAcceleratorType(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return map[string]any{"clusters": []any{map[string]any{"cluster_name": "local", "accelerators": []any{
			map[string]any{"accelerator_type": "L40", "available": 8, "total": 8},
			map[string]any{"accelerator_type": "H100", "available": 0, "total": 16},
		}}}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"capacity"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"cluster":"local","accelerator_type":"L40","available":8,"total":8}`,
		`{"cluster":"local","accelerator_type":"H100","available":0,"total":16}`,
	}
	if !reflect.DeepEqual(lines(stdout.String()), want) {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestJobStatsAndListDefaultToThePlatformExperiment(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		if r.Path == "/experiments" {
			return []any{}
		}
		return map[string]any{"total": 3, "by_status": map[string]any{"COMPLETED": 3}, "evictions_by_reason": map[string]any{}}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "stats", "--agent", "a"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Path != "/experiments/stats" || (*got)[0].Query != "agent=a&platform_experiment_id=pe-1" || stdout.String() != `{"by_status":{"COMPLETED":3},"total":3}`+"\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
	if code := run([]string{"job", "list", "--status", "RUNNING", "--limit", "50"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[1].Query != "limit=50&platform_experiment_id=pe-1&status=RUNNING" {
		t.Fatalf("job list query = %q", (*got)[1].Query)
	}
}

func TestJobLogsPrintsPlainLines(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return []string{"step 1", "step 2"} })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "logs", "j", "--lines", "5"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Path != "/experiments/j/logs" || (*got)[0].Query != "n=5" || stdout.String() != "step 1\nstep 2\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

func TestDonation(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "ok"} })
	t.Setenv("AGENT_ID", "a")
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	for _, tc := range []struct {
		args []string
		path string
		body map[string]any
	}{
		{[]string{"donation", "request", "--credits", "0.5", "--reason", "one more seed"}, "/donations",
			map[string]any{"agent_id": "a", "platform_experiment_id": "pe-1", "credits_want": 0.5, "reason": "one more seed"}},
		{[]string{"donation", "fulfill", "d-1"}, "/donations/d-1/fulfill", map[string]any{"donor_agent_id": "a"}},
		{[]string{"donation", "cancel", "d-1"}, "/donations/d-1/cancel", nil},
		{[]string{"donation", "list", "--status", "open"}, "/donations", nil},
	} {
		*got = nil
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %s", tc.args, code, stderr.String())
		}
		if (*got)[0].Path != tc.path || (tc.body != nil && !reflect.DeepEqual((*got)[0].Body, tc.body)) {
			t.Fatalf("%v: request %+v", tc.args, (*got)[0])
		}
	}
	if (*got)[0].Query != "platform_experiment_id=pe-1&status=open" {
		t.Fatalf("donation list query = %q", (*got)[0].Query)
	}
}

func TestHypothesisQueryTakesCypherFlag(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"columns": []string{"n"}, "rows": [][]any{{1}}} })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "query", "--platform-experiment", "pe-1", "--cypher", "RETURN 1 AS n"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Body["cypher"] != "RETURN 1 AS n" || stdout.String() != `{"n":1}`+"\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

func TestJobListAndHypothesisGetPrintBriefJobsWithMetricAndTheEnvTheyVaried(t *testing.T) {
	job := func(id string, env map[string]any) map[string]any {
		return map[string]any{"id": id, "agent_id": "a", "hypothesis_id": "hyp-1", "status": "COMPLETED", "hypothesis": "long text",
			"metric": map[string]any{"name": "acc", "best": 0.9, "basis": "raw"}, "phase_detail": map[string]any{}, "theory": "a > b",
			"estimated_cost_acch": 0.5, "created_at": "2026-10-04T00:00:00Z", "resolved_job": map[string]any{"image": "i"},
			"job": map[string]any{"image": "i", "env": env}}
	}
	jobs := []any{job("job-1", map[string]any{"GIT_TOKEN": "secret"}), job("job-2", map[string]any{"LR": "0.3", "GIT_TOKEN": "secret"})}
	recordingServer(t, func(_ int, r recorded) any {
		if r.Path == "/experiments" {
			return jobs
		}
		return map[string]any{"id": "hyp-1", "jobs": jobs, "job_count": 5}
	})
	brief := func(id, env string) string {
		return `{"id":"` + id + `","agent_id":"a","hypothesis_id":"hyp-1","status":"COMPLETED","metric":{"basis":"raw","best":0.9,"name":"acc"},` +
			`"theory":"a > b","estimated_cost_acch":0.5,"created_at":"2026-10-04T00:00:00Z","env":` + env + `}`
	}
	both := []string{brief("job-1", `{}`), brief("job-2", `{"LR":"0.3"}`)}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "list"}, &stdout, &stderr); code != 0 || !reflect.DeepEqual(lines(stdout.String()), both) || stderr.Len() != 0 {
		t.Fatalf("job list: exit %d, stdout %q, stderr %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	want := `{"id":"hyp-1","job_count":5,"jobs":[` + strings.Join(both, ",") + "]}\n"
	if code := run([]string{"hypothesis", "get", "hyp-1"}, &stdout, &stderr); code != 0 || stdout.String() != want {
		t.Fatalf("hypothesis get: exit %d, stdout %q, stderr %s", code, stdout.String(), stderr.String())
	}
	if got := stderr.String(); got != "hl: hypothesis get: 3 more jobs; --limit N (max 200) shows them\n" {
		t.Fatalf("hypothesis get stderr = %q", got)
	}
}

func TestJobListSaysHowManyJobsThePageLeftOut(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "39")
		_, _ = w.Write([]byte(`[{"id":"job-1"},{"id":"job-2"}]`))
	})
	setAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "list", "--offset", "30"}, &stdout, &stderr); code != 0 || len(lines(stdout.String())) != 2 {
		t.Fatalf("exit %d, stdout %q", code, stdout.String())
	}
	if got := stderr.String(); got != "hl: job list: 7 more; --limit N (max 50) and --offset N show them\n" {
		t.Fatalf("stderr = %q", got)
	}
}
